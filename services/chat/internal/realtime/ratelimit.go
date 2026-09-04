package realtime

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// incrementAndExpire is the whole rate limiter, as one atomic script.
//
// INCR followed by a conditional EXPIRE from Go would be two round trips with a
// gap in the middle, and a process that died in that gap would leave a counter
// with no expiry — a user silenced in that room until somebody noticed. A
// script runs both on the server or neither.
//
// The window is anchored to the *first* message rather than to a wall-clock
// bucket. Fixed buckets are simpler but they let a user send the limit twice
// over a bucket boundary — forty messages in a hundred milliseconds, straddling
// two ten-second windows — which is exactly the burst the limit exists to stop.
// PEXPIRE is set once, on the message that creates the counter, and is not
// pushed forward by later messages: a counter that reset its own TTL on every
// increment would become "twenty messages per ten *idle* seconds" and never
// expire under sustained load.
var incrementAndExpire = redis.NewScript(`
	local count = redis.call('INCR', KEYS[1])
	if count == 1 then
		redis.call('PEXPIRE', KEYS[1], ARGV[1])
	end
	return count
`)

// Limiter caps how fast one user may write to one room.
type Limiter struct {
	client *redis.Client
	limit  int
	window time.Duration
}

// NewLimiter builds the limiter over an already-configured client.
func NewLimiter(client *redis.Client, limit int, window time.Duration) *Limiter {
	return &Limiter{client: client, limit: limit, window: window}
}

// Allow reports whether this message may be sent, and consumes one unit of
// budget either way.
//
// Counting the rejected attempts too is intentional: a client that keeps
// hammering after being told to stop stays over the limit rather than being
// let back in the moment it slows to exactly the limit.
//
// **It fails open.** A Redis error returns `true` with the error alongside, so
// the caller can log it and deliver the message. A limiter that failed closed
// would turn a Redis blip into a service that silently refuses every message
// with an error frame — which is a far worse outage than a few seconds of
// unlimited typing, and Redis being down is already visible on /readyz.
func (l *Limiter) Allow(ctx context.Context, tripID, userID uuid.UUID) (bool, error) {
	key := fmt.Sprintf("%s%s:%s", ratePrefix, tripID, userID)

	count, err := incrementAndExpire.Run(ctx, l.client,
		[]string{key},
		l.window.Milliseconds(),
	).Int64()
	if err != nil {
		return true, fmt.Errorf("rate limit check for %s in room %s: %w", userID, tripID, err)
	}
	return count <= int64(l.limit), nil
}
