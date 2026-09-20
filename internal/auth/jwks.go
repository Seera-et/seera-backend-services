package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// KeySet holds the public keys a Supabase project signs user tokens with.
//
// Supabase signs with ES256 and publishes the public half at
// /auth/v1/.well-known/jwks.json, so the API can verify a token on its own. That
// matters for more than tidiness: asking Supabase to validate every request
// would put a third party in the latency path of every answer, and would mean a
// Supabase outage logs out every user mid-conversation.
//
// Keys are cached indefinitely and refetched only when a token arrives bearing a
// key id we have never seen, which is exactly what a rotation looks like from
// here.
type KeySet struct {
	url    string
	client *http.Client

	mu        sync.RWMutex
	keys      map[string]crypto.PublicKey
	lastFetch time.Time
}

// minRefetchInterval floors how often an unknown key id can trigger a fetch.
//
// Without it, a stream of tokens carrying garbage key ids — which costs an
// attacker nothing to generate — becomes a request flood aimed at Supabase with
// our credentials on it. One fetch per interval is enough to pick up a real
// rotation promptly.
const minRefetchInterval = time.Minute

// NewKeySet builds a key set for a Supabase project origin, e.g.
// https://<ref>.supabase.co. It does not fetch: the first verification does,
// so a Supabase blip at boot does not stop the process from starting.
func NewKeySet(projectURL string) *KeySet {
	return &KeySet{
		url: projectURL + "/auth/v1/.well-known/jwks.json",
		// Short timeout: this call sits in front of a user request, and a
		// hanging key fetch would hold the request open for the default
		// client's lifetime, which is forever.
		client: &http.Client{Timeout: 5 * time.Second},
		keys:   make(map[string]crypto.PublicKey),
	}
}

// Key returns the public key for a key id, fetching the set if it is unknown.
func (k *KeySet) Key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	k.mu.RLock()
	key, ok := k.keys[kid]
	stale := time.Since(k.lastFetch) >= minRefetchInterval
	k.mu.RUnlock()

	if ok {
		return key, nil
	}
	if !stale {
		// Recently fetched and this id was not in the answer, so refetching
		// would return the same thing. Treat it as what it is.
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}

	if err := k.refresh(ctx); err != nil {
		return nil, err
	}

	k.mu.RLock()
	defer k.mu.RUnlock()
	if key, ok := k.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

func (k *KeySet) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return fmt.Errorf("build jwks request: %w", err)
	}

	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: unexpected status %d", resp.StatusCode)
	}

	var document struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&document); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}

	parsed := make(map[string]crypto.PublicKey, len(document.Keys))
	for _, key := range document.Keys {
		// A set may legitimately carry key types we do not accept. Skipping
		// them beats failing the whole refresh and locking every user out.
		public, err := key.publicKey()
		if err != nil || key.Kid == "" {
			continue
		}
		parsed[key.Kid] = public
	}
	if len(parsed) == 0 {
		return fmt.Errorf("jwks at %s contained no usable keys", k.url)
	}

	k.mu.Lock()
	k.keys = parsed
	k.lastFetch = time.Now()
	k.mu.Unlock()
	return nil
}

// jwk is the subset of RFC 7517 this needs.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// publicKey converts a JWK to a key crypto can verify with.
//
// EC is what Supabase issues today; RSA is accepted too because a project can
// be switched to RS256 in the dashboard, and discovering that through a total
// sign-in outage would be an avoidable way to find out.
func (j jwk) publicKey() (crypto.PublicKey, error) {
	switch j.Kty {
	case "EC":
		if j.Crv != "P-256" {
			return nil, fmt.Errorf("unsupported EC curve %q", j.Crv)
		}
		x, err := decodeBigInt(j.X)
		if err != nil {
			return nil, fmt.Errorf("ec x: %w", err)
		}
		y, err := decodeBigInt(j.Y)
		if err != nil {
			return nil, fmt.Errorf("ec y: %w", err)
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil

	case "RSA":
		n, err := decodeBigInt(j.N)
		if err != nil {
			return nil, fmt.Errorf("rsa n: %w", err)
		}
		e, err := decodeBigInt(j.E)
		if err != nil {
			return nil, fmt.Errorf("rsa e: %w", err)
		}
		if !e.IsInt64() || e.Int64() > 1<<31-1 || e.Sign() <= 0 {
			return nil, fmt.Errorf("rsa exponent out of range")
		}
		return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil

	default:
		return nil, fmt.Errorf("unsupported key type %q", j.Kty)
	}
}

func decodeBigInt(raw string) (*big.Int, error) {
	// JWK numbers are base64url without padding.
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("empty value")
	}
	return new(big.Int).SetBytes(b), nil
}
