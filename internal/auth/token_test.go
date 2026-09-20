package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testKid      = "test-key-1"
	testAudience = "authenticated"
	testSubject  = "6f1d2c3b-0000-4000-8000-000000000001"
)

// testProject spins up a stand-in for a Supabase project's JWKS endpoint and
// returns a verifier pointed at it, plus the private key to sign with.
func testProject(t *testing.T) (*Verifier, *ecdsa.PrivateKey, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/v1/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "EC",
				"crv": "P-256",
				"kid": testKid,
				"use": "sig",
				"alg": "ES256",
				"x":   b64(key.PublicKey.X.Bytes()),
				"y":   b64(key.PublicKey.Y.Bytes()),
			}},
		})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return NewVerifier(server.URL, testAudience), key, server.URL
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signedToken builds a token with the given claims, signed ES256 with kid.
func signedToken(t *testing.T, key *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = kid
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return raw
}

func validClaims(issuer string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":   issuer + "/auth/v1",
		"sub":   testSubject,
		"aud":   testAudience,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"email": "abebe@example.com",
		"user_metadata": map[string]any{
			"full_name":  "Abebe Bekele",
			"avatar_url": "https://example.com/a.png",
		},
	}
}

func TestVerifyAcceptsAValidToken(t *testing.T) {
	verifier, key, issuer := testProject(t)

	raw := signedToken(t, key, testKid, validClaims(issuer))

	user, err := verifier.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify() = %v, want success", err)
	}
	if user.ID != testSubject {
		t.Errorf("ID = %q, want %q", user.ID, testSubject)
	}
	if user.Email != "abebe@example.com" {
		t.Errorf("Email = %q", user.Email)
	}
	if user.DisplayName != "Abebe Bekele" {
		t.Errorf("DisplayName = %q, want it read from user_metadata", user.DisplayName)
	}
}

// Every case here is a token a caller would like accepted.
func TestVerifyRejects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		claim func(issuer string) jwt.MapClaims
		kid   string
	}{
		{
			name: "expired",
			claim: func(issuer string) jwt.MapClaims {
				c := validClaims(issuer)
				c["exp"] = time.Now().Add(-time.Minute).Unix()
				return c
			},
		},
		{
			// Any Supabase project can mint a well-formed token. Only ours counts.
			name: "issued by another project",
			claim: func(issuer string) jwt.MapClaims {
				c := validClaims(issuer)
				c["iss"] = "https://someone-else.supabase.co/auth/v1"
				return c
			},
		},
		{
			// Supabase's anon key is itself a JWT with aud "anon", and it ships
			// inside the frontend bundle. Accepting it would authenticate every
			// visitor as a user.
			name: "anon audience",
			claim: func(issuer string) jwt.MapClaims {
				c := validClaims(issuer)
				c["aud"] = "anon"
				return c
			},
		},
		{
			name: "no subject",
			claim: func(issuer string) jwt.MapClaims {
				c := validClaims(issuer)
				delete(c, "sub")
				return c
			},
		},
		{
			name: "no expiry",
			claim: func(issuer string) jwt.MapClaims {
				c := validClaims(issuer)
				delete(c, "exp")
				return c
			},
		},
		{
			name:  "unknown key id",
			claim: validClaims,
			kid:   "a-key-that-was-never-published",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier, key, issuer := testProject(t)

			kid := tc.kid
			if kid == "" {
				kid = testKid
			}
			raw := signedToken(t, key, kid, tc.claim(issuer))

			if _, err := verifier.Verify(context.Background(), raw); err == nil {
				t.Fatal("Verify() accepted a token it must reject")
			} else if !errors.Is(err, ErrUnauthenticated) {
				t.Errorf("error = %v, want it to wrap ErrUnauthenticated so the "+
					"HTTP layer maps it to 401", err)
			}
		})
	}
}

// The classic algorithm-confusion attack: take the *public* key, which is
// published to the world, and use it as an HMAC secret. A verifier that reads
// the algorithm out of the token header instead of pinning it will happily
// confirm the signature, and anyone can then mint a token for any user.
func TestVerifyRejectsHMACSignedWithThePublicKey(t *testing.T) {
	verifier, key, issuer := testProject(t)

	public := elliptic.Marshal(key.PublicKey.Curve, key.PublicKey.X, key.PublicKey.Y) //nolint:staticcheck // the attack uses the classic encoding
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, validClaims(issuer))
	token.Header["kid"] = testKid
	raw, err := token.SignedString(public)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatal("Verify() accepted an HS256 token signed with the public key")
	}
}

// `alg: none` is the other half of the same family — a token with no signature
// at all, which some parsers treat as trivially valid.
func TestVerifyRejectsUnsignedToken(t *testing.T) {
	verifier, _, issuer := testProject(t)

	token := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims(issuer))
	token.Header["kid"] = testKid
	raw, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := verifier.Verify(context.Background(), raw); err == nil {
		t.Fatal("Verify() accepted a token with alg=none")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	verifier, _, _ := testProject(t)

	for _, raw := range []string{"", "not-a-jwt", "a.b.c"} {
		if _, err := verifier.Verify(context.Background(), raw); err == nil {
			t.Errorf("Verify(%q) accepted a malformed token", raw)
		}
	}
}

// An unknown key id is what a rotation looks like, so it triggers a refetch —
// but a caller can generate unknown key ids for free, and each one must not
// become a request to Supabase.
func TestKeySetThrottlesRefetches(t *testing.T) {
	var fetches int
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/v1/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "EC", "crv": "P-256", "kid": testKid,
				"x": b64(key.PublicKey.X.Bytes()), "y": b64(key.PublicKey.Y.Bytes()),
			}},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	set := NewKeySet(server.URL)
	for range 20 {
		_, _ = set.Key(context.Background(), "unknown-kid")
	}

	if fetches > 1 {
		t.Errorf("fetched the key set %d times for 20 unknown key ids; "+
			"an attacker sets the rate of that request", fetches)
	}
}
