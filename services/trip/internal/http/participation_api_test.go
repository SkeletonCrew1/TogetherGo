package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/auth"
	httpapi "github.com/togethergo/trip/internal/http"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/testsupport"
	"github.com/togethergo/trip/internal/users"
)

// The participation endpoints, end to end: the real router over a real
// database, with a real HTTP stand-in for identity's batch resolver. Nothing
// between the request and Postgres is faked — the acceptance criteria are
// statements about status codes and bodies, and a mocked store would only prove
// the mock agreed with itself.

type joinAPI struct {
	*api
	people *peopleStub
}

func newJoinAPI(t *testing.T) *joinAPI {
	t.Helper()

	pool := testsupport.Pool(t)
	identity := testsupport.NewIdentity(t)
	people := newPeopleStub(t)

	return &joinAPI{
		api: &api{
			t:        t,
			identity: identity,
			handler: httpapi.NewRouter(httpapi.Deps{
				Store:    store.New(pool),
				Verifier: auth.NewVerifier(auth.NewKeySet(identity.JWKSURL, time.Minute, nil)),
				Logger:   httpapi.NewLogger("error"),
				Users:    people.client(),
			}),
		},
		people: people,
	}
}

// recruiting creates a trip and publishes it, which is the only state in which
// anybody may ask to join one. Returns its id.
func (a *joinAPI) recruiting(organizer uuid.UUID, capacity int) string {
	a.t.Helper()

	trip := a.createTrip(organizer, func(body map[string]any) {
		body["capacity"] = capacity
	})
	res := a.do(http.MethodPost, tripPath(trip, "/publish"), organizer, nil)
	require.Equal(a.t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())
	return trip["id"].(string)
}

// request asks to join, asserts 201, and returns the created request.
func (a *joinAPI) request(tripID string, user uuid.UUID, message any) map[string]any {
	a.t.Helper()

	var body any
	if message != nil {
		body = map[string]any{"message": message}
	}
	res := a.do(http.MethodPost, "/api/trips/"+tripID+"/requests", user, body)
	require.Equal(a.t, http.StatusCreated, res.Code, "unexpected body: %s", res.Body.String())
	return decode(a.t, res)
}

// join is the whole happy path — ask, approve — for tests about what comes
// after it.
func (a *joinAPI) join(tripID string, organizer, user uuid.UUID) map[string]any {
	a.t.Helper()

	created := a.request(tripID, user, nil)
	res := a.do(http.MethodPost,
		fmt.Sprintf("/api/trips/%s/requests/%s/approve", tripID, created["id"]), organizer, nil)
	require.Equal(a.t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())
	return decode(a.t, res)
}

// --- asking to join ---------------------------------------------------------

func TestCreateJoinRequestOverHTTP(t *testing.T) {
	a := newJoinAPI(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := a.recruiting(organizer, 8)

	res := a.do(http.MethodPost, "/api/trips/"+tripID+"/requests", joiner,
		map[string]any{"message": "I know the ridge."})
	require.Equal(t, http.StatusCreated, res.Code, "unexpected body: %s", res.Body.String())
	require.Equal(t, "/api/trips/"+tripID+"/requests/me", res.Header().Get("Location"))

	body := decode(t, res)
	require.Equal(t, "pending", body["status"])
	require.Equal(t, joiner.String(), body["user_id"])
	require.Equal(t, "I know the ridge.", body["message"])
	require.Nil(t, body["decided_at"])

	// A note is optional, and so is the body: asking to come along without
	// writing anything is normal, and demanding `{}` would be a rule with no
	// reason behind it.
	other := uuid.New()
	res = a.do(http.MethodPost, "/api/trips/"+tripID+"/requests", other, nil)
	require.Equal(t, http.StatusCreated, res.Code, "unexpected body: %s", res.Body.String())
	require.Nil(t, decode(t, res)["message"])
}

// Each way of being refused has its own code, because each has a different
// remedy and a client switches on the code.
func TestJoinRequestRefusalsHaveDistinctCodes(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()

	t.Run("their own trip", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/requests", organizer, nil)
		require.Equal(t, http.StatusConflict, res.Code)
		require.Equal(t, "cannot_join_own_trip", errorOf(t, res)["code"])
	})

	t.Run("a draft, to a stranger, is a 404", func(t *testing.T) {
		trip := a.createTrip(organizer)
		res := a.do(http.MethodPost, tripPath(trip, "/requests"), uuid.New(), nil)
		require.Equal(t, http.StatusNotFound, res.Code)
		require.Equal(t, "trip_not_found", errorOf(t, res)["code"])
	})

	t.Run("a cancelled trip", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		require.Equal(t, http.StatusOK,
			a.do(http.MethodPost, "/api/trips/"+tripID+"/cancel", organizer, nil).Code)

		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/requests", uuid.New(), nil)
		require.Equal(t, http.StatusConflict, res.Code)
		require.Equal(t, "trip_not_recruiting", errorOf(t, res)["code"])
	})

	t.Run("a second open request", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		joiner := uuid.New()
		a.request(tripID, joiner, nil)

		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/requests", joiner, nil)
		require.Equal(t, http.StatusConflict, res.Code)
		require.Equal(t, "request_already_pending", errorOf(t, res)["code"])
	})

	t.Run("a trip they are already on", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		joiner := uuid.New()
		a.join(tripID, organizer, joiner)

		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/requests", joiner, nil)
		require.Equal(t, http.StatusConflict, res.Code)
		require.Equal(t, "already_participant", errorOf(t, res)["code"])
	})

	t.Run("a message over the limit", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		long := make([]byte, 501)
		for i := range long {
			long[i] = 'a'
		}
		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/requests", uuid.New(),
			map[string]any{"message": string(long)})
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Equal(t, []string{"message"}, fieldPaths(t, res))
	})
}

