package realtime

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// channelBuffer is how many messages the client library holds for us before it
// starts dropping them.
//
// go-redis's PubSub.Channel drops — and logs — when its buffer is full, rather
// than blocking the connection that feeds it, which is the right trade for a
// pub/sub reader: falling behind on one room must not stall the fan-out for
// every other. The number is high enough that only a genuinely stuck hub loop
// reaches it.
const channelBuffer = 256

// Bus is the fan-out between service replicas.
//
// Without it, two users on two instances cannot see each other's messages: each
// process only holds the sockets that happen to have landed on it, and a
// message written on one would reach nobody on the other. Every persisted
// message is published to `chat:room:<trip_id>`, every instance holding a
// socket for that room is subscribed, and each forwards to its own sockets.
//
// Redis pub/sub is fire-and-forget: a message published while an instance is
// disconnected is not replayed to it when it reconnects. That is acceptable
// precisely because the message is already in Postgres before it is published —
// the durable copy is the database, this is only the live path, and a client
// that missed a frame refetches history on reconnect.
type Bus struct {
	client *redis.Client
	pubsub *redis.PubSub
	logger *slog.Logger
}

// NewBus opens the subscriber connection.
//
// Subscribing to nothing up front is deliberate: rooms are subscribed to as
// sockets arrive and unsubscribed as the last one leaves, so an instance only
// carries the traffic of rooms it actually has somebody in. The alternative — a
// pattern subscription on `chat:room:*` — is one line shorter and delivers
// every room's traffic to every replica.
func NewBus(client *redis.Client, logger *slog.Logger) *Bus {
	return &Bus{
		client: client,
		pubsub: client.Subscribe(context.Background()),
		logger: logger,
	}
}

// Channel returns the Redis channel name for a room. Exported because the tests
// subscribe to it directly to prove that a message actually left the process.
func Channel(tripID uuid.UUID) string { return roomPrefix + tripID.String() }

// Publish sends one pre-encoded envelope to a room.
func (b *Bus) Publish(ctx context.Context, tripID uuid.UUID, payload []byte) error {
	if err := b.client.Publish(ctx, Channel(tripID), payload).Err(); err != nil {
		return fmt.Errorf("publish to room %s: %w", tripID, err)
	}
	return nil
}

// Subscribe starts delivering a room's traffic to this instance.
//
// go-redis re-issues its subscriptions itself when the connection drops, so
// there is no reconnection logic here and none is missing: after a Redis
// restart the rooms this instance had are resubscribed without anybody being
// told.
func (b *Bus) Subscribe(ctx context.Context, tripID uuid.UUID) error {
	if err := b.pubsub.Subscribe(ctx, Channel(tripID)); err != nil {
		return fmt.Errorf("subscribe to room %s: %w", tripID, err)
	}
	return nil
}

// Unsubscribe stops delivering a room's traffic, once the last local socket for
// it has gone.
func (b *Bus) Unsubscribe(ctx context.Context, tripID uuid.UUID) error {
	if err := b.pubsub.Unsubscribe(ctx, Channel(tripID)); err != nil {
		return fmt.Errorf("unsubscribe from room %s: %w", tripID, err)
	}
	return nil
}

// Run reads the subscription and hands every message to deliver, until ctx is
// cancelled.
//
// One goroutine for the whole process, not one per room. `deliver` is called on
// it, so it must not block: the hub's implementation takes a read lock, copies
// a slice of connections and does a non-blocking send to each, which is the
// reason a slow client is disconnected rather than allowed to back up into this
// loop.
func (b *Bus) Run(ctx context.Context, deliver func(tripID uuid.UUID, payload []byte)) {
	messages := b.pubsub.Channel(redis.WithChannelSize(channelBuffer))

	for {
		select {
		case <-ctx.Done():
			if err := b.pubsub.Close(); err != nil {
				b.logger.Warn("closing the redis subscription failed", slog.String("error", err.Error()))
			}
			b.logger.Info("room fan-out stopped")
			return

		case message, open := <-messages:
			if !open {
				b.logger.Warn("the redis subscription channel closed; room fan-out has stopped")
				return
			}
			name, ok := strings.CutPrefix(message.Channel, roomPrefix)
			tripID, err := uuid.Parse(name)
			if !ok || err != nil {
				// Something else is publishing under our prefix. Not our
				// message and not our problem, but worth a line: it means the
				// key namespace is being shared with something unexpected.
				b.logger.Warn("ignoring a fan-out message on an unparseable channel",
					slog.String("channel", message.Channel))
				continue
			}
			deliver(tripID, []byte(message.Payload))
		}
	}
}
