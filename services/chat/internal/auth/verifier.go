package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ErrInvalidToken is the only failure a caller ever sees.
//
// Missing, malformed, expired, wrong signature, wrong type — one error for all
// of them. Telling a client that its token was well-formed but expired, as
// opposed to not a token at all, is help an attacker probing the endpoint can
// use and a legitimate client never needs: it refreshes on any 401 either way.
// The specific reason is logged, not returned.
var ErrInvalidToken = errors.New("invalid access token")

// tokenTypeAccess is the `typ` claim identity stamps on access tokens. A
// refresh token is opaque and could never be presented here, but a token minted
// for some other purpose must not authenticate a request either.
const tokenTypeAccess = "access"

// Claims is what this service needs out of a verified token. Everything else a
// handler might want about the user is a lookup, not a claim.
type Claims struct {
	UserID uuid.UUID
	Email  string
	JTI    uuid.UUID
}

// Verifier turns a bearer token string into Claims.
type Verifier struct {
	keys   *KeySet
	parser *jwt.Parser
}

// NewVerifier builds a verifier over a key set.
func NewVerifier(keys *KeySet) *Verifier {
	return &Verifier{
		keys: keys,
		parser: jwt.NewParser(
			// RS256 is pinned here and never read from the token's own header.
			// Taking the header's word for `alg` is how a service ends up
			// verifying an `alg: none` token, or an HS256 token signed with
			// the public key it published itself.
			jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
			jwt.WithExpirationRequired(),
			jwt.WithIssuedAt(),
			// Identity mints tokens with no `iss` or `aud` claim (CLAUDE.md
			// lists sub, email, iat, exp, jti, typ), so there is nothing to
			// check them against. The signature is the audience check: only
			// identity holds the private half.
		),
	}
}

// Verify parses and validates a token, returning its claims.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	var claims jwt.MapClaims

	parsed, err := v.parser.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token header carries no kid")
		}
		return v.keys.Key(ctx, kid)
	})
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if !parsed.Valid {
		return Claims{}, ErrInvalidToken
	}

	if typ, _ := claims["typ"].(string); typ != tokenTypeAccess {
		return Claims{}, fmt.Errorf("%w: typ is not %q", ErrInvalidToken, tokenTypeAccess)
	}

	subject, _ := claims["sub"].(string)
	userID, err := uuid.Parse(subject)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: sub is not a uuid", ErrInvalidToken)
	}

	jtiRaw, _ := claims["jti"].(string)
	jti, err := uuid.Parse(jtiRaw)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: jti is not a uuid", ErrInvalidToken)
	}

	email, _ := claims["email"].(string)

	return Claims{UserID: userID, Email: email, JTI: jti}, nil
}

// Warm fetches the key set once. Called from readiness checks, where "can this
// instance verify a token" is part of "can it serve traffic".
func (v *Verifier) Warm(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return v.keys.Refresh(ctx)
}