func TestCancelOwnJoinRequestOverHTTP(t *testing.T) {
	a := newJoinAPI(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := a.recruiting(organizer, 8)
	a.request(tripID, joiner, nil)

	res := a.do(http.MethodDelete, "/api/trips/"+tripID+"/requests/me", joiner, nil)
	require.Equal(t, http.StatusNoContent, res.Code)
	require.Empty(t, res.Body.String())

	// There is nothing left to cancel, and saying so is more useful than a
	// second 204 that pretends something happened.
	res = a.do(http.MethodDelete, "/api/trips/"+tripID+"/requests/me", joiner, nil)
	require.Equal(t, http.StatusNotFound, res.Code)
	require.Equal(t, "join_request_not_found", errorOf(t, res)["code"])
}

// --- the organizer's queue --------------------------------------------------

// The acceptance criterion for the queue: pending first, and every row enriched
// with the requester's name, photo and rating.
func TestJoinRequestQueueIsEnrichedAndOrdered(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()
	tripID := a.recruiting(organizer, 20)

	decided := a.request(tripID, uuid.New(), nil)
	res := a.do(http.MethodPost,
		fmt.Sprintf("/api/trips/%s/requests/%s/reject", tripID, decided["id"]), organizer,
		map[string]any{"reason": "Not this time."})
	require.Equal(t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())

	pending := a.request(tripID, uuid.New(), "Bringing a stove.")

	before := a.people.calls.Load()
	res = a.do(http.MethodGet, "/api/trips/"+tripID+"/requests", organizer, nil)
	require.Equal(t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())

	var page struct {
		Items []struct {
			ID        string  `json:"id"`
			Status    string  `json:"status"`
			Message   *string `json:"message"`
			Reason    *string `json:"reason"`
			Requester struct {
				ID          string   `json:"id"`
				FullName    *string  `json:"full_name"`
				PhotoURL    *string  `json:"photo_url"`
				RatingAvg   *float64 `json:"rating_avg"`
				RatingCount *int     `json:"rating_count"`
			} `json:"requester"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &page), "body was: %s", res.Body.String())

	require.Len(t, page.Items, 2)
	require.Nil(t, page.NextCursor)
	require.Equal(t, pending["id"], page.Items[0].ID, "pending first")
	require.Equal(t, "pending", page.Items[0].Status)
	require.Equal(t, "rejected", page.Items[1].Status)
	require.Equal(t, "Not this time.", *page.Items[1].Reason)

	for _, item := range page.Items {
		require.NotNil(t, item.Requester.FullName, "the queue carries the requester's name")
		require.NotNil(t, item.Requester.PhotoURL)
		require.NotNil(t, item.Requester.RatingAvg)
		require.NotNil(t, item.Requester.RatingCount)
	}

	// One call to identity for the whole page, however many rows it has.
	require.Equal(t, int64(1), a.people.calls.Load()-before)
}

// A queue must still be usable when identity is not. The organizer approves an
// id instead of a name; the page does not 500.
func TestJoinRequestQueueSurvivesIdentityBeingDown(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()
	tripID := a.recruiting(organizer, 8)
	a.request(tripID, uuid.New(), nil)

	a.people.down.Store(true)

	res := a.do(http.MethodGet, "/api/trips/"+tripID+"/requests", organizer, nil)
	require.Equal(t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())

	body := decode(t, res)
	items := body["items"].([]any)
	require.Len(t, items, 1)

	requester := items[0].(map[string]any)["requester"].(map[string]any)
	require.NotEmpty(t, requester["id"])
	require.Nil(t, requester["full_name"])
	require.Nil(t, requester["rating_avg"])
}

func TestJoinRequestQueueIsOrganizerOnly(t *testing.T) {
	a := newJoinAPI(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := a.recruiting(organizer, 8)
	a.request(tripID, joiner, nil)

	// Even the requester: who else applied, and what they wrote, is not their
	// business.
	res := a.do(http.MethodGet, "/api/trips/"+tripID+"/requests", joiner, nil)
	require.Equal(t, http.StatusForbidden, res.Code)
	require.Equal(t, "not_organizer", errorOf(t, res)["code"])
}

func TestJoinRequestQueueRejectsABadQuery(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()
	tripID := a.recruiting(organizer, 8)

	res := a.do(http.MethodGet, "/api/trips/"+tripID+"/requests?limit=900&cursor=!!!&colour=blue", organizer, nil)
	require.Equal(t, http.StatusBadRequest, res.Code)
	require.ElementsMatch(t, []string{"limit", "cursor", "colour"}, fieldPaths(t, res))
}

// --- deciding ---------------------------------------------------------------

func TestApproveAndRejectOverHTTP(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()
	tripID := a.recruiting(organizer, 8)

	yes := a.request(tripID, uuid.New(), nil)
	res := a.do(http.MethodPost,
		fmt.Sprintf("/api/trips/%s/requests/%s/approve", tripID, yes["id"]), organizer, nil)
	require.Equal(t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())

	body := decode(t, res)
	require.Equal(t, "approved", body["status"])
	require.Equal(t, organizer.String(), body["decided_by"])
	require.NotNil(t, body["decided_at"])

	// The roster and the counter moved together.
	trip := decode(t, a.do(http.MethodGet, "/api/trips/"+tripID, organizer, nil))
	require.Equal(t, float64(2), trip["approved_count"])
	require.Equal(t, float64(6), trip["spots_left"])
	require.Len(t, trip["participants"].([]any), 2)

	no := a.request(tripID, uuid.New(), nil)
	res = a.do(http.MethodPost,
		fmt.Sprintf("/api/trips/%s/requests/%s/reject", tripID, no["id"]), organizer,
		map[string]any{"reason": "Group is full of navigators."})
	require.Equal(t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())
	require.Equal(t, "rejected", decode(t, res)["status"])

	// Rejecting gives no seat back, because it never took one.
	trip = decode(t, a.do(http.MethodGet, "/api/trips/"+tripID, organizer, nil))
	require.Equal(t, float64(2), trip["approved_count"])
}

// The organizer's sentence is readable by the requester and by nobody else.
func TestTheRequesterCanReadTheRejectionReason(t *testing.T) {
	a := newJoinAPI(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := a.recruiting(organizer, 8)

	created := a.request(tripID, joiner, nil)
	require.Equal(t, http.StatusOK, a.do(http.MethodPost,
		fmt.Sprintf("/api/trips/%s/requests/%s/reject", tripID, created["id"]), organizer,
		map[string]any{"reason": "Taking only people who have done it before."}).Code)

	mine := decode(t, a.do(http.MethodGet, "/api/trips/"+tripID+"/requests/me", joiner, nil))
	require.Equal(t, "rejected", mine["status"])
	require.Equal(t, "Taking only people who have done it before.", mine["reason"])

	// A stranger has no request on this trip, and learns nothing about anyone
	// else's.
	res := a.do(http.MethodGet, "/api/trips/"+tripID+"/requests/me", uuid.New(), nil)
	require.Equal(t, http.StatusNotFound, res.Code)
}

func TestDecidingRefusals(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()

	t.Run("by a stranger", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		created := a.request(tripID, uuid.New(), nil)

		res := a.do(http.MethodPost,
			fmt.Sprintf("/api/trips/%s/requests/%s/approve", tripID, created["id"]), uuid.New(), nil)
		require.Equal(t, http.StatusForbidden, res.Code)
		require.Equal(t, "not_organizer", errorOf(t, res)["code"])
	})

	t.Run("twice", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		created := a.request(tripID, uuid.New(), nil)
		path := fmt.Sprintf("/api/trips/%s/requests/%s/approve", tripID, created["id"])

		require.Equal(t, http.StatusOK, a.do(http.MethodPost, path, organizer, nil).Code)

		res := a.do(http.MethodPost, path, organizer, nil)
		require.Equal(t, http.StatusConflict, res.Code)
		require.Equal(t, "request_not_pending", errorOf(t, res)["code"])
	})

	t.Run("a request id that is not a uuid", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/requests/nonsense/approve", organizer, nil)
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Equal(t, []string{"request_id"}, fieldPaths(t, res))
	})

	t.Run("a full trip", func(t *testing.T) {
		// Capacity 2 is the organizer plus one.
		tripID := a.recruiting(organizer, 2)
		first := a.request(tripID, uuid.New(), nil)
		second := a.request(tripID, uuid.New(), nil)

		require.Equal(t, http.StatusOK, a.do(http.MethodPost,
			fmt.Sprintf("/api/trips/%s/requests/%s/approve", tripID, first["id"]), organizer, nil).Code)

		res := a.do(http.MethodPost,
			fmt.Sprintf("/api/trips/%s/requests/%s/approve", tripID, second["id"]), organizer, nil)
		require.Equal(t, http.StatusConflict, res.Code)
		require.Equal(t, "trip_full", errorOf(t, res)["code"])
	})
}

// --- leaving and being removed ---------------------------------------------

func TestRemoveParticipantOverHTTP(t *testing.T) {
	a := newJoinAPI(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := a.recruiting(organizer, 8)
	a.join(tripID, organizer, joiner)

	res := a.do(http.MethodDelete, "/api/trips/"+tripID+"/participants/"+joiner.String(), organizer, nil)
	require.Equal(t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())

	trip := decode(t, res)
	require.Equal(t, float64(1), trip["approved_count"])
	require.Len(t, trip["participants"].([]any), 1)

	res = a.do(http.MethodDelete, "/api/trips/"+tripID+"/participants/"+joiner.String(), organizer, nil)
	require.Equal(t, http.StatusNotFound, res.Code)
	require.Equal(t, "not_participant", errorOf(t, res)["code"])
}

func TestLeaveTripOverHTTP(t *testing.T) {
	a := newJoinAPI(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := a.recruiting(organizer, 8)
	a.join(tripID, organizer, joiner)

	res := a.do(http.MethodPost, "/api/trips/"+tripID+"/participants/me", joiner, nil)
	require.Equal(t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())
	require.Equal(t, float64(1), decode(t, res)["approved_count"])

	// The seat is genuinely free again: somebody else can take it.
	a.join(tripID, organizer, uuid.New())
}

func TestTheOrganizerCannotBeRemovedOverHTTP(t *testing.T) {
	a := newJoinAPI(t)
	organizer, joiner := uuid.New(), uuid.New()
	tripID := a.recruiting(organizer, 8)
	a.join(tripID, organizer, joiner)

	res := a.do(http.MethodDelete, "/api/trips/"+tripID+"/participants/"+organizer.String(), organizer, nil)
	require.Equal(t, http.StatusConflict, res.Code)
	require.Equal(t, "cannot_remove_organizer", errorOf(t, res)["code"])

	res = a.do(http.MethodPost, "/api/trips/"+tripID+"/participants/me", organizer, nil)
	require.Equal(t, http.StatusConflict, res.Code)
	require.Equal(t, "cannot_remove_organizer", errorOf(t, res)["code"])

	// And a participant cannot remove another participant.
	other := uuid.New()
	a.join(tripID, organizer, other)
	res = a.do(http.MethodDelete, "/api/trips/"+tripID+"/participants/"+other.String(), joiner, nil)
	require.Equal(t, http.StatusForbidden, res.Code)
	require.Equal(t, "not_organizer", errorOf(t, res)["code"])
}

// --- invitations ------------------------------------------------------------

func TestInviteUsersOverHTTP(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()
	tripID := a.recruiting(organizer, 8)
	first, second := uuid.New(), uuid.New()

	res := a.do(http.MethodPost, "/api/trips/"+tripID+"/invites", organizer,
		map[string]any{"user_ids": []string{first.String(), second.String()}})
	require.Equal(t, http.StatusCreated, res.Code, "unexpected body: %s", res.Body.String())

	items := decode(t, res)["items"].([]any)
	require.Len(t, items, 2)
	invite := items[0].(map[string]any)
	require.NotEmpty(t, invite["id"])
	require.NotEmpty(t, invite["expires_at"])

	created, err := time.Parse(time.RFC3339, invite["created_at"].(string))
	require.NoError(t, err)
	expires, err := time.Parse(time.RFC3339, invite["expires_at"].(string))
	require.NoError(t, err)
	require.Equal(t, created.Add(7*24*time.Hour), expires)

	listed := decode(t, a.do(http.MethodGet, "/api/trips/"+tripID+"/invites", organizer, nil))
	require.Len(t, listed["items"].([]any), 2)
}

// All or nothing, and the 409 names the ids that caused it — otherwise the
// organizer has to bisect their own request.
func TestInviteConflictNamesTheUsers(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()
	tripID := a.recruiting(organizer, 8)
	already, fresh := uuid.New(), uuid.New()

	require.Equal(t, http.StatusCreated, a.do(http.MethodPost, "/api/trips/"+tripID+"/invites", organizer,
		map[string]any{"user_ids": []string{already.String()}}).Code)

	res := a.do(http.MethodPost, "/api/trips/"+tripID+"/invites", organizer,
		map[string]any{"user_ids": []string{fresh.String(), already.String()}})
	require.Equal(t, http.StatusConflict, res.Code)

	errObj := errorOf(t, res)
	require.Equal(t, "already_invited", errObj["code"])
	details := errObj["details"].(map[string]any)
	require.Equal(t, []any{already.String()}, details["user_ids"])

	// The one that would have been fine was not written either.
	listed := decode(t, a.do(http.MethodGet, "/api/trips/"+tripID+"/invites", organizer, nil))
	require.Len(t, listed["items"].([]any), 1)
}

func TestInviteRefusals(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()

	t.Run("themselves", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/invites", organizer,
			map[string]any{"user_ids": []string{organizer.String()}})
		require.Equal(t, http.StatusConflict, res.Code)
		require.Equal(t, "cannot_invite_self", errorOf(t, res)["code"])
	})

	t.Run("an id that is not a uuid, naming its position", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/invites", organizer,
			map[string]any{"user_ids": []string{uuid.NewString(), "nonsense"}})
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Equal(t, []string{"user_ids[1]"}, fieldPaths(t, res))
	})

	t.Run("nobody at all", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/invites", organizer,
			map[string]any{"user_ids": []string{}})
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Equal(t, []string{"user_ids"}, fieldPaths(t, res))
	})

	t.Run("by a stranger", func(t *testing.T) {
		tripID := a.recruiting(organizer, 8)
		res := a.do(http.MethodPost, "/api/trips/"+tripID+"/invites", uuid.New(),
			map[string]any{"user_ids": []string{uuid.NewString()}})
		require.Equal(t, http.StatusForbidden, res.Code)
	})
}

// Every participation endpoint sits inside the authenticated group.
func TestParticipationEndpointsRequireAToken(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()
	tripID := a.recruiting(organizer, 8)

	for _, route := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/trips/" + tripID + "/requests"},
		{http.MethodGet, "/api/trips/" + tripID + "/requests"},
		{http.MethodGet, "/api/trips/" + tripID + "/requests/me"},
		{http.MethodDelete, "/api/trips/" + tripID + "/requests/me"},
		{http.MethodPost, "/api/trips/" + tripID + "/requests/" + uuid.NewString() + "/approve"},
		{http.MethodPost, "/api/trips/" + tripID + "/requests/" + uuid.NewString() + "/reject"},
		{http.MethodPost, "/api/trips/" + tripID + "/participants/me"},
		{http.MethodDelete, "/api/trips/" + tripID + "/participants/" + uuid.NewString()},
		{http.MethodPost, "/api/trips/" + tripID + "/invites"},
		{http.MethodGet, "/api/trips/" + tripID + "/invites"},
	} {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			res := a.do(route.method, route.path, uuid.Nil, nil)
			require.Equal(t, http.StatusUnauthorized, res.Code)
			require.Equal(t, "invalid_access_token", errorOf(t, res)["code"])
		})
	}
}

// peopleStub is identity's /internal/users over real HTTP, answering for any id
// it is asked about — the participation tests generate their users rather than
// picking from a fixture, and what is under test is that the queue resolves
// them in one call, not which names come back.
type peopleStub struct {
	server *httptest.Server
	calls  atomic.Int64
	down   atomic.Bool
}

func newPeopleStub(t *testing.T) *peopleStub {
	t.Helper()

	stub := &peopleStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.calls.Add(1)
		if stub.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		require.Equal(t, stubInternalToken, r.Header.Get("X-Internal-Token"))

		found := []map[string]any{}
		for _, raw := range strings.Split(r.URL.Query().Get("ids"), ",") {
			id, err := uuid.Parse(raw)
			if err != nil {
				continue
			}
			found = append(found, map[string]any{
				"id":           id.String(),
				"full_name":    "Traveller " + id.String()[:8],
				"photo_url":    "https://example.test/" + id.String() + ".jpg",
				"rating_avg":   4.6,
				"rating_count": 12,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"users": found}))
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *peopleStub) client() users.Resolver {
	return users.New(users.Options{
		BaseURL:       s.server.URL,
		InternalToken: stubInternalToken,
		Timeout:       2 * time.Second,
	})
}
