package auth

import (
	"context"

	"github.com/google/uuid"
)

// contextKey is unexported so no other package can write the caller's identity
// into a request context. Only the authentication middleware puts it there.
type contextKey struct{}

var claimsKey contextKey

// WithClaims returns a context carrying the verified caller.
func WithClaims(ctx context.Context, claims Claims) context.Context {
	return context.WithValue(ctx, claimsKey, claims)
}

// ClaimsFrom returns the verified caller, and false on an unauthenticated
// context. Handlers behind the authentication middleware can rely on the true
// branch; the check exists so that mounting one outside it fails visibly.
func ClaimsFrom(ctx context.Context) (Claims, bool) {
	claims, ok := ctx.Value(claimsKey).(Claims)
	return claims, ok
}

// UserIDFrom returns just the caller's id, or the nil UUID.
func UserIDFrom(ctx context.Context) uuid.UUID {
	claims, ok := ClaimsFrom(ctx)
	if !ok {
		return uuid.Nil
	}
	return claims.UserID
}
