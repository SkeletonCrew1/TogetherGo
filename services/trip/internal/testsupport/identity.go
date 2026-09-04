package testsupport

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Identity is a stand-in for the identity service: an RS256 key, a JWKS
// endpoint serving its public half, and a way to mint access tokens shaped
// exactly like the real ones.
//
// A stub rather than the real service because what is under test here is that
// this service verifies a signature against a JWKS — not that identity can
// produce one, which identity's own suite covers.
type Identity struct {
	KeyID   string
	JWKSURL string

	key    *rsa.PrivateKey
	server *httptest.Server
}

// NewIdentity starts the stub. It is torn down when the test ends.
func NewIdentity(t *testing.T) *Identity {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	stub := &Identity{KeyID: "test-key-1", key: key}

	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pub := key.Public().(*rsa.PublicKey)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"keys": []any{map[string]string{
				"kty": "RSA",
				"use": "sig",
				"alg": "RS256",
				"kid": stub.KeyID,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
			}},
		}))
	}))
	t.Cleanup(stub.server.Close)

	stub.JWKSURL = stub.server.URL + "/.well-known/jwks.json"
	return stub
}

// Token mints a 15-minute access token for a user, with the six claims
// CLAUDE.md specifies.
func (i *Identity) Token(t *testing.T, userID uuid.UUID) string {
	t.Helper()

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   userID.String(),
		"email": userID.String() + "@example.test",
		"iat":   now.Unix(),
		"exp":   now.Add(15 * time.Minute).Unix(),
		"jti":   uuid.NewString(),
		"typ":   "access",
	})
	token.Header["kid"] = i.KeyID

	signed, err := token.SignedString(i.key)
	require.NoError(t, err)
	return signed
}
