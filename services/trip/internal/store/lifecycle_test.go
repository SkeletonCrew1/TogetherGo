package store_test

import (
	"context"
	"encoding/json"
	"sync"
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

// The lifecycle scheduler's store half.
//
// Every test here drives Store.LifecycleTick, which is exactly what the debug
// endpoint and the sixty-second loop call — there is no test-only path through
// the transitions, so what these assertions cover is what production runs.

// finishedTrip builds a trip that is in_progress with `others` extra
// participants aboard, its end_at already gone by.
//
// It goes through the real flow — create, publish, apply, approve, start — and
// only then rewinds the dates, because the roster is what trip.completed
// carries and a hand-written INSERT into participants would be asserting
// against a fixture rather than against the service.
func finishedTrip(t *testing.T, s *store.Store, pool *pgxpool.Pool, organizer uuid.UUID, others int) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	detail, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)
	tripID := detail.Trip.ID

	_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusRecruiting)
	require.NoError(t, err)

	for i := 0; i < others; i++ {
		joiner := uuid.New()
		request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
		require.NoError(t, err)
		_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
		require.NoError(t, err)
	}

	_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusInProgress)
	require.NoError(t, err)

	// The trip ran yesterday and finished an hour ago.
	testsupport.Rewind(t, pool, tripID, now.Add(-26*time.Hour), now.Add(-time.Hour))
	return tripID
}

// outboxPayloads returns every unpublished payload of one event type for a
// trip, newest last. Counting rows is what "exactly once" means here.
func outboxPayloads(t *testing.T, pool *pgxpool.Pool, tripID uuid.UUID, eventType string) []json.RawMessage {
	t.Helper()

	rows, err := pool.Query(context.Background(), `
		SELECT payload FROM outbox
		WHERE aggregate_id = $1 AND event_type = $2
		ORDER BY created_at, id`, tripID, eventType)
	require.NoError(t, err)
	defer rows.Close()

	var payloads []json.RawMessage
	for rows.Next() {
		var payload json.RawMessage
		require.NoError(t, rows.Scan(&payload))
		payloads = append(payloads, payload)
	}
	require.NoError(t, rows.Err())
	return payloads
}

