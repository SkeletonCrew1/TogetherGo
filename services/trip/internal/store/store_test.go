package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/testsupport"
)

// The clock every test validates against, so "start_at must be in the future"
// means the same thing on every machine and in a year's time.
var now = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func newStore(t *testing.T) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := testsupport.Pool(t)
	return store.New(pool).WithClock(func() time.Time { return now }), pool
}

func validInput() domain.TripInput {
	return domain.TripInput{
		Title:       "Carpathians in autumn",
		Description: ptr("Three days on the Chornohora ridge."),
		Category:    "hiking",
		Capacity:    8,
		StartAt:     now.Add(14 * 24 * time.Hour),
		EndAt:       now.Add(17 * 24 * time.Hour),
		Points: []domain.PointInput{
			{Name: "Lviv", Lat: 49.8397, Lng: 24.0297},
			{Name: "Vorokhta", Lat: 48.2833, Lng: 24.5667, Transport: ptr("train"), ArriveAt: ptr(now.Add(14*24*time.Hour + 6*time.Hour))},
			{Name: "Hoverla", Lat: 48.1600, Lng: 24.5000},
		},
	}
}

func TestCreateTrip(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	detail, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)

	trip := detail.Trip
	require.NotEqual(t, uuid.Nil, trip.ID)
	require.Equal(t, organizer, trip.OrganizerID)
	require.Equal(t, "Carpathians in autumn", trip.Title)
	require.Equal(t, "hiking", trip.Category)
	require.Equal(t, 8, trip.Capacity)

	// A new trip is always a draft: publishing is a separate, explicit act.
	require.Equal(t, domain.StatusDraft, trip.Status)

	// The organizer occupies a seat from the moment the trip exists.
	require.Equal(t, 1, trip.ApprovedCount)
	require.Equal(t, 7, trip.SpotsLeft())
	require.Len(t, detail.Participants, 1)
	require.Equal(t, organizer, detail.Participants[0].UserID)
	require.Equal(t, domain.RoleOrganizer, detail.Participants[0].Role)

	require.False(t, trip.CreatedAt.IsZero())
	require.False(t, trip.UpdatedAt.IsZero())
}

func TestCreateTripStoresTheRouteInOrder(t *testing.T) {
	s, _ := newStore(t)

	detail, err := s.CreateTrip(context.Background(), uuid.New(), validInput())
	require.NoError(t, err)

	require.Len(t, detail.Points, 3)
	for i, p := range detail.Points {
		require.Equal(t, i, p.Seq, "seq must be dense and zero-based")
		require.NotEqual(t, uuid.Nil, p.ID)
	}
	require.Equal(t, []string{"Lviv", "Vorokhta", "Hoverla"},
		[]string{detail.Points[0].Name, detail.Points[1].Name, detail.Points[2].Name})

	require.InDelta(t, 49.8397, detail.Points[0].Lat, 1e-9)
	require.InDelta(t, 24.0297, detail.Points[0].Lng, 1e-9)

	require.Equal(t, "train", *detail.Points[1].Transport)
	require.NotNil(t, detail.Points[1].ArriveAt)
	require.Nil(t, detail.Points[0].Transport)
	require.Nil(t, detail.Points[0].ArriveAt)
}

// TestCreateTripDenormalisesTheEndpoints is the invariant the migration's
// comment promises: the two geography columns on `trips` are copies of the
// first and last trip_point, written in the same transaction.
func TestCreateTripDenormalisesTheEndpoints(t *testing.T) {
	s, pool := newStore(t)

	detail, err := s.CreateTrip(context.Background(), uuid.New(), validInput())
	require.NoError(t, err)

	require.InDelta(t, 49.8397, detail.Trip.Departure.Lat, 1e-9)
	require.InDelta(t, 24.0297, detail.Trip.Departure.Lng, 1e-9)
	require.InDelta(t, 48.16, detail.Trip.Destination.Lat, 1e-9)
	require.InDelta(t, 24.50, detail.Trip.Destination.Lng, 1e-9)

	requireEndpointsMatchRoute(t, pool, detail.Trip.ID)
}

