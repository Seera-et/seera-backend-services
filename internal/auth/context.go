package auth

import "context"

// contextKey is unexported so no other package can write a user onto a request
// context. Only this package's middleware can say who the caller is.
type contextKey struct{}

// WithUser returns a context carrying the authenticated user.
func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, contextKey{}, user)
}

// UserFrom reads the authenticated user. ok is false on an anonymous request,
// which is a normal state on the public endpoints rather than an error.
func UserFrom(ctx context.Context) (User, bool) {
	user, ok := ctx.Value(contextKey{}).(User)
	return user, ok
}
