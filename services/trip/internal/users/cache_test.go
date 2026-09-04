package users_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/togethergo/trip/internal/users"
)

// The Redis cache is tested against a real redis:7-alpine — the same image
// Compose runs — because what is being asserted is that MGET returns misses
// positionally and that SET carries a TTL. A fake that agreed with the code
// would prove neither.

func newRedis(t *testing.T) *goredis.Client {
	t.Helper()

	ctx := context.Background()
	container, err := redis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	endpoint, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	options, err := goredis.ParseURL(endpoint)
	require.NoError(t, err)

	client := goredis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, client.Ping(ctx).Err())
	return client
}

func TestRedisCacheRoundTrip(t *testing.T) {
	client := newRedis(t)
	cache := users.NewRedisCache(client, users.TTL, nil)
	ctx := context.Background()

	rating := 4.8
	photo := "https://example.test/olena.jpg"
	olena := users.User{
		ID: uuid.New(), FullName: "Olena K.", PhotoURL: &photo, RatingAvg: &rating, RatingCount: 31,
	}
	// A user with nothing but a name: the nullable fields have to survive the
	// round trip as nulls, not as zeroes.
	andrii := users.User{ID: uuid.New(), FullName: "Andrii B."}

	cache.Put(ctx, map[uuid.UUID]users.User{olena.ID: olena, andrii.ID: andrii})

	found := cache.Get(ctx, []uuid.UUID{olena.ID, andrii.ID})
	require.Len(t, found, 2)
	require.Equal(t, olena, found[olena.ID])
	require.Equal(t, andrii, found[andrii.ID])
	require.Nil(t, found[andrii.ID].RatingAvg)
	require.Nil(t, found[andrii.ID].PhotoURL)
}

// TestRedisCacheReportsMissesPositionally is the bug MGET invites: its result
// is one element per key, with nil for the misses, so the ids and the values
// have to be walked together.
func TestRedisCacheReportsMissesPositionally(t *testing.T) {
	client := newRedis(t)
	cache := users.NewRedisCache(client, users.TTL, nil)
	ctx := context.Background()

	present := users.User{ID: uuid.New(), FullName: "Olena K."}
	cache.Put(ctx, map[uuid.UUID]users.User{present.ID: present})

	absentBefore, absentAfter := uuid.New(), uuid.New()
	found := cache.Get(ctx, []uuid.UUID{absentBefore, present.ID, absentAfter})

	require.Len(t, found, 1)
	require.Equal(t, present, found[present.ID],
		"the hit must be attributed to its own id, not to a neighbour's")
}

// TestRedisCacheExpiresEntries: sixty seconds is the contract, and the entry
// has to actually carry it.
func TestRedisCacheExpiresEntries(t *testing.T) {
	client := newRedis(t)
	ctx := context.Background()

	cache := users.NewRedisCache(client, users.TTL, nil)
	user := users.User{ID: uuid.New(), FullName: "Olena K."}
	cache.Put(ctx, map[uuid.UUID]users.User{user.ID: user})

	ttl, err := client.TTL(ctx, "trip:user:"+user.ID.String()).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 55*time.Second)
	require.LessOrEqual(t, ttl, users.TTL)

	// And a short TTL really does let the entry go, rather than the expiry
	// being a value written and never applied.
	brief := users.NewRedisCache(client, 50*time.Millisecond, nil)
	short := users.User{ID: uuid.New(), FullName: "Andrii B."}
	brief.Put(ctx, map[uuid.UUID]users.User{short.ID: short})
	require.Len(t, brief.Get(ctx, []uuid.UUID{short.ID}), 1)

	require.Eventually(t, func() bool {
		return len(brief.Get(ctx, []uuid.UUID{short.ID})) == 0
	}, 3*time.Second, 25*time.Millisecond)
}

// TestRedisCacheSurvivesAnUnreachableRedis is the reason neither method returns
// an error. A cache that is down is a slower page, never a failed one.
func TestRedisCacheSurvivesAnUnreachableRedis(t *testing.T) {
	client := newRedis(t)
	cache := users.NewRedisCache(client, users.TTL, nil)
	ctx := context.Background()

	user := users.User{ID: uuid.New(), FullName: "Olena K."}
	cache.Put(ctx, map[uuid.UUID]users.User{user.ID: user})
	require.NoError(t, client.Close())

	require.NotPanics(t, func() {
		require.Empty(t, cache.Get(ctx, []uuid.UUID{user.ID}), "every lookup becomes a miss")
		cache.Put(ctx, map[uuid.UUID]users.User{user.ID: user})
	})
}

// TestRedisCacheTreatsUndecodableEntriesAsMisses: an entry written by an older
// encoding of the struct must expire quietly, not fail the page that read it.
func TestRedisCacheTreatsUndecodableEntriesAsMisses(t *testing.T) {
	client := newRedis(t)
	cache := users.NewRedisCache(client, users.TTL, nil)
	ctx := context.Background()

	id := uuid.New()
	require.NoError(t, client.Set(ctx, "trip:user:"+id.String(), "not json at all", time.Minute).Err())

	require.Empty(t, cache.Get(ctx, []uuid.UUID{id}))
}

// TestNoCacheIsAlwaysAMiss — the configuration a service without REDIS_URL runs.
func TestNoCacheIsAlwaysAMiss(t *testing.T) {
	ctx := context.Background()
	cache := users.NoCache{}

	id := uuid.New()
	cache.Put(ctx, map[uuid.UUID]users.User{id: {ID: id, FullName: "Olena K."}})
	require.Empty(t, cache.Get(ctx, []uuid.UUID{id}))
}
