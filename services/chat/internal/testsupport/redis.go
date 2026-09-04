package testsupport

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

const redisImage = "redis:7-alpine"

// sharedRedis is built once per test binary, like the database.
var sharedRedis struct {
	url  string
	err  error
	once sync.Once
}

// Redis returns a client on a flushed, throwaway Redis.
//
// A real Redis rather than a fake, because the three things this service asks of
// it are the three a fake gets wrong: GETDEL's atomicity is the whole of
// single-use tickets, the rate limiter is a Lua script, and pub/sub is the
// fan-out itself. A test against an in-memory stub would prove that the stub
// agrees with the code that was written against it.
func Redis(t *testing.T) *redis.Client {
	t.Helper()

	sharedRedis.once.Do(func() {
		// The same escape hatch the database has: `make up` leaves a Redis on
		// localhost:6379, and pointing at it turns a container start into
		// nothing.
		if url := os.Getenv("CHAT_TEST_REDIS_URL"); url != "" {
			sharedRedis.url = url
			return
		}
		sharedRedis.url, sharedRedis.err = startRedis()
	})
	if sharedRedis.err != nil {
		t.Fatalf("start redis container: %v", sharedRedis.err)
	}

	options, err := redis.ParseURL(sharedRedis.url)
	require.NoError(t, err)

	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })

	// Flushed per test rather than per binary. Tickets, presence and rate
	// limits are all keyed by ids the tests generate, so collisions are
	// impossible — but a leftover rate-limit counter from a previous test would
	// make the next one's twenty-first message the eighteenth, and that is
	// precisely the kind of ordering dependency a suite should not have.
	require.NoError(t, client.FlushDB(context.Background()).Err())
	return client
}

func startRedis() (string, error) {
	ctx := context.Background()
	container, err := tcredis.Run(ctx, redisImage)
	if err != nil {
		return "", err
	}
	return container.ConnectionString(ctx)
}
