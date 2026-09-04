package users

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TTL is how long a resolved user may be reused.
//
// Sixty seconds, matching the `Cache-Control: max-age=60` identity stamps on
// the same response. Aligning with the owner of the data is what makes a
// renamed user stale for a bounded, known interval platform-wide, instead of
// for however long each consumer happened to pick.
const TTL = 60 * time.Second

// keyPrefix namespaces this service's entries. Redis is shared between
// services (one container, CLAUDE.md), so every key says who wrote it.
const keyPrefix = "trip:user:"

// Cache is the short-lived store in front of identity.
//
// Neither method returns an error, and that is the contract rather than
// laziness: a cache that is down is a slower page, not a failed one. The
// implementations log and carry on, so no caller has to write "if the cache
// broke, do it anyway" — that is the only thing they could do.
type Cache interface {
	Get(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]User
	Put(ctx context.Context, users map[uuid.UUID]User)
}

// NoCache is the cache used when REDIS_URL is not configured. Every lookup is
// a miss; nothing is stored.
type NoCache struct{}

func (NoCache) Get(context.Context, []uuid.UUID) map[uuid.UUID]User {
	return map[uuid.UUID]User{}
}

func (NoCache) Put(context.Context, map[uuid.UUID]User) {}

// RedisCache is the real one.
type RedisCache struct {
	client *redis.Client
	ttl    time.Duration
	logger *slog.Logger
}

// NewRedisCache builds a cache over an already-configured client. The client's
// lifetime belongs to the caller.
func NewRedisCache(client *redis.Client, ttl time.Duration, logger *slog.Logger) *RedisCache {
	if ttl <= 0 {
		ttl = TTL
	}
	return &RedisCache{client: client, ttl: ttl, logger: logger}
}

// Get reads every id in one MGET.
//
// One round trip, not one per id: the whole point of this cache is to keep a
// fifty-item page down to a fixed number of network hops, and a loop here would
// have replaced identity's N+1 with Redis's.
func (c *RedisCache) Get(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]User {
	found := make(map[uuid.UUID]User, len(ids))
	if len(ids) == 0 {
		return found
	}

	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = keyPrefix + id.String()
	}

	values, err := c.client.MGet(ctx, keys...).Result()
	if err != nil {
		c.warn("user cache read failed", err)
		return found
	}

	for i, value := range values {
		// A miss is a nil element at that position, which is why MGet's result
		// is positional and must be walked alongside the ids rather than
		// filtered first.
		text, ok := value.(string)
		if !ok {
			continue
		}
		var user User
		if err := json.Unmarshal([]byte(text), &user); err != nil {
			// A value this service cannot parse is a value from an older
			// encoding of this struct. Treating it as a miss lets the entry
			// expire and be rewritten, rather than failing the page.
			c.warn("user cache entry could not be decoded", err)
			continue
		}
		found[ids[i]] = user
	}
	return found
}

// Put writes every entry in one pipeline, each with its own expiry.
//
// A per-key TTL rather than a single cached page: the same user organises many
// trips and appears on many pages, and keying by user is what makes the second
// page of a search mostly cache hits.
func (c *RedisCache) Put(ctx context.Context, users map[uuid.UUID]User) {
	if len(users) == 0 {
		return
	}

	pipe := c.client.Pipeline()
	for id, user := range users {
		encoded, err := json.Marshal(user)
		if err != nil {
			c.warn("user cache entry could not be encoded", err)
			continue
		}
		pipe.Set(ctx, keyPrefix+id.String(), encoded, c.ttl)
	}

	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		c.warn("user cache write failed", err)
	}
}

func (c *RedisCache) warn(msg string, err error) {
	if c.logger == nil {
		return
	}
	c.logger.Warn(msg, slog.String("error", err.Error()))
}