// requireEndpointsMatchRoute asserts the denormalised columns against the route
// in SQL, so the check is on what Postgres holds rather than on what the Go
// code just returned.
func requireEndpointsMatchRoute(t *testing.T, pool *pgxpool.Pool, tripID uuid.UUID) {
	t.Helper()

	var departureMatches, destinationMatches bool
	err := pool.QueryRow(context.Background(), `
		SELECT
			ST_Equals(t.departure_location::geometry,
			          (SELECT p.location::geometry FROM trip_points p
			            WHERE p.trip_id = t.id ORDER BY p.seq ASC LIMIT 1)),
			ST_Equals(t.destination_location::geometry,
			          (SELECT p.location::geometry FROM trip_points p
			            WHERE p.trip_id = t.id ORDER BY p.seq DESC LIMIT 1))
		FROM trips t WHERE t.id = $1`, tripID).Scan(&departureMatches, &destinationMatches)
	require.NoError(t, err)

	require.True(t, departureMatches, "departure_location has drifted from the first point")
	require.True(t, destinationMatches, "destination_location has drifted from the last point")
}

// TestCreateTripRejectsAOnePointRoute is the acceptance criterion, at the store
// level: the failure is a validation error naming a field path.
func TestCreateTripRejectsAOnePointRoute(t *testing.T) {
	s, pool := newStore(t)

	in := validInput()
	in.Points = in.Points[:1]

	_, err := s.CreateTrip(context.Background(), uuid.New(), in)

	var validation *domain.ValidationError
	require.ErrorAs(t, err, &validation)
	require.Equal(t, "points", validation.Fields[0].Field)

	// Nothing was written: validation runs before the transaction opens.
	var count int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM trips`).Scan(&count))
	require.Zero(t, count)
}

func TestCreateTripRollsBackEverythingOnFailure(t *testing.T) {
	s, pool := newStore(t)

	in := validInput()
	// Past the DB's capacity check but not the domain's, so the failure lands
	// mid-transaction rather than before it — which is what makes this a test
	// of the rollback rather than of the validator.
	in.Capacity = domain.CapacityMax + 1

	_, err := s.CreateTrip(context.Background(), uuid.New(), in)
	require.Error(t, err)

	for _, table := range []string{"trips", "trip_points", "participants"} {
		var count int
		require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&count))
		require.Zero(t, count, "%s should be empty after a failed create", table)
	}
}

func TestTripDetailOnAnUnknownID(t *testing.T) {
	s, _ := newStore(t)

	_, err := s.TripDetail(context.Background(), uuid.New())
	require.ErrorIs(t, err, domain.ErrTripNotFound)
}

func TestChangeStatusFollowsTheStateMachine(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	created, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)
	id := created.Trip.ID

	published, err := s.ChangeStatus(ctx, id, organizer, domain.StatusRecruiting)
	require.NoError(t, err)
	require.Equal(t, domain.StatusRecruiting, published.Trip.Status)
	require.True(t, published.Trip.UpdatedAt.After(created.Trip.UpdatedAt) ||
		published.Trip.UpdatedAt.Equal(created.Trip.UpdatedAt))

	started, err := s.ChangeStatus(ctx, id, organizer, domain.StatusInProgress)
	require.NoError(t, err)
	require.Equal(t, domain.StatusInProgress, started.Trip.Status)

	finished, err := s.ChangeStatus(ctx, id, organizer, domain.StatusCompleted)
	require.NoError(t, err)
	require.Equal(t, domain.StatusCompleted, finished.Trip.Status)
}

// TestChangeStatusRefusesAnIllegalTransition: the store consults the same map
// the unit tests exercise, and reports the refusal richly enough for the
// response to say what would have been allowed.
func TestChangeStatusRefusesAnIllegalTransition(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	created, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)
	id := created.Trip.ID

	_, err = s.ChangeStatus(ctx, id, organizer, domain.StatusCancelled)
	require.NoError(t, err)

	// Publishing a cancelled trip: the acceptance criterion, at the store level.
	_, err = s.ChangeStatus(ctx, id, organizer, domain.StatusRecruiting)
	require.ErrorIs(t, err, domain.ErrInvalidTransition)

	var transition *domain.TransitionError
	require.ErrorAs(t, err, &transition)
	require.Equal(t, domain.StatusCancelled, transition.From)
	require.Equal(t, domain.StatusRecruiting, transition.To)
	require.Empty(t, transition.Allowed())

	// And the row is untouched.
	after, err := s.TripDetail(ctx, id)
	require.NoError(t, err)
	require.Equal(t, domain.StatusCancelled, after.Trip.Status)
}

func TestChangeStatusAuthorisation(t *testing.T) {
	ctx := context.Background()

	t.Run("a stranger cannot publish someone else's draft, and is told it does not exist", func(t *testing.T) {
		s, _ := newStore(t)
		organizer, stranger := uuid.New(), uuid.New()

		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		_, err = s.ChangeStatus(ctx, created.Trip.ID, stranger, domain.StatusRecruiting)
		require.ErrorIs(t, err, domain.ErrTripNotFound,
			"a draft must not confirm its own existence to a stranger")
	})

	t.Run("a stranger cannot cancel a published trip, and is told so", func(t *testing.T) {
		s, _ := newStore(t)
		organizer, stranger := uuid.New(), uuid.New()

		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)
		_, err = s.ChangeStatus(ctx, created.Trip.ID, organizer, domain.StatusRecruiting)
		require.NoError(t, err)

		_, err = s.ChangeStatus(ctx, created.Trip.ID, stranger, domain.StatusCancelled)
		require.ErrorIs(t, err, domain.ErrNotOrganizer,
			"a recruiting trip is public, so the honest 403 leaks nothing")
	})
}

func TestUpdateTrip(t *testing.T) {
	ctx := context.Background()

	t.Run("omitted fields are left alone", func(t *testing.T) {
		s, _ := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		updated, err := s.UpdateTrip(ctx, created.Trip.ID, organizer,
			domain.TripPatch{Title: ptr("Chornohora ridge, three days")})
		require.NoError(t, err)

		require.Equal(t, "Chornohora ridge, three days", updated.Trip.Title)
		require.Equal(t, created.Trip.Description, updated.Trip.Description)
		require.Equal(t, created.Trip.Capacity, updated.Trip.Capacity)
		require.Equal(t, created.Trip.StartAt, updated.Trip.StartAt)
		require.Len(t, updated.Points, 3)
	})

	t.Run("an explicit null clears the description", func(t *testing.T) {
		s, _ := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		updated, err := s.UpdateTrip(ctx, created.Trip.ID, organizer,
			domain.TripPatch{DescriptionSet: true, Description: nil})
		require.NoError(t, err)
		require.Nil(t, updated.Trip.Description)
	})

	t.Run("replacing the route resyncs the denormalised endpoints", func(t *testing.T) {
		s, pool := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		updated, err := s.UpdateTrip(ctx, created.Trip.ID, organizer, domain.TripPatch{
			Points: []domain.PointInput{
				{Name: "Kyiv", Lat: 50.4501, Lng: 30.5234},
				{Name: "Kaniv", Lat: 49.7517, Lng: 31.4667},
				{Name: "Odesa", Lat: 46.4825, Lng: 30.7233},
			},
		})
		require.NoError(t, err)

		require.Len(t, updated.Points, 3)
		require.Equal(t, "Kyiv", updated.Points[0].Name)
		require.Equal(t, "Odesa", updated.Points[2].Name)
		require.InDelta(t, 50.4501, updated.Trip.Departure.Lat, 1e-9)
		require.InDelta(t, 46.4825, updated.Trip.Destination.Lat, 1e-9)

		requireEndpointsMatchRoute(t, pool, created.Trip.ID)

		// The old route is gone, not appended to: seq stays dense.
		var count int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM trip_points WHERE trip_id = $1`, created.Trip.ID).Scan(&count))
		require.Equal(t, 3, count)
	})

	t.Run("a shorter route resyncs too", func(t *testing.T) {
		s, pool := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		_, err = s.UpdateTrip(ctx, created.Trip.ID, organizer, domain.TripPatch{
			Points: []domain.PointInput{
				{Name: "Lviv", Lat: 49.8397, Lng: 24.0297},
				{Name: "Uzhhorod", Lat: 48.6208, Lng: 22.2879},
			},
		})
		require.NoError(t, err)
		requireEndpointsMatchRoute(t, pool, created.Trip.ID)
	})

	t.Run("the merged result is validated, not the patch", func(t *testing.T) {
		s, _ := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		// end_at alone, moved before the stored start_at.
		_, err = s.UpdateTrip(ctx, created.Trip.ID, organizer,
			domain.TripPatch{EndAt: ptr(created.Trip.StartAt.Add(-time.Hour))})

		var validation *domain.ValidationError
		require.ErrorAs(t, err, &validation)
		require.Equal(t, "end_at", validation.Fields[0].Field)
	})

	t.Run("editing is allowed while recruiting", func(t *testing.T) {
		s, _ := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)
		_, err = s.ChangeStatus(ctx, created.Trip.ID, organizer, domain.StatusRecruiting)
		require.NoError(t, err)

		updated, err := s.UpdateTrip(ctx, created.Trip.ID, organizer, domain.TripPatch{Capacity: ptr(12)})
		require.NoError(t, err)
		require.Equal(t, 12, updated.Trip.Capacity)
	})

	t.Run("editing is refused once the trip is under way", func(t *testing.T) {
		s, _ := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)
		for _, to := range []domain.Status{domain.StatusRecruiting, domain.StatusInProgress} {
			_, err = s.ChangeStatus(ctx, created.Trip.ID, organizer, to)
			require.NoError(t, err)
		}

		_, err = s.UpdateTrip(ctx, created.Trip.ID, organizer, domain.TripPatch{Title: ptr("too late")})
		require.ErrorIs(t, err, domain.ErrNotEditable)
	})

	t.Run("editing is refused once the trip is cancelled", func(t *testing.T) {
		s, _ := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)
		_, err = s.ChangeStatus(ctx, created.Trip.ID, organizer, domain.StatusCancelled)
		require.NoError(t, err)

		_, err = s.UpdateTrip(ctx, created.Trip.ID, organizer, domain.TripPatch{Title: ptr("too late")})
		require.ErrorIs(t, err, domain.ErrNotEditable)
	})

	t.Run("a stranger editing a draft is told it does not exist", func(t *testing.T) {
		s, _ := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		_, err = s.UpdateTrip(ctx, created.Trip.ID, uuid.New(), domain.TripPatch{Title: ptr("mine now")})
		require.ErrorIs(t, err, domain.ErrTripNotFound)
	})

	t.Run("an unchanged start_at is not re-checked against the clock", func(t *testing.T) {
		s, pool := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		// The trip has since begun. Editing the title must still work.
		// The same database, seen by a store whose clock is past the departure.
		late := store.New(pool).WithClock(func() time.Time { return created.Trip.StartAt.Add(time.Hour) })
		_, err = late.UpdateTrip(ctx, created.Trip.ID, organizer, domain.TripPatch{Title: ptr("a fixed typo")})
		require.NoError(t, err)

		// Moving start_at, though, is held to the rule.
		_, err = late.UpdateTrip(ctx, created.Trip.ID, organizer,
			domain.TripPatch{StartAt: ptr(created.Trip.StartAt)})
		var validation *domain.ValidationError
		require.ErrorAs(t, err, &validation)
		require.Equal(t, "start_at", validation.Fields[0].Field)
	})
}

