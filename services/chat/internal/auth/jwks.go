// Package auth verifies access tokens issued by the identity service.
//
// Architecture rule 6: this service validates JWTs itself, against identity's
// JWKS. It does not trust a gateway-injected identity header, because in
// Compose every container shares a network with every other and anything on it
// could set that header.
package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// DefaultCacheTTL is how long a fetched key set is served without refetching.
// Matches the Cache-Control identity puts on the document.
const DefaultCacheTTL = 10 * time.Minute

// minRefreshInterval bounds the unknown-kid refresh path.
//
// Without it, a flood of tokens carrying a bogus `kid` — which is the cheapest
// forgery there is, since the header is unauthenticated — would turn into one
// upstream request per token and make this service a load generator pointed at
// identity. With it, the first such token refetches and the rest are answered
// from what that refetch found, for the next ten seconds.
const minRefreshInterval = 10 * time.Second

// jwk is the subset of RFC 7517 identity actually publishes: RSA public keys,
// modulus and exponent base64url-encoded without padding.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// KeySet is a cached, concurrently-usable view of identity's JWKS.
type KeySet struct {
	url        string
	ttl        time.Duration
	httpClient *http.Client

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	// lastMiss is when an *unknown kid* last provoked a refetch. Tracked apart
	// from fetchedAt so an ordinary TTL refresh does not start a cooldown: a
	// key rotated seconds after a scheduled refetch must still be picked up on
	// the first token that carries it.
	lastMiss time.Time
}

// NewKeySet builds a key set that fetches from url and caches for ttl.
//
// Nothing is fetched here: a service that cannot start because identity is not
// up yet would make boot order significant, and the first token to arrive can
// perfectly well pay for the first fetch.
func NewKeySet(url string, ttl time.Duration, client *http.Client) *KeySet {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &KeySet{url: url, ttl: ttl, httpClient: client, keys: map[string]*rsa.PublicKey{}}
}

// Key returns the public key for kid.
//
// Three cases, in order:
//
//  1. the cache is fresh and holds the kid — return it, no I/O;
//  2. the cache is stale — refetch, then look again;
//  3. the cache is fresh but does not hold the kid — refetch anyway, rate
//     limited. This is the key-rotation path: identity starts signing with a
//     new kid and every verifier finds out within one request instead of
//     rejecting valid tokens until its TTL happens to expire.
func (k *KeySet) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if time.Since(k.fetchedAt) < k.ttl {
		if key, ok := k.keys[kid]; ok {
			return key, nil
		}
		// Unknown kid on a fresh cache: refetch, but not more often than
		// minRefreshInterval. The stamp is taken before the attempt so that a
		// failing upstream throttles the retries too.
		if time.Since(k.lastMiss) < minRefreshInterval {
			return nil, fmt.Errorf("no key with kid %q in the cached key set", kid)
		}
		k.lastMiss = time.Now()
	}

	if err := k.refreshLocked(ctx); err != nil {
		// A stale-but-usable cache beats a hard failure: identity being briefly
		// unreachable should not log every user out of the trip service.
		if key, ok := k.keys[kid]; ok {
			return key, nil
		}
		return nil, err
	}

	key, ok := k.keys[kid]
	if !ok {
		return nil, fmt.Errorf("no key with kid %q in the key set at %s", kid, k.url)
	}
	return key, nil
}

// Refresh fetches the document unconditionally. Used by readiness checks.
func (k *KeySet) Refresh(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.refreshLocked(ctx)
}

// refreshLocked replaces the cache. The caller holds k.mu.
//
// Holding the mutex across the HTTP call serialises concurrent misses onto one
// upstream request instead of letting every waiting request issue its own,
// which is the behaviour worth having when a key has just rotated.
func (k *KeySet) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return fmt.Errorf("build jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := k.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks from %s: %w", k.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks from %s: unexpected status %d", k.url, resp.StatusCode)
	}

	// The document is a handful of keys. Capping the read keeps a
	// misconfigured URL pointing at something enormous from being a memory
	// problem.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read jwks from %s: %w", k.url, err)
	}

	var set jwkSet
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("decode jwks from %s: %w", k.url, err)
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, key := range set.Keys {
		// Anything that is not an RS256 signing key cannot verify one of our
		// tokens, so it is skipped rather than treated as an error: a future
		// identity build publishing an extra key must not break this one.
		if key.Kty != "RSA" || key.Kid == "" {
			continue
		}
		if key.Alg != "" && key.Alg != "RS256" {
			continue
		}
		if key.Use != "" && key.Use != "sig" {
			continue
		}
		parsed, err := parseRSAPublicKey(key)
		if err != nil {
			return fmt.Errorf("jwks key %q: %w", key.Kid, err)
		}
		keys[key.Kid] = parsed
	}

	if len(keys) == 0 {
		return fmt.Errorf("jwks at %s contains no usable RS256 keys", k.url)
	}

	k.keys = keys
	k.fetchedAt = time.Now()
	return nil
}

func parseRSAPublicKey(key jwk) (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil {
		return nil, fmt.Errorf("modulus is not base64url: %w", err)
	}
	exponent, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil {
		return nil, fmt.Errorf("exponent is not base64url: %w", err)
	}
	if len(exponent) == 0 || len(exponent) > 8 {
		return nil, fmt.Errorf("exponent has implausible length %d", len(exponent))
	}

	e := 0
	for _, b := range exponent {
		e = e<<8 | int(b)
	}

	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(modulus), E: e}
	if pub.N.Sign() == 0 {
		return nil, fmt.Errorf("modulus is zero")
	}
	// RS256 with a key this small is not a key, it is a formality.
	if pub.N.BitLen() < 2048 {
		return nil, fmt.Errorf("modulus is %d bits, RS256 requires at least 2048", pub.N.BitLen())
	}
	return pub, nil
}