func statusOf(t *testing.T, pool *pgxpool.Pool, tripID uuid.UUID) domain.Status {
	t.Helper()

	var status domain.Status
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status FROM trips WHERE id = $1`, tripID).Scan(&status))
	return status
}

// TestTickCompletesADueTripAndEmitsTheRosterOnce is the first acceptance
// criterion: a trip whose end_at has passed is completed by one tick, and
// exactly one trip.completed row is in the outbox.
func TestTickCompletesADueTripAndEmitsTheRosterOnce(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	tripID := finishedTrip(t, s, pool, organizer, 2)

	result, err := s.LifecycleTick(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, result.Completed)
	require.Equal(t, 1, result.CompletedEvents)
	require.Equal(t, domain.StatusCompleted, statusOf(t, pool, tripID))

	completed := outboxPayloads(t, pool, tripID, events.TypeTripCompleted)
	require.Len(t, completed, 1, "exactly one trip.completed per trip")

	var payload events.TripCompleted
	require.NoError(t, json.Unmarshal(completed[0], &payload))
	require.Equal(t, tripID, payload.TripID)
	require.Equal(t, "Carpathians in autumn", payload.Title)
	require.Equal(t, organizer, payload.OrganizerID)

	// The roster is the whole point of this payload: identity decides who may
	// rate whom from it and cannot call back for the rest.
	require.Len(t, payload.Participants, 3)
	roles := map[string]int{}
	for _, p := range payload.Participants {
		roles[p.Role]++
		require.NotEqual(t, uuid.Nil, p.UserID)
		require.NotEmpty(t, p.JoinedAt)
	}
	require.Equal(t, map[string]int{domain.RoleOrganizer: 1, domain.RoleParticipant: 2}, roles)

	// The rating window is absolute on the wire so that identity never has to
	// share a duration constant with this service.
	completedAt, err := time.Parse(time.RFC3339, payload.CompletedAt)
	require.NoError(t, err)
	closesAt, err := time.Parse(time.RFC3339, payload.RatingWindowClosesAt)
	require.NoError(t, err)
	require.Equal(t, domain.RatingWindow, closesAt.Sub(completedAt))

	// The transition itself is on the bus too, from the same function the
	// organizer's own publish goes through.
	changes := outboxPayloads(t, pool, tripID, events.TypeTripStatusChanged)
	var last events.TripStatusChanged
	require.NoError(t, json.Unmarshal(changes[len(changes)-1], &last))
	require.Equal(t, string(domain.StatusInProgress), last.OldStatus)
	require.Equal(t, string(domain.StatusCompleted), last.NewStatus)
}

// TestTickTwiceEmitsOneCompletedEvent is the second acceptance criterion. The
// second pass finds nothing — `completed` no longer matches the claim query —
// and the outbox is unchanged.
func TestTickTwiceEmitsOneCompletedEvent(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	tripID := finishedTrip(t, s, pool, uuid.New(), 1)

	first, err := s.LifecycleTick(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, first.Completed)

	second, err := s.LifecycleTick(ctx, 100)
	require.NoError(t, err)
	require.Zero(t, second.Started)
	require.Zero(t, second.Completed)
	require.Zero(t, second.CompletedEvents)

	require.Len(t, outboxPayloads(t, pool, tripID, events.TypeTripCompleted), 1)
}

// TestCompletedEventEmittedGuardsOnItsOwn isolates the column from the status
// check.
//
// The two guards overlap in production — `completed` is terminal, so the claim
// query cannot return the same trip twice — which is exactly why the column
// needs a test that removes the other guard. A trip already marked as having
// emitted its event completes normally and emits nothing, which is what makes a
// replay, a backfill or a hand-run of the scheduler safe.
func TestCompletedEventEmittedGuardsOnItsOwn(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	tripID := finishedTrip(t, s, pool, uuid.New(), 1)
	_, err := pool.Exec(ctx, `UPDATE trips SET completed_event_emitted = true WHERE id = $1`, tripID)
	require.NoError(t, err)

	result, err := s.LifecycleTick(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, result.Completed, "the trip still completes")
	require.Zero(t, result.CompletedEvents, "but its event is not emitted twice")

	require.Equal(t, domain.StatusCompleted, statusOf(t, pool, tripID))
	require.Empty(t, outboxPayloads(t, pool, tripID, events.TypeTripCompleted))
	// The transition is still announced: the trip did move.
	require.NotEmpty(t, outboxPayloads(t, pool, tripID, events.TypeTripStatusChanged))
}

// TestASoloTripCompletesWithoutARosterEvent: a trip nobody joined still
// finishes, but opens no rating window. There is nobody to rate, and a mail
// asking one person to rate themselves is a mail that wastes an afternoon.
func TestASoloTripCompletesWithoutARosterEvent(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	tripID := finishedTrip(t, s, pool, uuid.New(), 0)

	result, err := s.LifecycleTick(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, result.Completed)
	require.Zero(t, result.CompletedEvents)

	require.Equal(t, domain.StatusCompleted, statusOf(t, pool, tripID))
	require.Empty(t, outboxPayloads(t, pool, tripID, events.TypeTripCompleted))

	// And the column stays false, because no event was emitted. It records what
	// happened, not what was considered.
	var emitted bool
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT completed_event_emitted FROM trips WHERE id = $1`, tripID).Scan(&emitted))
	require.False(t, emitted)
}

// TestTickStartsDueTrips covers the other edge: recruiting -> in_progress once
// start_at has passed.
func TestTickStartsDueTrips(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	detail, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)
	tripID := detail.Trip.ID
	_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusRecruiting)
	require.NoError(t, err)

	// Under way now, finishing tomorrow.
	testsupport.Rewind(t, pool, tripID, now.Add(-time.Hour), now.Add(24*time.Hour))

	result, err := s.LifecycleTick(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, result.Started)
	require.Zero(t, result.Completed)
	require.Equal(t, domain.StatusInProgress, statusOf(t, pool, tripID))

	changes := outboxPayloads(t, pool, tripID, events.TypeTripStatusChanged)
	var last events.TripStatusChanged
	require.NoError(t, json.Unmarshal(changes[len(changes)-1], &last))
	require.Equal(t, string(domain.StatusRecruiting), last.OldStatus)
	require.Equal(t, string(domain.StatusInProgress), last.NewStatus)
	require.Equal(t, organizer, last.OrganizerID)
}