// TestUpdateTripRefusesToShrinkBelowTheRoster: capacity is bounded below by the
// people already on the trip, not just by CapacityMin.
func TestUpdateTripRefusesToShrinkBelowTheRoster(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	created, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)

	// Join requests arrive in a later stage, so the roster is filled here
	// directly — this is a store test, and what is under test is the guard, not
	// the approval flow.
	addApprovedParticipants(t, pool, created.Trip.ID, 4)

	_, err = s.UpdateTrip(ctx, created.Trip.ID, organizer, domain.TripPatch{Capacity: ptr(3)})
	require.ErrorIs(t, err, domain.ErrCapacityBelowApproved)

	// Exactly the number already approved is fine: it closes the trip, it does
	// not overbook it.
	updated, err := s.UpdateTrip(ctx, created.Trip.ID, organizer, domain.TripPatch{Capacity: ptr(5)})
	require.NoError(t, err)
	require.Equal(t, 5, updated.Trip.Capacity)
	require.Zero(t, updated.Trip.SpotsLeft())
}

func addApprovedParticipants(t *testing.T, pool *pgxpool.Pool, tripID uuid.UUID, n int) {
	t.Helper()
	ctx := context.Background()

	for i := 0; i < n; i++ {
		_, err := pool.Exec(ctx,
			`INSERT INTO participants (trip_id, user_id, role) VALUES ($1, $2, $3)`,
			tripID, uuid.New(), domain.RoleParticipant)
		require.NoError(t, err)
	}
	_, err := pool.Exec(ctx,
		`UPDATE trips SET approved_count = approved_count + $2 WHERE id = $1`, tripID, n)
	require.NoError(t, err)
}

