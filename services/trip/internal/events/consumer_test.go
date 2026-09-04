package events_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/events"
	"github.com/togethergo/trip/internal/testsupport"
)

// The user projection consumer, driven end to end: a message published to the
// real exchange, routed by the real bindings, consumed by the real consumer,
// and asserted on in the real table.
//
// Nothing here fakes the broker. What is under test is partly the topology —
// that `user.profile_updated` reaches `trip.user-events` at all — and a fake
// would only prove the test agreed with itself.

// consumerWorld is `world` plus a running consumer.
type consumerWorld struct {
	*world
	consumer *events.Consumer
}

func newConsumerWorld(t *testing.T) *consumerWorld {
	t.Helper()

	w := newWorld(t)
	w.broker.Purge(testsupport.UserQueue)
	w.broker.Purge(testsupport.UserDLQ)

	consumer := events.NewConsumer(events.ConsumerOptions{
		Projection: w.store,
		AMQPURL:    w.broker.URL,
		// Discarded: the poison-message test deliberately logs at error, and a
		// suite that prints its own expected failures is a suite nobody reads.
		Logger:      slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DialTimeout: 2 * time.Second,
		// The contract's ladder is 200/400/800 ms; the tests do not need to
		// wait out 1.4 s to watch a message reach the DLQ.
		RetryBase: 5 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("consumer did not stop within 30s")
		}
	})

	return &consumerWorld{world: w, consumer: consumer}
}

// publish wraps a payload in an envelope and sends it under its own routing
// key, exactly as identity's relay would.
func (w *consumerWorld) publish(eventID uuid.UUID, eventType string, aggregate uuid.UUID, payload any) {
	w.t.Helper()

	body, err := json.Marshal(payload)
	require.NoError(w.t, err)

	envelope, err := json.Marshal(map[string]any{
		"event_id":      eventID,
		"event_type":    eventType,
		"event_version": 1,
		"occurred_at":   now.Format(time.RFC3339),
		"aggregate_id":  aggregate,
		"payload":       json.RawMessage(body),
	})
	require.NoError(w.t, err)

	w.broker.Publish(eventType, envelope)
}

