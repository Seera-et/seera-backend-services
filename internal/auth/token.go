// Package auth verifies the user tokens Supabase issues and puts the
// authenticated user on the request context.
//
// It owns no credentials of its own: Supabase holds the Google client secret and
// the signing key, and this package only ever sees the public half. There is no
// password, no session table and no logout endpoint to build — signing out is a
// client-side discard of a token that expires on its own.
package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// ErrUnauthenticated is the single error the HTTP layer maps to 401. The reason
// a token failed is logged, never returned: "expired" and "bad signature" tell
// an attacker which half of the guess was right.
var ErrUnauthenticated = errors.New("unauthenticated")

// User is an authenticated caller, as the token describes them.
//
// This is the whole identity the API works with. Anything richer — a plan, a
// quota, a preference — belongs in app_users, keyed by ID, not in a claim a
// client could influence.
type User struct {
	// ID is the Supabase user id (the `sub` claim), and the primary key of
	// app_users. Stable across sign-ins and across providers.
	ID string
	// Email as verified by Google.
	Email string
	// DisplayName and AvatarURL come from user_metadata, which Supabase fills
	// from the Google profile. Both are cosmetic and may be empty.
	DisplayName string
	AvatarURL   string
}

// claims is the subset of a Supabase token this service reads.
type claims struct {
	jwt.RegisteredClaims
	Email        string `json:"email"`
	UserMetadata struct {
		FullName  string `json:"full_name"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
		Picture   string `json:"picture"`
	} `json:"user_metadata"`
}

// Verifier turns a bearer token into a User, or refuses.
type Verifier struct {
	keys     *KeySet
	issuer   string
	audience string
}

// NewVerifier builds a verifier for a Supabase project origin, e.g.
// https://<ref>.supabase.co.
func NewVerifier(projectURL, audience string) *Verifier {
	return &Verifier{
		keys: NewKeySet(projectURL),
		// Supabase issues tokens under the /auth/v1 path, not the bare origin.
		issuer:   projectURL + "/auth/v1",
		audience: audience,
	}
}

// Verify parses and validates a raw JWT.
//
// Every check here is one an attacker would like skipped:
//
//   - WithValidMethods pins the accepted algorithms. Without it a token with
//     `alg: none` parses as valid, and a token signed with HMAC using the
//     *public* key as the secret verifies — the classic algorithm-confusion
//     attack, and the reason the method is pinned rather than read from the
//     header.
//   - The issuer must be this project. Any Supabase project can mint a
//     well-formed token; only ours should be trusted.
//   - The audience must be "authenticated". Supabase's anon key is itself a JWT
//     with `aud: anon`, and it is published in the frontend bundle. Skipping
//     this check would let every visitor authenticate as a user.
//
// Expiry and not-before are enforced by the parser.
func (v *Verifier) Verify(ctx context.Context, raw string) (User, error) {
	keyFunc := func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token has no key id")
		}
		return v.keys.Key(ctx, kid)
	}

	var c claims
	_, err := jwt.ParseWithClaims(raw, &c, keyFunc,
		jwt.WithValidMethods([]string{"ES256", "RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return User{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}

	if c.Subject == "" {
		return User{}, fmt.Errorf("%w: token has no subject", ErrUnauthenticated)
	}

	return User{
		ID:          c.Subject,
		Email:       c.Email,
		DisplayName: firstNonEmpty(c.UserMetadata.FullName, c.UserMetadata.Name),
		AvatarURL:   firstNonEmpty(c.UserMetadata.AvatarURL, c.UserMetadata.Picture),
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
