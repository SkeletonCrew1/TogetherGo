// Package events_test drives the relay against a real RabbitMQ and a real
// Postgres.
//
// External test package (`events_test`, not `events`) because these tests write
// the outbox through internal/store, and internal/store imports internal/events
// — an internal test file would be an import cycle. It is also the honest
// arrangement: what is under test is the relay's behaviour from outside, which
// is the only thing the rest of the system can see.
package events_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/events"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/testsupport"
)

// The clock the fixtures validate against, so "start_at must be in the future"
// means the same thing on every machine and in a year's time.
var now = time.Now().UTC()

type world struct {
	t      *testing.T
	pool   *pgxpool.Pool
	store  *store.Store
	broker *testsupport.Broker
	ctx    context.Context
}

func newWorld(t *testing.T) *world {
	t.Helper()

	broker := testsupport.NewBroker(t)
	pool := testsupport.Pool(t)

	// Every queue this service publishes into, emptied before the test runs:
	// the broker is shared by the whole binary and its queues are durable, so
	// one test's events would otherwise be another's.
	broker.Purge(testsupport.NotificationQueue)
	broker.Purge(testsupport.ChatQueue)

	return &world{
		t:      t,
		pool:   pool,
		store:  store.New(pool).WithClock(func() time.Time { return now }),
		broker: broker,
		ctx:    context.Background(),
	}
}

// relay builds a relay pointed at this world, with a fast poll so tests do not
// wait on the production half-second.
func (w *world) relay(opts ...func(*events.RelayOptions)) *events.Relay {
	options := events.RelayOptions{
		Pool:     w.pool,
		AMQPURL:  w.broker.URL,
		Exchange: events.Exchange,
		// Discarded rather than left on slog.Default(): the relay logs a warning
		// per failed tick, and the reconnection test spends several seconds
		// deliberately failing them.
		Logger:       slog.New(slog.NewJSONHandler(io.Discard, nil)),
		PollInterval: 50 * time.Millisecond,
		DialTimeout:  2 * time.Second,
		BatchTimeout: 10 * time.Second,
	}
	for _, apply := range opts {
		apply(&options)
	}
	return events.NewRelay(options)
}

// run starts a relay for the duration of the test and returns a function that
// stops it and waits for it to finish, so a test can assert on what happened
// after shutdown rather than racing it.
func (w *world) run(relay *events.Relay) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.Run(ctx)
	}()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			w.t.Fatal("relay did not stop within 30s")
		}
	}
	w.t.Cleanup(stop)
	return stop
}

// recruitingTrip creates a trip and publishes it. The outbox is cleared
// afterwards so a test counts only the events it went on to cause.
func (w *world) recruitingTrip(organizer uuid.UUID, capacity int) uuid.UUID {
	w.t.Helper()

	detail, err := w.store.CreateTrip(w.ctx, organizer, domain.TripInput{
		Title:    "Carpathians in autumn",
		Category: "hiking",
		Capacity: capacity,
		StartAt:  now.Add(14 * 24 * time.Hour),
		EndAt:    now.Add(17 * 24 * time.Hour),
		Points: []domain.PointInput{
			{Name: "Lviv", Lat: 49.8397, Lng: 24.0297},
			{Name: "Hoverla", Lat: 48.1600, Lng: 24.5000},
		},
	})
	require.NoError(w.t, err)

	_, err = w.store.ChangeStatus(w.ctx, detail.Trip.ID, organizer, domain.StatusRecruiting)
	require.NoError(w.t, err)

	// Publishing the draft writes a trip.status_changed of its own, and these
	// tests count messages. Clearing the outbox here is what makes every
	// assertion below about the events the test went on to cause rather than
	// about the fixture that set it up.
	_, err = w.pool.Exec(w.ctx, `DELETE FROM outbox`)
	require.NoError(w.t, err)

	return detail.Trip.ID
}

func (w *world) unpublished() int {
	w.t.Helper()

	var count int
	require.NoError(w.t, w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&count))
	return count
}

// envelope is the wire shape, decoded exactly as a consumer in another service
// would have to decode it.
type envelope struct {
	EventID      uuid.UUID       `json:"event_id"`
	EventType    string          `json:"event_type"`
	EventVersion int             `json:"event_version"`
	OccurredAt   string          `json:"occurred_at"`
	AggregateID  uuid.UUID       `json:"aggregate_id"`
	Payload      json.RawMessage `json:"payload"`
}

