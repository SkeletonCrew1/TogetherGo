package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/auth"
)

// jwksServer stands in for the identity service: it holds a set of RSA keys and
// serves them as a JWKS document, counting how often it is asked.
type jwksServer struct {
	*httptest.Server

	mu    sync.Mutex
	keys  map[string]*rsa.PrivateKey
	hits  atomic.Int64
	fail  atomic.Bool
	empty atomic.Bool
}

func newJWKSServer(t *testing.T, kids ...string) *jwksServer {
	t.Helper()

	s := &jwksServer{keys: map[string]*rsa.PrivateKey{}}
	for _, kid := range kids {
		s.addKey(t, kid)
	}

	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.hits.Add(1)
		if s.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		document := map[string]any{"keys": []any{}}
		if !s.empty.Load() {
			keys := make([]any, 0, len(s.keys))
			for kid, key := range s.keys {
				pub := key.Public().(*rsa.PublicKey)
				keys = append(keys, map[string]string{
					"kty": "RSA",
					"use": "sig",
					"alg": "RS256",
					"kid": kid,
					"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
					"e":   base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
				})
			}
			document["keys"] = keys
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(document))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) addKey(t *testing.T, kid string) {
	t.Helper()
	// 2048 bits because the JWKS parser rejects anything smaller — see
	// parseRSAPublicKey. Generating one per test is the slow part of this file
	// and still measured in tens of milliseconds.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[kid] = key
}

