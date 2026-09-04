package realtime

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Health is the readiness probe for Redis.
//
// A thin adapter, and it earns its keep by shape rather than by behaviour:
// go-redis returns its own command type, and internal/http asks its
// dependencies for `Ping(ctx) error` so that it can stay ignorant of both Redis
// and AMQP. One of the two ends has to convert, and it belongs on this side.
type Health struct {
	client *redis.Client
}

// NewHealth builds the probe over an already-configured client.
func NewHealth(client *redis.Client) *Health {
	return &Health{client: client}
}

// Ping round-trips to Redis. Not a cheap check by design: this service cannot
// accept a socket or deliver a message without Redis, so /readyz has to know
// that the connection actually works rather than that a client object exists.
func (h *Health) Ping(ctx context.Context) error {
	if err := h.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("ping redis: %w", err)
	}
	return nil
}