func TestDeleteTrip(t *testing.T) {
	ctx := context.Background()

	t.Run("a draft is hard-deleted, route and roster with it", func(t *testing.T) {
		s, pool := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)

		require.NoError(t, s.DeleteTrip(ctx, created.Trip.ID, organizer))

		_, err = s.TripDetail(ctx, created.Trip.ID)
		require.ErrorIs(t, err, domain.ErrTripNotFound)

		for _, table := range []string{"trip_points", "participants"} {
			var count int
			require.NoError(t, pool.QueryRow(ctx,
				`SELECT count(*) FROM `+table+` WHERE trip_id = $1`, created.Trip.ID).Scan(&count))
			require.Zero(t, count, "%s should have been cascaded away", table)
		}
	})

	t.Run("a published trip is cancelled, never erased", func(t *testing.T) {
		s, _ := newStore(t)
		organizer := uuid.New()
		created, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)
		_, err = s.ChangeStatus(ctx, created.Trip.ID, organizer, domain.StatusRecruiting)
		require.NoError(t, err)

		require.ErrorIs(t, s.DeleteTrip(ctx, created.Trip.ID, organizer), domain.ErrNotDraft)

		_, err = s.TripDetail(ctx, created.Trip.ID)
		require.NoError(t, err, "the trip is still there")
	})

	t.Run("a stranger deleting a draft is told it does not exist", func(t *testing.T) {
		s, _ := newStore(t)
		created, err := s.CreateTrip(ctx, uuid.New(), validInput())
		require.NoError(t, err)

		require.ErrorIs(t, s.DeleteTrip(ctx, created.Trip.ID, uuid.New()), domain.ErrTripNotFound)
	})

	t.Run("deleting an unknown trip", func(t *testing.T) {
		s, _ := newStore(t)
		require.ErrorIs(t, s.DeleteTrip(ctx, uuid.New(), uuid.New()), domain.ErrTripNotFound)
	})
}

