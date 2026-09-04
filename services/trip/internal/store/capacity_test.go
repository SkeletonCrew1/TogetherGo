package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
)

// TestConcurrentApprovalsRespectCapacity is the acceptance criterion for the
// capacity invariant, and the reason Store.ApproveJoinRequest opens with
// SELECT ... FOR UPDATE.
//
// A trip with N free seats, N+5 pending requests, and N+5 goroutines released
// at once to approve them. Exactly N must succeed, the other five must fail
// with ErrTripFull, and approved_count must never exceed capacity — not at the
// end, which a compensating decrement could also achieve, but at no point,
// which the trips_approved_fits CHECK constraint enforces on every write.
//
// There is no application-level mutex here or in the store, deliberately. A
// mutex would pass this test and fail in production the moment a second replica
// started, because it coordinates goroutines and the thing that needs
// coordinating is transactions. The row lock is the mechanism: every one of
// these transactions asks Postgres for the trip row, Postgres hands it to one
// of them at a time, and each one that gets it re-reads an approved_count that
// already includes its predecessors' commits.
//
// Run it with `-race -count=10`; `make test-trip-race` does.
func TestConcurrentApprovalsRespectCapacity(t *testing.T) {
	const freeSeats = 7
	const contenders = freeSeats + 5

	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	// The organizer occupies a seat from the moment the trip exists, so a trip
	// with `freeSeats` seats left has capacity `freeSeats + 1`.
	tripID := recruitingTrip(t, s, organizer, freeSeats+1)
	require.Equal(t, 1, approvedCount(t, pool, tripID))

	requestIDs := make([]uuid.UUID, 0, contenders)
	for range contenders {
		request, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)
		requestIDs = append(requestIDs, request.ID)
	}

	// A barrier rather than a plain loop of goroutines: without it the first
	// few would be done before the last was scheduled and the test would prove
	// nothing about contention.
	release := make(chan struct{})
	results := make([]error, contenders)

	var wg sync.WaitGroup
	wg.Add(contenders)
	for i, requestID := range requestIDs {
		go func() {
			defer wg.Done()
			<-release
			_, err := s.ApproveJoinRequest(ctx, tripID, requestID, organizer)
			results[i] = err
		}()
	}
	close(release)
	wg.Wait()

	var approved, full int
	for i, err := range results {
		switch {
		case err == nil:
			approved++
		case ErrorIsTripFull(err):
			full++
		default:
			t.Fatalf("approval %d failed with an unexpected error: %v", i, err)
		}
	}

	require.Equal(t, freeSeats, approved, "exactly the free seats were filled")
	require.Equal(t, contenders-freeSeats, full, "everyone else was told the trip is full")

	// The counter, the roster and the requests all agree, and the counter is at
	// capacity rather than over it.
	trip, err := s.Trip(ctx, tripID)
	require.NoError(t, err)
	require.Equal(t, trip.Capacity, trip.ApprovedCount)
	require.LessOrEqual(t, trip.ApprovedCount, trip.Capacity)

	detail, err := s.TripDetail(ctx, tripID)
	require.NoError(t, err)
	require.Len(t, detail.Participants, freeSeats+1, "the organizer plus the approved")

	var approvedRequests int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM join_requests WHERE trip_id = $1 AND status = 'approved'`,
		tripID).Scan(&approvedRequests))
	require.Equal(t, freeSeats, approvedRequests, "no request was approved without a seat")

	var pending int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM join_requests WHERE trip_id = $1 AND status = 'pending'`,
		tripID).Scan(&pending))
	require.Equal(t, contenders-freeSeats, pending, "the losers are still pending and can be rejected")

	// One outbox row per successful approval, and none for the failures: the
	// event and the seat are written in the same transaction, so a rolled-back
	// approval cannot leave an event behind claiming somebody joined.
	var published int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE aggregate_id = $1 AND event_type = 'join_request.approved'`,
		tripID).Scan(&published))
	require.Equal(t, freeSeats, published)
}

// TestConcurrentApprovalsOfOneRequest is the double-click: the same request,
// approved twice at the same moment, on a trip with room for both.
//
// Exactly one must win. The guard is the "still pending" check, made under the
// same row lock — without it both transactions would insert a participant and
// the trip would gain two seats' worth of one person.
func TestConcurrentApprovalsOfOneRequest(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()
	tripID := recruitingTrip(t, s, organizer, 10)

	request, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)

	release := make(chan struct{})
	results := make([]error, 4)

	var wg sync.WaitGroup
	wg.Add(len(results))
	for i := range results {
		go func() {
			defer wg.Done()
			<-release
			_, err := s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
			results[i] = err
		}()
	}
	close(release)
	wg.Wait()

	var won int
	for i, err := range results {
		switch {
		case err == nil:
			won++
		case ErrorIsNotPending(err):
		default:
			t.Fatalf("approval %d failed with an unexpected error: %v", i, err)
		}
	}

	require.Equal(t, 1, won)
	require.Equal(t, 2, approvedCount(t, pool, tripID), "the organizer and one participant")
}

// Small named predicates rather than errors.Is inline, so the switches above
// read as the two or three outcomes each test allows and nothing else.
func ErrorIsTripFull(err error) bool   { return errors.Is(err, domain.ErrTripFull) }
func ErrorIsNotPending(err error) bool { return errors.Is(err, domain.ErrRequestNotPending) }