// TestOneTickCatchesUpABacklog: a trip whose start *and* end are both in the
// past is fully caught up by a single pass. This is the reason the two passes
// run in that order, and the reason they are two passes rather than one query.
func TestOneTickCatchesUpABacklog(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	detail, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)
	tripID := detail.Trip.ID
	_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusRecruiting)
	require.NoError(t, err)

	joiner := uuid.New()
	request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)
	_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
	require.NoError(t, err)

	// The service was down for the whole trip.
	testsupport.Rewind(t, pool, tripID, now.Add(-72*time.Hour), now.Add(-48*time.Hour))

	result, err := s.LifecycleTick(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 1, result.Started)
	require.Equal(t, 1, result.Completed)
	require.Equal(t, 1, result.CompletedEvents)
	require.Equal(t, domain.StatusCompleted, statusOf(t, pool, tripID))
}

// TestTickLeavesTripsThatAreNotDueAlone. The claim queries are the only thing
// deciding what moves, so a test that only ever seeded due trips would pass
// against a scheduler that transitioned everything it could see.
func TestTickLeavesTripsThatAreNotDueAlone(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	// A draft: no edge from draft is the scheduler's to take.
	draft, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)

	// A recruiting trip that has not left yet.
	future, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)
	_, err = s.ChangeStatus(ctx, future.Trip.ID, organizer, domain.StatusRecruiting)
	require.NoError(t, err)

	// A cancelled one whose dates have long gone by.
	cancelled, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)
	_, err = s.ChangeStatus(ctx, cancelled.Trip.ID, organizer, domain.StatusCancelled)
	require.NoError(t, err)
	testsupport.Rewind(t, pool, cancelled.Trip.ID, now.Add(-72*time.Hour), now.Add(-48*time.Hour))

	result, err := s.LifecycleTick(ctx, 100)
	require.NoError(t, err)
	require.Zero(t, result.Started)
	require.Zero(t, result.Completed)

	require.Equal(t, domain.StatusDraft, statusOf(t, pool, draft.Trip.ID))
	require.Equal(t, domain.StatusRecruiting, statusOf(t, pool, future.Trip.ID))
	require.Equal(t, domain.StatusCancelled, statusOf(t, pool, cancelled.Trip.ID))
}

// TestConcurrentTicksDoNotDoubleEmit is the SKIP LOCKED claim, made against two
// goroutines rather than against the comment above the query.
//
// Both passes run at once over the same due trips. Between them every trip is
// completed exactly once and every roster event written exactly once — which is
// what makes running more than one trip replica safe without a leader election.
func TestConcurrentTicksDoNotDoubleEmit(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	const trips = 6
	ids := make([]uuid.UUID, trips)
	for i := range ids {
		ids[i] = finishedTrip(t, s, pool, uuid.New(), 1)
	}

	var wg sync.WaitGroup
	results := make([]domain.LifecycleResult, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.LifecycleTick(ctx, trips)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, trips, results[0].Completed+results[1].Completed,
		"between them the two passes complete every trip exactly once")

	for _, id := range ids {
		require.Equal(t, domain.StatusCompleted, statusOf(t, pool, id))
		require.Len(t, outboxPayloads(t, pool, id, events.TypeTripCompleted), 1)
	}
}

// TestTickRefusesANonPositiveBatch. A batch of zero would claim nothing and
// report success forever, which is the most boring way for a scheduler to be
// broken.
func TestTickRefusesANonPositiveBatch(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.LifecycleTick(context.Background(), 0)
	require.Error(t, err)
}