// sign mints an access token shaped exactly like identity's: RS256, a kid in
// the header, and the six claims CLAUDE.md lists.
func (s *jwksServer) sign(t *testing.T, kid string, mutate func(jwt.MapClaims)) string {
	t.Helper()

	s.mu.Lock()
	key, ok := s.keys[kid]
	s.mu.Unlock()
	require.True(t, ok, "no signing key %q", kid)

	now := time.Now()
	claims := jwt.MapClaims{
		"sub":   uuid.NewString(),
		"email": "traveller@example.com",
		"iat":   now.Unix(),
		"exp":   now.Add(15 * time.Minute).Unix(),
		"jti":   uuid.NewString(),
		"typ":   "access",
	}
	if mutate != nil {
		mutate(claims)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid

	signed, err := token.SignedString(key)
	require.NoError(t, err)
	return signed
}

func TestVerifyAcceptsAnIdentityToken(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	verifier := auth.NewVerifier(auth.NewKeySet(server.URL, time.Minute, nil))

	subject := uuid.New()
	token := server.sign(t, "dev-key-1", func(c jwt.MapClaims) { c["sub"] = subject.String() })

	claims, err := verifier.Verify(context.Background(), token)
	require.NoError(t, err)
	require.Equal(t, subject, claims.UserID)
	require.Equal(t, "traveller@example.com", claims.Email)
	require.NotEqual(t, uuid.Nil, claims.JTI)
}

// TestKeySetCachesForItsTTL: the document is fetched once and reused, which is
// the whole point of the ten-minute cache.
func TestKeySetCachesForItsTTL(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	verifier := auth.NewVerifier(auth.NewKeySet(server.URL, time.Minute, nil))

	for i := 0; i < 5; i++ {
		_, err := verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
		require.NoError(t, err)
	}

	require.EqualValues(t, 1, server.hits.Load(), "a fresh cache must not refetch")
}

func TestKeySetRefetchesAfterItsTTL(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	// A TTL short enough to expire between two calls without a sleep worth
	// mentioning.
	verifier := auth.NewVerifier(auth.NewKeySet(server.URL, 20*time.Millisecond, nil))

	_, err := verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
	require.NoError(t, err)

	time.Sleep(40 * time.Millisecond)

	_, err = verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
	require.NoError(t, err)

	require.EqualValues(t, 2, server.hits.Load(), "an expired cache must refetch")
}

// TestUnknownKidTriggersARefresh is the key-rotation path: identity starts
// signing with a new kid, and this service picks it up on the first token that
// carries it rather than rejecting valid tokens until its TTL happens to lapse.
func TestUnknownKidTriggersARefresh(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	verifier := auth.NewVerifier(auth.NewKeySet(server.URL, time.Hour, nil))

	_, err := verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
	require.NoError(t, err)
	require.EqualValues(t, 1, server.hits.Load())

	// Identity rotates. The cache is still an hour from expiring.
	server.addKey(t, "dev-key-2")

	claims, err := verifier.Verify(context.Background(), server.sign(t, "dev-key-2", nil))
	require.NoError(t, err, "an unknown kid must trigger a refetch, not a rejection")
	require.NotEqual(t, uuid.Nil, claims.UserID)
	require.EqualValues(t, 2, server.hits.Load())

	// And the old key still works: the refresh replaced the set, it did not
	// invalidate the tokens already in flight.
	_, err = verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
	require.NoError(t, err)
}

// TestUnknownKidRefreshIsRateLimited: a forged `kid` is the cheapest attack
// there is — the header is unauthenticated — and one upstream fetch per forged
// token would turn this service into a load generator pointed at identity.
func TestUnknownKidRefreshIsRateLimited(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	other := newJWKSServer(t, "forged-key")
	verifier := auth.NewVerifier(auth.NewKeySet(server.URL, time.Hour, nil))

	_, err := verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
	require.NoError(t, err)
	require.EqualValues(t, 1, server.hits.Load())

	for i := 0; i < 25; i++ {
		_, err := verifier.Verify(context.Background(), other.sign(t, "forged-key", nil))
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	}

	require.EqualValues(t, 2, server.hits.Load(),
		"the first unknown kid refetches; the rest are answered from that result")
}

// TestStaleCacheSurvivesAnUpstreamOutage: identity being briefly unreachable
// must not log every user out of the trip service.
func TestStaleCacheSurvivesAnUpstreamOutage(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	verifier := auth.NewVerifier(auth.NewKeySet(server.URL, 20*time.Millisecond, nil))

	_, err := verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
	require.NoError(t, err)

	server.fail.Store(true)
	time.Sleep(40 * time.Millisecond)

	_, err = verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
	require.NoError(t, err, "a known kid must still verify from a stale cache")
}

func TestVerifyRejects(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	foreign := newJWKSServer(t, "dev-key-1")
	verifier := auth.NewVerifier(auth.NewKeySet(server.URL, time.Hour, nil))

	// Prime the cache so the failures below are about the token, not about a
	// cold key set.
	_, err := verifier.Verify(context.Background(), server.sign(t, "dev-key-1", nil))
	require.NoError(t, err)

	t.Run("an expired token", func(t *testing.T) {
		token := server.sign(t, "dev-key-1", func(c jwt.MapClaims) {
			c["iat"] = time.Now().Add(-time.Hour).Unix()
			c["exp"] = time.Now().Add(-time.Minute).Unix()
		})
		_, err := verifier.Verify(context.Background(), token)
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("a token with no exp", func(t *testing.T) {
		token := server.sign(t, "dev-key-1", func(c jwt.MapClaims) { delete(c, "exp") })
		_, err := verifier.Verify(context.Background(), token)
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("a refresh-typed token", func(t *testing.T) {
		// Refresh tokens are opaque and could never arrive here, but a token
		// minted for another purpose must not authenticate a request either.
		token := server.sign(t, "dev-key-1", func(c jwt.MapClaims) { c["typ"] = "refresh" })
		_, err := verifier.Verify(context.Background(), token)
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("a token with no typ", func(t *testing.T) {
		token := server.sign(t, "dev-key-1", func(c jwt.MapClaims) { delete(c, "typ") })
		_, err := verifier.Verify(context.Background(), token)
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("a sub that is not a uuid", func(t *testing.T) {
		token := server.sign(t, "dev-key-1", func(c jwt.MapClaims) { c["sub"] = "not-a-uuid" })
		_, err := verifier.Verify(context.Background(), token)
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("a missing jti", func(t *testing.T) {
		token := server.sign(t, "dev-key-1", func(c jwt.MapClaims) { delete(c, "jti") })
		_, err := verifier.Verify(context.Background(), token)
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("a token signed by someone else's key with our kid", func(t *testing.T) {
		_, err := verifier.Verify(context.Background(), foreign.sign(t, "dev-key-1", nil))
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("an unsigned token", func(t *testing.T) {
		// alg: none. Accepting the header's word for `alg` is how a service
		// ends up verifying one of these; the parser pins RS256 instead.
		claims := jwt.MapClaims{"sub": uuid.NewString(), "typ": "access", "jti": uuid.NewString()}
		token := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
		token.Header["kid"] = "dev-key-1"
		signed, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
		require.NoError(t, err)

		_, err = verifier.Verify(context.Background(), signed)
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("a token with no kid at all", func(t *testing.T) {
		claims := jwt.MapClaims{
			"sub": uuid.NewString(), "typ": "access", "jti": uuid.NewString(),
			"exp": time.Now().Add(time.Minute).Unix(),
		}
		signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(server.keys["dev-key-1"])
		require.NoError(t, err)

		_, err = verifier.Verify(context.Background(), signed)
		require.ErrorIs(t, err, auth.ErrInvalidToken)
	})

	t.Run("garbage", func(t *testing.T) {
		for _, token := range []string{"", "not.a.token", "Bearer", "a.b.c"} {
			_, err := verifier.Verify(context.Background(), token)
			require.ErrorIs(t, err, auth.ErrInvalidToken)
		}
	})
}

func TestKeySetRejectsAnEmptyDocument(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	server.empty.Store(true)

	keys := auth.NewKeySet(server.URL, time.Minute, nil)

	err := keys.Refresh(context.Background())
	require.Error(t, err, "a JWKS with no usable keys is a broken upstream, not an empty cache")
	require.Contains(t, err.Error(), "no usable RS256 keys")
}

func TestKeySetReportsAnUnreachableUpstream(t *testing.T) {
	keys := auth.NewKeySet("http://127.0.0.1:1/.well-known/jwks.json", time.Minute,
		&http.Client{Timeout: 200 * time.Millisecond})

	require.Error(t, keys.Refresh(context.Background()))
}

// TestConcurrentVerifiesShareOneFetch: the mutex is held across the HTTP call so
// a burst of requests arriving on a cold cache makes one upstream request, not
// one each.
func TestConcurrentVerifiesShareOneFetch(t *testing.T) {
	server := newJWKSServer(t, "dev-key-1")
	verifier := auth.NewVerifier(auth.NewKeySet(server.URL, time.Hour, nil))

	tokens := make([]string, 20)
	for i := range tokens {
		tokens[i] = server.sign(t, "dev-key-1", nil)
	}

	var wg sync.WaitGroup
	for _, token := range tokens {
		wg.Add(1)
		go func(token string) {
			defer wg.Done()
			_, err := verifier.Verify(context.Background(), token)
			require.NoError(t, err)
		}(token)
	}
	wg.Wait()

	require.EqualValues(t, 1, server.hits.Load())
}