func (w *world) consume(queue string, count int, timeout time.Duration) []envelope {
	w.t.Helper()

	bodies := w.broker.Consume(queue, count, timeout)
	out := make([]envelope, 0, len(bodies))
	for _, body := range bodies {
		var e envelope
		require.NoError(w.t, json.Unmarshal(body, &e), "body was: %s", body)
		out = append(out, e)
	}
	return out
}

// --- the envelope on the wire ----------------------------------------------

// The relay's whole job, asserted from the far end of the bus: a domain write
// becomes a message in the consumer's queue, wrapped in the five envelope
// fields contracts/events.md fixes.
func TestRelayPublishesTheOutboxWithTheContractEnvelope(t *testing.T) {
	w := newWorld(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := w.recruitingTrip(organizer, 8)

	request, err := w.store.CreateJoinRequest(w.ctx, tripID, joiner, nil)
	require.NoError(t, err)
	approved, err := w.store.ApproveJoinRequest(w.ctx, tripID, request.ID, organizer)
	require.NoError(t, err)

	w.run(w.relay())

	// notification.events is bound to `join_request.*`, so both the creation
	// and the approval land in it — in that order, since the relay publishes a
	// batch in created_at order.
	received := w.consume(testsupport.NotificationQueue, 2, 30*time.Second)

	require.Equal(t, events.TypeJoinRequestCreated, received[0].EventType)
	require.Equal(t, events.TypeJoinRequestApproved, received[1].EventType)

	for _, e := range received {
		require.NotEqual(t, uuid.Nil, e.EventID)
		require.Equal(t, 1, e.EventVersion)
		require.Equal(t, tripID, e.AggregateID, "the aggregate is the trip")

		// UTC, RFC 3339, Z rather than +00:00 — the way every timestamp on the
		// bus is spelled.
		occurred, err := time.Parse(time.RFC3339, e.OccurredAt)
		require.NoError(t, err)
		require.Equal(t, e.OccurredAt, occurred.UTC().Format(time.RFC3339))
	}

	// The approval payload carries everything chat and notification need, so
	// neither has to call back into this service to render its side.
	var payload struct {
		JoinRequestID    uuid.UUID `json:"join_request_id"`
		TripID           uuid.UUID `json:"trip_id"`
		Title            string    `json:"title"`
		OrganizerID      uuid.UUID `json:"organizer_id"`
		ParticipantID    uuid.UUID `json:"participant_id"`
		ParticipantCount int       `json:"participant_count"`
		MaxParticipants  int       `json:"max_participants"`
	}
	require.NoError(t, json.Unmarshal(received[1].Payload, &payload))
	require.Equal(t, approved.ID, payload.JoinRequestID)
	require.Equal(t, tripID, payload.TripID)
	require.Equal(t, "Carpathians in autumn", payload.Title)
	require.Equal(t, organizer, payload.OrganizerID)
	require.Equal(t, joiner, payload.ParticipantID)
	require.Equal(t, 2, payload.ParticipantCount)
	require.Equal(t, 8, payload.MaxParticipants)

	require.Zero(t, w.unpublished(), "every row was marked published")
}

// The event_id on the wire is the outbox row's id, which is what makes the
// whole chain idempotent: a batch republished after a relay crash carries the
// same id and consumers deduplicate on it.
func TestTheEventIDIsTheOutboxRowID(t *testing.T) {
	w := newWorld(t)
	organizer := uuid.New()
	tripID := w.recruitingTrip(organizer, 8)

	_, err := w.store.CreateJoinRequest(w.ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)

	var rowID uuid.UUID
	require.NoError(t, w.pool.QueryRow(w.ctx,
		`SELECT id FROM outbox WHERE event_type = $1`, events.TypeJoinRequestCreated).Scan(&rowID))

	w.run(w.relay())
	received := w.consume(testsupport.NotificationQueue, 1, 30*time.Second)
	require.Equal(t, rowID, received[0].EventID)
}

// Routing keys are the event types verbatim, and the bindings in
// definitions.json are what decide who hears what. participant.removed goes to
// chat and not to notification; the invite goes the other way.
func TestEventsReachTheQueuesTheirBindingsName(t *testing.T) {
	w := newWorld(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := w.recruitingTrip(organizer, 8)

	request, err := w.store.CreateJoinRequest(w.ctx, tripID, joiner, nil)
	require.NoError(t, err)
	_, err = w.store.ApproveJoinRequest(w.ctx, tripID, request.ID, organizer)
	require.NoError(t, err)
	_, err = w.store.RemoveParticipant(w.ctx, tripID, joiner, organizer)
	require.NoError(t, err)
	_, err = w.store.InviteUsers(w.ctx, tripID, organizer, []uuid.UUID{uuid.New()})
	require.NoError(t, err)

	w.run(w.relay())

	// chat.trip-events is bound to join_request.approved and
	// participant.removed only.
	chat := w.consume(testsupport.ChatQueue, 2, 30*time.Second)
	require.Equal(t, events.TypeJoinRequestApproved, chat[0].EventType)
	require.Equal(t, events.TypeParticipantRemoved, chat[1].EventType)

	// notification.events is bound to join_request.* and trip.invite_sent.
	notification := w.consume(testsupport.NotificationQueue, 3, 30*time.Second)
	var types []string
	for _, e := range notification {
		types = append(types, e.EventType)
	}
	require.Equal(t, []string{
		events.TypeJoinRequestCreated,
		events.TypeJoinRequestApproved,
		events.TypeTripInviteSent,
	}, types)
}

// --- surviving a broker restart --------------------------------------------

// TestEventsSurviveABrokerRestart is the second acceptance criterion, and the
// reason the relay reconnects from inside its own loop rather than at startup.
//
// Kill RabbitMQ, approve three requests, bring RabbitMQ back: all three events
// arrive, and no restart of this service was needed to make that happen. While
// the broker is down the relay's ticks fail and back off, the rows stay
// `published_at IS NULL`, and nothing is lost — which is the whole point of
// writing the event as a row in the first place.
func TestEventsSurviveABrokerRestart(t *testing.T) {
	w := newWorld(t)
	organizer := uuid.New()
	tripID := w.recruitingTrip(organizer, 10)

	requests := make([]uuid.UUID, 0, 3)
	for range 3 {
		request, err := w.store.CreateJoinRequest(w.ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)
		requests = append(requests, request.ID)
	}

	// Drain the three creations first, so what arrives after the restart is
	// unambiguously the approvals.
	stopFirst := w.run(w.relay())
	w.consume(testsupport.NotificationQueue, 3, 30*time.Second)
	require.Zero(t, w.unpublished())
	stopFirst()

	// The relay under test runs across the whole outage. A backoff capped well
	// below the default 30 seconds so the test does not spend half a minute
	// waiting for the next attempt after the broker comes back.
	relay := w.relay(func(o *events.RelayOptions) {
		o.MaxBackoff = 2 * time.Second
	})
	w.run(relay)

	w.broker.Stop()

	for _, requestID := range requests {
		_, err := w.store.ApproveJoinRequest(w.ctx, tripID, requestID, organizer)
		require.NoError(t, err, "the API keeps working while the broker is down")
	}

	// Give the relay time to fail a few ticks against the dead broker. The
	// events must still be in the outbox: unpublishable is not the same as
	// lost, and a relay that dropped them would look identical up to here.
	time.Sleep(2 * time.Second)
	require.Equal(t, 3, w.unpublished(), "nothing was dropped while the broker was down")

	w.broker.Start()

	received := w.consume(testsupport.NotificationQueue, 3, 60*time.Second)
	require.Len(t, received, 3)
	for _, e := range received {
		require.Equal(t, events.TypeJoinRequestApproved, e.EventType)
	}

	require.Eventually(t, func() bool { return w.unpublished() == 0 },
		30*time.Second, 100*time.Millisecond,
		"the outbox drained once the broker came back")
}

// --- marking, cleanup and shutdown -----------------------------------------

// Rows are marked in the same transaction that published them, so a second pass
// finds nothing to do. Without publisher confirms and that shared transaction,
// `published_at` would mean "we wrote to a socket", which is a different claim.
func TestPublishedRowsAreNotRepublished(t *testing.T) {
	w := newWorld(t)
	organizer := uuid.New()
	tripID := w.recruitingTrip(organizer, 8)

	_, err := w.store.CreateJoinRequest(w.ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)

	stop := w.run(w.relay())
	w.consume(testsupport.NotificationQueue, 1, 30*time.Second)
	stop()

	var publishedAt *time.Time
	require.NoError(t, w.pool.QueryRow(w.ctx,
		`SELECT published_at FROM outbox WHERE event_type = $1`,
		events.TypeJoinRequestCreated).Scan(&publishedAt))
	require.NotNil(t, publishedAt)

	// A second relay over the same table publishes nothing, so nothing new
	// reaches the queue.
	w.run(w.relay())
	time.Sleep(500 * time.Millisecond)

	var depth int
	require.NoError(t, w.pool.QueryRow(w.ctx, `SELECT count(*) FROM outbox`).Scan(&depth))
	require.Equal(t, 1, depth)
	require.Zero(t, w.unpublished())
}

// The sweep deletes published rows past the retention window and leaves
// unpublished ones alone however old they are — a row still NULL after a week
// is a stuck event and wants a human, not a DELETE.
func TestCleanupPrunesOnlyOldPublishedRows(t *testing.T) {
	w := newWorld(t)

	aggregate := uuid.New()
	insert := func(publishedDaysAgo int, published bool) uuid.UUID {
		id := uuid.New()
		var publishedAt any
		if published {
			publishedAt = now.AddDate(0, 0, -publishedDaysAgo)
		}
		_, err := w.pool.Exec(w.ctx, `
			INSERT INTO outbox (id, aggregate_id, event_type, payload, created_at, published_at)
			VALUES ($1, $2, $3, '{}'::jsonb, $4, $5)`,
			id, aggregate, events.TypeJoinRequestCreated,
			now.AddDate(0, 0, -publishedDaysAgo-1), publishedAt)
		require.NoError(w.t, err)
		return id
	}

	old := insert(30, true)
	recent := insert(1, true)
	stuck := insert(30, false)

	relay := w.relay(func(o *events.RelayOptions) {
		o.Retention = 7 * 24 * time.Hour
		o.CleanupInterval = time.Hour
		o.CleanupDelay = 50 * time.Millisecond
	})
	w.run(relay)

	require.Eventually(t, func() bool {
		var count int
		require.NoError(t, w.pool.QueryRow(w.ctx,
			`SELECT count(*) FROM outbox WHERE id = $1`, old).Scan(&count))
		return count == 0
	}, 30*time.Second, 100*time.Millisecond, "the old published row was pruned")

	for _, id := range []uuid.UUID{recent, stuck} {
		var count int
		require.NoError(t, w.pool.QueryRow(w.ctx,
			`SELECT count(*) FROM outbox WHERE id = $1`, id).Scan(&count))
		require.Equal(t, 1, count, "row %s should have been kept", id)
	}
}

// Shutdown finishes what it started. Cancelling the relay's context stops it
// from beginning another batch; the one in flight runs to completion, because
// abandoning one between "the broker confirmed these" and "the rows are marked
// published" would republish the lot on the next boot for no reason.
func TestShutdownFinishesTheBatchInFlight(t *testing.T) {
	w := newWorld(t)
	organizer := uuid.New()
	tripID := w.recruitingTrip(organizer, 20)

	for range 5 {
		_, err := w.store.CreateJoinRequest(w.ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)
	}
	require.Equal(t, 5, w.unpublished())

	relay := w.relay()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		relay.Run(ctx)
	}()

	// Cancel while the first batch is very likely in flight. Whichever side of
	// the batch the cancellation lands on, the invariant is the same: the relay
	// returns, and no row is left half-processed — either published and marked,
	// or neither.
	time.Sleep(60 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("relay did not stop")
	}

	var marked int
	require.NoError(t, w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM outbox WHERE published_at IS NOT NULL`).Scan(&marked))
	require.Contains(t, []int{0, 5}, marked, "a batch is all or nothing")

	// Whatever was left is picked up by the next relay — nothing is stranded.
	w.run(w.relay())
	require.Eventually(t, func() bool { return w.unpublished() == 0 },
		30*time.Second, 100*time.Millisecond)
	w.consume(testsupport.NotificationQueue, 5, 30*time.Second)
}

// A relay that starts before RabbitMQ does starts anyway: the broker is
// contacted lazily from the publish loop, so ordering the two at deploy time is
// not something anybody has to get right.
func TestARelayStartsWithoutABroker(t *testing.T) {
	w := newWorld(t)

	relay := w.relay(func(o *events.RelayOptions) {
		o.AMQPURL = "amqp://nobody:nobody@127.0.0.1:1/"
		o.MaxBackoff = 200 * time.Millisecond
	})
	stop := w.run(relay)

	organizer := uuid.New()
	tripID := w.recruitingTrip(organizer, 8)
	_, err := w.store.CreateJoinRequest(w.ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)

	time.Sleep(time.Second)
	require.Equal(t, 1, w.unpublished(), "the row waits rather than being dropped")
	require.False(t, relay.Connected())
	require.Error(t, relay.Ping(w.ctx), "/readyz reports the broker as unavailable")

	stop()

	// The same rows, published by a relay that can reach the broker.
	w.run(w.relay())
	w.consume(testsupport.NotificationQueue, 1, 30*time.Second)
}