// awaitName polls the projection until the user has the expected name, or
// fails. Polled rather than slept on: the consumer is asynchronous by nature
// and a fixed sleep is either flaky or slow, usually both.
func (w *consumerWorld) awaitName(userID uuid.UUID, want string) {
	w.t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		refs, err := w.store.UserRefs(w.ctx, []uuid.UUID{userID})
		require.NoError(w.t, err)
		if ref, ok := refs[userID]; ok {
			last = ref.FullName
			if last == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.t.Fatalf("projection never showed %q for %s (last saw %q)", want, userID, last)
}

func (w *consumerWorld) processed() int {
	w.t.Helper()

	var count int
	require.NoError(w.t, w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM processed_events`).Scan(&count))
	return count
}

// awaitProcessed polls until the ledger holds `want` rows and then holds still
// for a moment, so a test asserting "and no more arrived" is not just asserting
// that it looked early.
func (w *consumerWorld) awaitProcessed(want int) {
	w.t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if w.processed() == want {
			time.Sleep(300 * time.Millisecond)
			require.Equal(w.t, want, w.processed())
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	w.t.Fatalf("processed_events never reached %d rows (has %d)", want, w.processed())
}

func profilePayload(userID uuid.UUID, name string, at time.Time) map[string]any {
	return map[string]any{
		"user_id":      userID,
		"display_name": name,
		"avatar_url":   "https://cdn.example.test/" + userID.String() + ".jpg",
		"bio":          "Weekend hiker.",
		"updated_at":   at.Format(time.RFC3339),
	}
}

// TestTheSameProfileEventTwiceUpdatesTheProjectionOnce is the third acceptance
// criterion, end to end.
//
// The same event_id is published twice — which is what at-least-once delivery
// looks like from the outside — carrying a *different* name the second time.
// The projection must keep the first, because the second was never applied.
func TestTheSameProfileEventTwiceUpdatesTheProjectionOnce(t *testing.T) {
	w := newConsumerWorld(t)

	userID := uuid.New()
	eventID := uuid.New()

	w.publish(eventID, events.TypeUserProfileUpdated, userID, profilePayload(userID, "Alex", now))
	w.awaitName(userID, "Alex")

	w.publish(eventID, events.TypeUserProfileUpdated, userID, profilePayload(userID, "Someone Else", now.Add(time.Hour)))
	w.awaitProcessed(1)

	refs, err := w.store.UserRefs(w.ctx, []uuid.UUID{userID})
	require.NoError(t, err)
	require.Equal(t, "Alex", refs[userID].FullName, "the redelivery was ignored")

	// Both messages were acked, so neither is sitting on the queue or in the
	// DLQ. A duplicate is handled, not dead-lettered.
	require.Zero(t, w.broker.QueueDepth(testsupport.UserQueue))
	require.Zero(t, w.broker.QueueDepth(testsupport.UserDLQ))
}

// TestRatingUpdatesReachTheProjection: the other bound routing key, and proof
// that the two event types feed different columns of the same row.
func TestRatingUpdatesReachTheProjection(t *testing.T) {
	w := newConsumerWorld(t)
	userID := uuid.New()

	w.publish(uuid.New(), events.TypeUserProfileUpdated, userID, profilePayload(userID, "Alex K.", now))
	w.awaitName(userID, "Alex K.")

	w.publish(uuid.New(), events.TypeUserRatingUpdated, userID, map[string]any{
		"user_id":        userID,
		"rating_average": 4.75,
		"rating_count":   12,
		"updated_at":     now.Format(time.RFC3339),
	})
	w.awaitProcessed(2)

	refs, err := w.store.UserRefs(w.ctx, []uuid.UUID{userID})
	require.NoError(t, err)
	require.Equal(t, "Alex K.", refs[userID].FullName)
	require.NotNil(t, refs[userID].RatingAvg)
	require.InDelta(t, 4.75, *refs[userID].RatingAvg, 0.001)
	require.Equal(t, 12, refs[userID].RatingCount)
}

// TestAnUnknownEventTypeIsMarkedProcessedAndAcked.
//
// contracts/events.md: log at warn, mark processed, ack. An unknown event is
// not an error, it is a message meant for a newer version of this service, and
// dead-lettering it would fill a DLQ with messages nobody will ever act on.
//
// It is published under a bound routing key, because that is the only way one
// can reach this queue — the envelope's event_type is what the consumer
// dispatches on, and the two can disagree when a publisher is upgraded ahead of
// its consumers.
func TestAnUnknownEventTypeIsMarkedProcessedAndAcked(t *testing.T) {
	w := newConsumerWorld(t)
	userID := uuid.New()

	body, err := json.Marshal(map[string]any{
		"event_id":      uuid.New(),
		"event_type":    "user.something_new",
		"event_version": 1,
		"occurred_at":   now.Format(time.RFC3339),
		"aggregate_id":  userID,
		"payload":       map[string]any{"user_id": userID},
	})
	require.NoError(t, err)
	w.broker.Publish(events.TypeUserProfileUpdated, body)

	w.awaitProcessed(1)
	require.Zero(t, w.broker.QueueDepth(testsupport.UserDLQ), "an unknown type is not poison")
}

// TestAPoisonMessageIsDeadLetteredNotRequeued.
//
// A body that is not an envelope will never become one. The contract is blunt
// about the alternative: `requeue=true` puts the message back at the head of
// the same queue, it is redelivered at once, it fails again, and the consumer
// spins at full speed on it. So it goes to the DLQ, which is a human queue.
func TestAPoisonMessageIsDeadLetteredNotRequeued(t *testing.T) {
	w := newConsumerWorld(t)

	w.broker.Publish(events.TypeUserProfileUpdated, []byte(`{"this is not": `))

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if w.broker.QueueDepth(testsupport.UserDLQ) == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Equal(t, 1, w.broker.QueueDepth(testsupport.UserDLQ))

	// And it is not still going round the main queue.
	require.Zero(t, w.broker.QueueDepth(testsupport.UserQueue))
	require.Zero(t, w.processed(), "a message that never parsed is not 'handled'")
}