// TestTripsAreIsolatedFromEachOther guards against a WHERE clause that forgot
// its trip_id: two trips' routes and rosters must not bleed into one another.
func TestTripsAreIsolatedFromEachOther(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	first, err := s.CreateTrip(ctx, uuid.New(), validInput())
	require.NoError(t, err)

	other := validInput()
	other.Title = "A different trip"
	other.Points = []domain.PointInput{
		{Name: "Kyiv", Lat: 50.4501, Lng: 30.5234},
		{Name: "Odesa", Lat: 46.4825, Lng: 30.7233},
	}
	second, err := s.CreateTrip(ctx, uuid.New(), other)
	require.NoError(t, err)

	require.Len(t, first.Points, 3)
	require.Len(t, second.Points, 2)
	require.Len(t, first.Participants, 1)
	require.Len(t, second.Participants, 1)

	// Replacing one route must leave the other alone.
	_, err = s.UpdateTrip(ctx, second.Trip.ID, second.Trip.OrganizerID, domain.TripPatch{
		Points: []domain.PointInput{
			{Name: "Kharkiv", Lat: 49.9935, Lng: 36.2304},
			{Name: "Poltava", Lat: 49.5883, Lng: 34.5514},
		},
	})
	require.NoError(t, err)

	reread, err := s.TripDetail(ctx, first.Trip.ID)
	require.NoError(t, err)
	require.Len(t, reread.Points, 3)
	require.Equal(t, "Lviv", reread.Points[0].Name)
}

// TestTimestampsComeBackInUTC: coordinates and instants both round-trip, and an
// arrive_at written as UTC must not come back shifted.
func TestTimestampsRoundTrip(t *testing.T) {
	s, _ := newStore(t)

	in := validInput()
	arrival := now.Add(15 * 24 * time.Hour)
	in.Points[1].ArriveAt = &arrival

	detail, err := s.CreateTrip(context.Background(), uuid.New(), in)
	require.NoError(t, err)

	require.True(t, detail.Points[1].ArriveAt.Equal(arrival))
	require.True(t, detail.Trip.StartAt.Equal(in.StartAt))
	require.True(t, detail.Trip.EndAt.Equal(in.EndAt))
}
