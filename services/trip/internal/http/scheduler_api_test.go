package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/auth"
	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/events"
	httpapi "github.com/togethergo/trip/internal/http"
	"github.com/togethergo/trip/internal/scheduler"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/testsupport"
)

// POST /internal/scheduler/tick.
//
// The endpoint exists so a test does not have to sleep out a tick interval, and
// these are the tests that would have had to. What they drive is the real
// router over a real database with a real scheduler behind it — the same Tick
// the sixty-second loop calls.

const testInternalToken = "test-internal-token"

// schedulerAPI is the api harness plus the /internal group.
type schedulerAPI struct {
	*api
	pool  *pgxpool.Pool
	store *store.Store
	now   time.Time
}

func newSchedulerAPI(t *testing.T) *schedulerAPI {
	t.Helper()

	pool := testsupport.Pool(t)
	identity := testsupport.NewIdentity(t)
	clock := time.Now().UTC()
	st := store.New(pool).WithClock(func() time.Time { return clock })

	handler := httpapi.NewRouter(httpapi.Deps{
		Store:    st,
		Verifier: auth.NewVerifier(auth.NewKeySet(identity.JWKSURL, time.Minute, nil)),
		Logger:   httpapi.NewLogger("error"),
		Scheduler: scheduler.New(scheduler.Options{
			Lifecycle: st,
			Logger:    httpapi.NewLogger("error"),
		}),
		InternalToken: testInternalToken,
	})

	return &schedulerAPI{
		api:   &api{t: t, handler: handler, identity: identity},
		pool:  pool,
		store: st,
		now:   clock,
	}
}

// tick calls the debug endpoint with the shared secret, as another service or
// an operator on the Compose network would.
func (s *schedulerAPI) tick(token string) *httptest.ResponseRecorder {
	s.t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/internal/scheduler/tick", nil)
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	recorder := httptest.NewRecorder()
	s.handler.ServeHTTP(recorder, req)
	return recorder
}

// finishedTrip seeds a trip that ran yesterday, with one participant besides
// its organizer, through the real store.
func (s *schedulerAPI) finishedTrip(organizer uuid.UUID) uuid.UUID {
	s.t.Helper()
	ctx := context.Background()

	detail, err := s.store.CreateTrip(ctx, organizer, domain.TripInput{
		Title:    "Carpathians in autumn",
		Category: "hiking",
		Capacity: 8,
		StartAt:  s.now.Add(14 * 24 * time.Hour),
		EndAt:    s.now.Add(17 * 24 * time.Hour),
		Points: []domain.PointInput{
			{Name: "Lviv", Lat: 49.8397, Lng: 24.0297},
			{Name: "Hoverla", Lat: 48.1600, Lng: 24.5000},
		},
	})
	require.NoError(s.t, err)
	tripID := detail.Trip.ID

	_, err = s.store.ChangeStatus(ctx, tripID, organizer, domain.StatusRecruiting)
	require.NoError(s.t, err)

	request, err := s.store.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(s.t, err)
	_, err = s.store.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
	require.NoError(s.t, err)

	_, err = s.store.ChangeStatus(ctx, tripID, organizer, domain.StatusInProgress)
	require.NoError(s.t, err)

	testsupport.Rewind(s.t, s.pool, tripID, s.now.Add(-26*time.Hour), s.now.Add(-time.Hour))
	return tripID
}

func (s *schedulerAPI) countOutbox(tripID uuid.UUID, eventType string) int {
	s.t.Helper()

	var count int
	require.NoError(s.t, s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE aggregate_id = $1 AND event_type = $2`,
		tripID, eventType).Scan(&count))
	return count
}

// TestDebugTickCompletesADueTrip is the first acceptance criterion driven
// through the endpoint rather than through the store: seed a trip whose end_at
// is in the past, call the tick, and find it completed with exactly one
// trip.completed row in the outbox.
func TestDebugTickCompletesADueTrip(t *testing.T) {
	api := newSchedulerAPI(t)
	tripID := api.finishedTrip(uuid.New())

	response := api.tick(testInternalToken)
	require.Equal(t, http.StatusOK, response.Code)

	var body struct {
		Started         int `json:"started"`
		Completed       int `json:"completed"`
		CompletedEvents int `json:"completed_events"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, 1, body.Completed)
	require.Equal(t, 1, body.CompletedEvents)

	var status domain.Status
	require.NoError(t, api.pool.QueryRow(context.Background(),
		`SELECT status FROM trips WHERE id = $1`, tripID).Scan(&status))
	require.Equal(t, domain.StatusCompleted, status)

	require.Equal(t, 1, api.countOutbox(tripID, events.TypeTripCompleted))
}

// TestDebugTickTwiceEmitsOneCompletedEvent is the second acceptance criterion,
// through the endpoint. The second call is a no-op because the trip is no
// longer claimable, and the outbox proves it.
func TestDebugTickTwiceEmitsOneCompletedEvent(t *testing.T) {
	api := newSchedulerAPI(t)
	tripID := api.finishedTrip(uuid.New())

	require.Equal(t, http.StatusOK, api.tick(testInternalToken).Code)

	second := api.tick(testInternalToken)
	require.Equal(t, http.StatusOK, second.Code)
	require.JSONEq(t, `{"started":0,"completed":0,"completed_events":0}`, second.Body.String())

	require.Equal(t, 1, api.countOutbox(tripID, events.TypeTripCompleted))
}

// TestDebugTickRequiresTheInternalToken. /internal is not routed at the
// gateway, but in Compose every container shares one network with every other,
// so "not routed" is a statement about Traefik rather than about who can open a
// socket to port 8002.
//
// The answer is 404 rather than 401 on purpose: a caller who does not hold the
// secret should not learn the endpoint is there.
func TestDebugTickRequiresTheInternalToken(t *testing.T) {
	api := newSchedulerAPI(t)
	tripID := api.finishedTrip(uuid.New())

	for _, token := range []string{"", "wrong-token"} {
		response := api.tick(token)
		require.Equal(t, http.StatusNotFound, response.Code)
		require.Contains(t, response.Body.String(), `"not_found"`)
	}

	require.Zero(t, api.countOutbox(tripID, events.TypeTripCompleted),
		"a refused tick transitions nothing")
}

// TestTheDebugTickIsNotMountedWithoutASchedulerRoute. In production
// TRIP_SCHEDULER_DEBUG_TICK is off, main passes no scheduler, and the route
// must not exist at all — an endpoint that advances trips on demand is a lever
// nobody needs and somebody will eventually pull.
func TestTheDebugTickIsNotMountedWithoutAScheduler(t *testing.T) {
	pool := testsupport.Pool(t)
	identity := testsupport.NewIdentity(t)

	handler := httpapi.NewRouter(httpapi.Deps{
		Store:         store.New(pool),
		Verifier:      auth.NewVerifier(auth.NewKeySet(identity.JWKSURL, time.Minute, nil)),
		Logger:        httpapi.NewLogger("error"),
		InternalToken: testInternalToken,
	})

	req := httptest.NewRequest(http.MethodPost, "/internal/scheduler/tick", nil)
	req.Header.Set("X-Internal-Token", testInternalToken)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusNotFound, recorder.Code)
}
