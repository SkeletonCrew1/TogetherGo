package events_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/events"
	"github.com/togethergo/chat/internal/store"
	"github.com/togethergo/chat/internal/testsupport"
)

// These tests drive the consumer through a real broker, publishing the way the
// trip service's relay does and reading the result out of Postgres.
//
// The broker boots with the repository's own deploy/rabbitmq/definitions.json,
// so `chat.trip-events` is bound to the four routing keys this service consumes
// and `chat.trip-events.dlq` is where a poison message lands. A test that
// declared its own queue would be testing a topology nothing deploys.

// publish sends an envelope the way a relay would.
func publish(t *testing.T, broker *testsupport.Broker, eventID uuid.UUID, eventType string, aggregate uuid.UUID, payload any) {
	t.Helper()

	encoded, err := json.Marshal(payload)
	require.NoError(t, err)

	body, err := json.Marshal(events.Envelope{
		EventID:      eventID,
		EventType:    eventType,
		EventVersion: 1,
		OccurredAt:   time.Now().UTC().Format(time.RFC3339),
		AggregateID:  aggregate,
		Payload:      encoded,
	})
	require.NoError(t, err)

	broker.Publish(eventType, body)
}

// runConsumer starts a consumer against the test broker and stops it with the
// test.
func runConsumer(t *testing.T, broker *testsupport.Broker, projection events.Projection, evictor events.Evictor) {
	t.Helper()

	consumer := events.NewConsumer(events.ConsumerOptions{
		Projection: projection,
		Evictor:    evictor,
		AMQPURL:    broker.URL,
		Queue:      testsupport.ChatQueue,
		Logger:     testsupport.DiscardLogger(),
		RetryBase:  10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// The whole projection, end to end: a trip is created, somebody is approved,
// somebody is removed, and the trip is called off — and the room follows.
func TestTheConsumerProjectsTheRoomLifecycle(t *testing.T) {
	pool := testsupport.Pool(t)
	broker := testsupport.NewBroker(t)
	broker.Purge(testsupport.ChatQueue)

	st := store.New(pool)
	runConsumer(t, broker, st, nil)

	tripID, organizer, participant := uuid.New(), uuid.New(), uuid.New()

	publish(t, broker, uuid.New(), events.TypeTripCreated, tripID, events.TripCreated{
		TripID: tripID, OrganizerID: organizer, Title: "Carpathians ridge",
	})
	requireEventually(t, func() bool {
		return testsupport.MemberCount(t, pool, tripID) == 1
	}, "the room was provisioned with its organizer")

	publish(t, broker, uuid.New(), events.TypeJoinRequestApproved, tripID, events.JoinRequestApproved{
		TripID: tripID, ParticipantID: participant, Title: "Carpathians ridge",
	})
	requireEventually(t, func() bool {
		return testsupport.MemberCount(t, pool, tripID) == 2
	}, "the approved participant was added")

	publish(t, broker, uuid.New(), events.TypeParticipantRemoved, tripID, events.ParticipantRemoved{
		TripID: tripID, ParticipantID: participant,
	})
	requireEventually(t, func() bool {
		return testsupport.MemberCount(t, pool, tripID) == 1
	}, "the removed participant lost access")

	publish(t, broker, uuid.New(), events.TypeTripCancelled, tripID, events.TripCancelled{
		TripID: tripID, Title: "Carpathians ridge",
	})
	requireEventually(t, func() bool {
		access, err := st.Authorize(context.Background(), tripID, organizer)
		return err == nil && !access.Room.IsOpen()
	}, "the room was closed to new messages")
}

// The idempotency criterion, over the wire this time: the same event_id
// delivered twice adds one member row.
//
// Delivery is at-least-once by contract, so this is not a hypothetical: a relay
// that crashes between publishing and marking the row published republishes it,
// and a consumer that commits and dies before its ack gets it again.
func TestConsumingTheSameApprovalTwiceAddsOneMember(t *testing.T) {
	pool := testsupport.Pool(t)
	broker := testsupport.NewBroker(t)
	broker.Purge(testsupport.ChatQueue)

	runConsumer(t, broker, store.New(pool), nil)

	tripID, organizer, participant := uuid.New(), uuid.New(), uuid.New()
	publish(t, broker, uuid.New(), events.TypeTripCreated, tripID, events.TripCreated{
		TripID: tripID, OrganizerID: organizer, Title: "Carpathians ridge",
	})

	eventID := uuid.New()
	payload := events.JoinRequestApproved{
		TripID: tripID, ParticipantID: participant, Title: "Carpathians ridge",
	}
	publish(t, broker, eventID, events.TypeJoinRequestApproved, tripID, payload)
	publish(t, broker, eventID, events.TypeJoinRequestApproved, tripID, payload)

	requireEventually(t, func() bool {
		return testsupport.MemberCount(t, pool, tripID) == 2
	}, "the organizer and one participant")

	// Held for long enough that a second insert would have happened by now.
	// Asserting on a count that has not moved needs a window, not an instant.
	time.Sleep(500 * time.Millisecond)
	require.Equal(t, 2, testsupport.MemberCount(t, pool, tripID),
		"the redelivery must not have added a second row")
}

// A body that is not a valid envelope goes straight to the DLQ. Retrying it
// three times would be three more log lines and the same answer.
func TestAnUnreadableMessageIsDeadLettered(t *testing.T) {
	pool := testsupport.Pool(t)
	broker := testsupport.NewBroker(t)
	broker.Purge(testsupport.ChatQueue)
	broker.Purge(testsupport.ChatDLQ)

	runConsumer(t, broker, store.New(pool), nil)

	broker.Publish(events.TypeTripCreated, []byte("this is not an envelope"))

	requireEventually(t, func() bool {
		return broker.QueueDepth(testsupport.ChatDLQ) == 1
	}, "the poison message reached chat.trip-events.dlq")
}

// An event type this build has never heard of is logged, marked processed and
// acked — never dead-lettered. contracts/events.md is explicit: it is a message
// meant for a newer version of this service, not an error.
func TestAnUnknownEventTypeIsAckedRatherThanDeadLettered(t *testing.T) {
	pool := testsupport.Pool(t)
	broker := testsupport.NewBroker(t)
	broker.Purge(testsupport.ChatQueue)
	broker.Purge(testsupport.ChatDLQ)

	runConsumer(t, broker, store.New(pool), nil)

	tripID := uuid.New()
	eventID := uuid.New()
	// Published under a routing key the queue *is* bound to, carrying a type
	// the dispatch does not know — which is what a redeployment order or a
	// widened binding actually looks like.
	publish(t, broker, eventID, "trip.something_new", tripID, map[string]string{"trip_id": tripID.String()})
	broker.Publish(events.TypeTripCreated, mustEnvelope(t, eventID, "trip.something_new", tripID))

	requireEventually(t, func() bool {
		var processed bool
		err := pool.QueryRow(context.Background(),
			`SELECT EXISTS (SELECT 1 FROM processed_events WHERE event_id = $1)`, eventID).Scan(&processed)
		return err == nil && processed
	}, "the unknown event was recorded as handled")

	require.Equal(t, 0, broker.QueueDepth(testsupport.ChatDLQ))
}

func mustEnvelope(t *testing.T, eventID uuid.UUID, eventType string, aggregate uuid.UUID) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"trip_id": aggregate.String()})
	require.NoError(t, err)
	body, err := json.Marshal(events.Envelope{
		EventID:      eventID,
		EventType:    eventType,
		EventVersion: 1,
		OccurredAt:   time.Now().UTC().Format(time.RFC3339),
		AggregateID:  aggregate,
		Payload:      payload,
	})
	require.NoError(t, err)
	return body
}

// requireEventually polls until the condition holds, because everything in this
// file crosses a broker and none of it is synchronous.
func requireEventually(t *testing.T, condition func() bool, what string) {
	t.Helper()
	require.Eventually(t, condition, 15*time.Second, 50*time.Millisecond, what)
}
