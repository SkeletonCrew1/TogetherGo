package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/auth"
	httpapi "github.com/togethergo/trip/internal/http"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/testsupport"
)

// api wires the real router over a real database and a stub identity service.
// Nothing between the request and Postgres is faked: the acceptance criteria
// are statements about status codes and bodies, and a mocked store would only
// prove the mock agreed with itself.
type api struct {
	t        *testing.T
	handler  http.Handler
	identity *testsupport.Identity
}

func newAPI(t *testing.T) *api {
	t.Helper()

	pool := testsupport.Pool(t)
	identity := testsupport.NewIdentity(t)

	return &api{
		t:        t,
		identity: identity,
		handler: httpapi.NewRouter(httpapi.Deps{
			Store:    store.New(pool),
			Verifier: auth.NewVerifier(auth.NewKeySet(identity.JWKSURL, time.Minute, nil)),
			Logger:   httpapi.NewLogger("error"),
		}),
	}
}

// do issues a request as `user`, or unauthenticated when user is uuid.Nil.
func (a *api) do(method, path string, user uuid.UUID, body any) *httptest.ResponseRecorder {
	a.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(a.t, err)
		reader = bytes.NewReader(encoded)
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		req.Header.Set("Authorization", "Bearer "+a.identity.Token(a.t, user))
	}

	recorder := httptest.NewRecorder()
	a.handler.ServeHTTP(recorder, req)
	return recorder
}

// raw issues a request with a body that is not necessarily valid JSON.
func (a *api) raw(method, path string, user uuid.UUID, body string) *httptest.ResponseRecorder {
	a.t.Helper()

	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	if user != uuid.Nil {
		req.Header.Set("Authorization", "Bearer "+a.identity.Token(a.t, user))
	}

	recorder := httptest.NewRecorder()
	a.handler.ServeHTTP(recorder, req)
	return recorder
}

// createTrip posts a valid trip and returns the decoded response.
func (a *api) createTrip(user uuid.UUID, overrides ...func(map[string]any)) map[string]any {
	a.t.Helper()

	body := validBody()
	for _, override := range overrides {
		override(body)
	}

	res := a.do(http.MethodPost, "/api/trips", user, body)
	require.Equal(a.t, http.StatusCreated, res.Code, "unexpected body: %s", res.Body.String())
	return decode(a.t, res)
}

func validBody() map[string]any {
	start := time.Now().Add(21 * 24 * time.Hour).UTC().Truncate(time.Second)
	return map[string]any{
		"title":       "Carpathians in autumn",
		"description": "Three days on the Chornohora ridge.",
		"category":    "hiking",
		"capacity":    8,
		"start_at":    start.Format(time.RFC3339),
		"end_at":      start.Add(3 * 24 * time.Hour).Format(time.RFC3339),
		"points": []any{
			map[string]any{"name": "Lviv", "lat": 49.8397, "lng": 24.0297},
			map[string]any{"name": "Vorokhta", "lat": 48.2833, "lng": 24.5667, "transport": "train"},
			map[string]any{"name": "Hoverla", "lat": 48.16, "lng": 24.5},
		},
	}
}

func decode(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	require.Equal(t, "application/json", res.Header().Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body), "body was: %s", res.Body.String())
	return body
}

// errorOf asserts the single error shape and returns the error object.
func errorOf(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	body := decode(t, res)
	require.Contains(t, body, "error", "every error response uses the {\"error\": {...}} shape")

	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok)
	require.NotEmpty(t, errObj["code"])
	require.NotEmpty(t, errObj["message"])
	return errObj
}

// fieldPaths pulls the field paths out of a validation_error's details.
func fieldPaths(t *testing.T, res *httptest.ResponseRecorder) []string {
	t.Helper()

	errObj := errorOf(t, res)
	require.Equal(t, "validation_error", errObj["code"])

	details, ok := errObj["details"].(map[string]any)
	require.True(t, ok, "a validation_error must carry details")
	fields, ok := details["fields"].([]any)
	require.True(t, ok, "details.fields must be a list")
	require.NotEmpty(t, fields)

	paths := make([]string, 0, len(fields))
	for _, raw := range fields {
		field, ok := raw.(map[string]any)
		require.True(t, ok)
		require.NotEmpty(t, field["message"], "every field error carries a message")
		paths = append(paths, field["field"].(string))
	}
	return paths
}

func tripPath(trip map[string]any, suffix ...string) string {
	path := "/api/trips/" + trip["id"].(string)
	for _, s := range suffix {
		path += s
	}
	return path
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

func TestEveryTripEndpointRequiresAToken(t *testing.T) {
	a := newAPI(t)
	id := uuid.New().String()

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/trips"},
		{http.MethodGet, "/api/trips/" + id},
		{http.MethodPatch, "/api/trips/" + id},
		{http.MethodDelete, "/api/trips/" + id},
		{http.MethodPost, "/api/trips/" + id + "/publish"},
		{http.MethodPost, "/api/trips/" + id + "/cancel"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			res := a.do(tc.method, tc.path, uuid.Nil, nil)

			require.Equal(t, http.StatusUnauthorized, res.Code)
			require.Equal(t, "Bearer", res.Header().Get("WWW-Authenticate"))
			require.Equal(t, "invalid_access_token", errorOf(t, res)["code"])
		})
	}
}

// TestAGatewayInjectedHeaderIsNotIdentity: rule 6. In Compose every container
// shares a network with every other, so a header claiming who the caller is
// proves nothing.
func TestAGatewayInjectedHeaderIsNotIdentity(t *testing.T) {
	a := newAPI(t)

	req := httptest.NewRequest(http.MethodPost, "/api/trips", nil)
	req.Header.Set("X-User-Id", uuid.NewString())
	req.Header.Set("X-Authenticated-User", uuid.NewString())

	recorder := httptest.NewRecorder()
	a.handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestAMalformedAuthorizationHeader(t *testing.T) {
	a := newAPI(t)

	for _, header := range []string{
		"",
		"Bearer",
		"Bearer ",
		"Basic dXNlcjpwYXNz",
		"bearer",
		"Token abc.def.ghi",
	} {
		t.Run(fmt.Sprintf("%q", header), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/trips/"+uuid.NewString(), nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}

			recorder := httptest.NewRecorder()
			a.handler.ServeHTTP(recorder, req)
			require.Equal(t, http.StatusUnauthorized, recorder.Code)
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/trips
// ---------------------------------------------------------------------------

func TestCreateTrip(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()

	res := a.do(http.MethodPost, "/api/trips", organizer, validBody())
	require.Equal(t, http.StatusCreated, res.Code, res.Body.String())

	trip := decode(t, res)
	require.Equal(t, "draft", trip["status"], "a new trip is always a draft")
	require.Equal(t, organizer.String(), trip["organizer_id"])
	require.Equal(t, "Carpathians in autumn", trip["title"])
	require.EqualValues(t, 8, trip["capacity"])
	require.EqualValues(t, 1, trip["approved_count"])
	require.EqualValues(t, 7, trip["spots_left"])

	require.Equal(t, "/api/trips/"+trip["id"].(string), res.Header().Get("Location"))

	points := trip["points"].([]any)
	require.Len(t, points, 3)
	require.Equal(t, "Lviv", points[0].(map[string]any)["name"])
	require.EqualValues(t, 0, points[0].(map[string]any)["seq"])
	require.EqualValues(t, 2, points[2].(map[string]any)["seq"])

	participants := trip["participants"].([]any)
	require.Len(t, participants, 1)
	require.Equal(t, organizer.String(), participants[0].(map[string]any)["user_id"])
	require.Equal(t, "organizer", participants[0].(map[string]any)["role"])

	departure := trip["departure"].(map[string]any)
	require.InDelta(t, 49.8397, departure["lat"], 1e-9)
	destination := trip["destination"].(map[string]any)
	require.InDelta(t, 48.16, destination["lat"], 1e-9)

	// Timestamps go out as RFC 3339 in UTC.
	_, err := time.Parse(time.RFC3339, trip["start_at"].(string))
	require.NoError(t, err)
	require.Contains(t, trip["created_at"].(string), "Z")
}

// TestCreateTripWithOnePointIsA400: the acceptance criterion, end to end.
func TestCreateTripWithOnePointIsA400(t *testing.T) {
	a := newAPI(t)

	body := validBody()
	body["points"] = []any{map[string]any{"name": "Lviv", "lat": 49.8397, "lng": 24.0297}}

	res := a.do(http.MethodPost, "/api/trips", uuid.New(), body)

	require.Equal(t, http.StatusBadRequest, res.Code)
	require.Equal(t, "validation_error", errorOf(t, res)["code"])
	require.Contains(t, fieldPaths(t, res), "points", "the error must name the field that failed")
}

func TestCreateTripValidation(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()

	for _, tc := range []struct {
		name   string
		break_ func(map[string]any)
		field  string
	}{
		{"title too short", func(b map[string]any) { b["title"] = "ab" }, "title"},
		{"category off the allowlist", func(b map[string]any) { b["category"] = "spelunking" }, "category"},
		{"capacity of one", func(b map[string]any) { b["capacity"] = 1 }, "capacity"},
		{"capacity of fifty-one", func(b map[string]any) { b["capacity"] = 51 }, "capacity"},
		{
			"a start date in the past",
			func(b map[string]any) { b["start_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) },
			"start_at",
		},
		{
			"a trip longer than sixty days",
			func(b map[string]any) {
				start := time.Now().Add(24 * time.Hour).UTC()
				b["start_at"] = start.Format(time.RFC3339)
				b["end_at"] = start.Add(61 * 24 * time.Hour).Format(time.RFC3339)
			},
			"end_at",
		},
		{
			"latitude out of range",
			func(b map[string]any) { b["points"].([]any)[1].(map[string]any)["lat"] = 91.0 },
			"points[1].lat",
		},
		{
			"longitude out of range",
			func(b map[string]any) { b["points"].([]any)[0].(map[string]any)["lng"] = -181.0 },
			"points[0].lng",
		},
		{
			"a point with no latitude at all",
			func(b map[string]any) { delete(b["points"].([]any)[2].(map[string]any), "lat") },
			"points[2].lat",
		},
		{
			"twenty-one points",
			func(b map[string]any) {
				points := make([]any, 21)
				for i := range points {
					points[i] = map[string]any{"name": "stop", "lat": float64(i), "lng": float64(i)}
				}
				b["points"] = points
			},
			"points",
		},
		{
			"two identical consecutive points",
			func(b map[string]any) {
				points := b["points"].([]any)
				points[2].(map[string]any)["lat"] = points[1].(map[string]any)["lat"]
				points[2].(map[string]any)["lng"] = points[1].(map[string]any)["lng"]
			},
			"points[2]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := validBody()
			tc.break_(body)

			res := a.do(http.MethodPost, "/api/trips", organizer, body)
			require.Equal(t, http.StatusBadRequest, res.Code, res.Body.String())
			require.Contains(t, fieldPaths(t, res), tc.field)
		})
	}
}

func TestCreateTripRejectsAnUnreadableBody(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()

	t.Run("not JSON at all", func(t *testing.T) {
		res := a.raw(http.MethodPost, "/api/trips", organizer, "{nope")
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Equal(t, "invalid_body", errorOf(t, res)["code"])
	})

	t.Run("an empty body", func(t *testing.T) {
		res := a.raw(http.MethodPost, "/api/trips", organizer, "")
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Equal(t, "invalid_body", errorOf(t, res)["code"])
	})

	t.Run("a misspelled key", func(t *testing.T) {
		// Silently ignoring `titel` would store an empty title and report
		// success; the failure would surface as a trip nobody can find.
		res := a.raw(http.MethodPost, "/api/trips", organizer, `{"titel": "typo"}`)
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Equal(t, "invalid_body", errorOf(t, res)["code"])
	})

	t.Run("a value of the wrong type", func(t *testing.T) {
		res := a.raw(http.MethodPost, "/api/trips", organizer, `{"capacity": "eight"}`)
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Contains(t, fieldPaths(t, res), "capacity")
	})

	t.Run("two JSON objects", func(t *testing.T) {
		res := a.raw(http.MethodPost, "/api/trips", organizer, `{} {}`)
		require.Equal(t, http.StatusBadRequest, res.Code)
		require.Equal(t, "invalid_body", errorOf(t, res)["code"])
	})
}

// ---------------------------------------------------------------------------
// GET /api/trips/{id}
// ---------------------------------------------------------------------------

func TestGetTrip(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	res := a.do(http.MethodGet, tripPath(created), organizer, nil)
	require.Equal(t, http.StatusOK, res.Code)

	trip := decode(t, res)
	require.Equal(t, created["id"], trip["id"])
	require.Len(t, trip["points"], 3)
	require.Len(t, trip["participants"], 1)
	require.EqualValues(t, 8, trip["capacity"])
	require.EqualValues(t, 7, trip["spots_left"])
}

// TestGetDraftByANonOrganizerIs404 is the acceptance criterion: drafts are
// invisible, so an unauthorised read is answered "no such trip" and not
// "forbidden". A 403 would confirm the id names a real trip.
func TestGetDraftByANonOrganizerIs404(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	res := a.do(http.MethodGet, tripPath(created), uuid.New(), nil)

	require.Equal(t, http.StatusNotFound, res.Code)
	require.Equal(t, "trip_not_found", errorOf(t, res)["code"])
	require.NotEqual(t, http.StatusForbidden, res.Code, "a draft must not confirm its own existence")
}

func TestGetPublishedTripByANonOrganizer(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	require.Equal(t, http.StatusOK, a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil).Code)

	res := a.do(http.MethodGet, tripPath(created), uuid.New(), nil)
	require.Equal(t, http.StatusOK, res.Code, "a recruiting trip is public")
	require.Equal(t, "recruiting", decode(t, res)["status"])
}

func TestGetAnUnknownTrip(t *testing.T) {
	a := newAPI(t)

	res := a.do(http.MethodGet, "/api/trips/"+uuid.NewString(), uuid.New(), nil)
	require.Equal(t, http.StatusNotFound, res.Code)
	require.Equal(t, "trip_not_found", errorOf(t, res)["code"])
}

func TestGetWithAMalformedID(t *testing.T) {
	a := newAPI(t)

	res := a.do(http.MethodGet, "/api/trips/not-a-uuid", uuid.New(), nil)
	require.Equal(t, http.StatusBadRequest, res.Code)
	require.Contains(t, fieldPaths(t, res), "id")
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestPublishAndCancel(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	published := a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil)
	require.Equal(t, http.StatusOK, published.Code)
	require.Equal(t, "recruiting", decode(t, published)["status"])

	cancelled := a.do(http.MethodPost, tripPath(created, "/cancel"), organizer, nil)
	require.Equal(t, http.StatusOK, cancelled.Code)
	require.Equal(t, "cancelled", decode(t, cancelled)["status"])
}

// TestPublishingACancelledTripIs409 is the acceptance criterion, with the
// `details` a stale client needs in order to recover.
func TestPublishingACancelledTripIs409(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	require.Equal(t, http.StatusOK, a.do(http.MethodPost, tripPath(created, "/cancel"), organizer, nil).Code)

	res := a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil)

	require.Equal(t, http.StatusConflict, res.Code)
	errObj := errorOf(t, res)
	require.Equal(t, "invalid_transition", errObj["code"])

	details := errObj["details"].(map[string]any)
	require.Equal(t, "cancelled", details["from"])
	require.Equal(t, "recruiting", details["to"])
	require.Empty(t, details["allowed"], "nothing is allowed out of a terminal state")
}

func TestRepublishingIsAConflict(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	require.Equal(t, http.StatusOK, a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil).Code)

	// Not a no-op: answering 409 is what tells a client its copy is stale.
	res := a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil)
	require.Equal(t, http.StatusConflict, res.Code)
	require.Equal(t, "invalid_transition", errorOf(t, res)["code"])
}

func TestCancellingATwiceCancelledTrip(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	require.Equal(t, http.StatusOK, a.do(http.MethodPost, tripPath(created, "/cancel"), organizer, nil).Code)

	res := a.do(http.MethodPost, tripPath(created, "/cancel"), organizer, nil)
	require.Equal(t, http.StatusConflict, res.Code)
}

func TestLifecycleAuthorisation(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()

	t.Run("publishing someone else's draft is a 404", func(t *testing.T) {
		created := a.createTrip(organizer)
		res := a.do(http.MethodPost, tripPath(created, "/publish"), uuid.New(), nil)
		require.Equal(t, http.StatusNotFound, res.Code)
	})

	t.Run("cancelling someone else's published trip is a 403", func(t *testing.T) {
		created := a.createTrip(organizer)
		require.Equal(t, http.StatusOK, a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil).Code)

		res := a.do(http.MethodPost, tripPath(created, "/cancel"), uuid.New(), nil)
		require.Equal(t, http.StatusForbidden, res.Code, "the trip is public, so the honest 403 leaks nothing")
		require.Equal(t, "not_organizer", errorOf(t, res)["code"])
	})
}

// ---------------------------------------------------------------------------
// PATCH /api/trips/{id}
// ---------------------------------------------------------------------------

func TestUpdateTrip(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	res := a.do(http.MethodPatch, tripPath(created), organizer, map[string]any{
		"title":    "Chornohora ridge, three days",
		"capacity": 12,
	})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	trip := decode(t, res)
	require.Equal(t, "Chornohora ridge, three days", trip["title"])
	require.EqualValues(t, 12, trip["capacity"])
	require.Equal(t, created["description"], trip["description"], "an omitted key changes nothing")
	require.Equal(t, created["category"], trip["category"])
	require.Len(t, trip["points"], 3)
}

func TestUpdateTripClearsADescriptionWithNull(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)
	require.NotNil(t, created["description"])

	res := a.raw(http.MethodPatch, tripPath(created), organizer, `{"description": null}`)
	require.Equal(t, http.StatusOK, res.Code)
	require.Nil(t, decode(t, res)["description"])
}

func TestUpdateTripReplacesTheWholeRoute(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	res := a.do(http.MethodPatch, tripPath(created), organizer, map[string]any{
		"points": []any{
			map[string]any{"name": "Kyiv", "lat": 50.4501, "lng": 30.5234},
			map[string]any{"name": "Odesa", "lat": 46.4825, "lng": 30.7233},
		},
	})
	require.Equal(t, http.StatusOK, res.Code, res.Body.String())

	trip := decode(t, res)
	points := trip["points"].([]any)
	require.Len(t, points, 2)
	require.Equal(t, "Kyiv", points[0].(map[string]any)["name"])

	// The denormalised endpoints followed the route.
	require.InDelta(t, 50.4501, trip["departure"].(map[string]any)["lat"], 1e-9)
	require.InDelta(t, 46.4825, trip["destination"].(map[string]any)["lat"], 1e-9)
}

func TestUpdateTripIsRefusedOnceUnderWay(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	require.Equal(t, http.StatusOK, a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil).Code)
	require.Equal(t, http.StatusOK, a.do(http.MethodPost, tripPath(created, "/cancel"), organizer, nil).Code)

	res := a.do(http.MethodPatch, tripPath(created), organizer, map[string]any{"title": "too late"})
	require.Equal(t, http.StatusConflict, res.Code)
	require.Equal(t, "trip_not_editable", errorOf(t, res)["code"])
}

func TestUpdateTripByANonOrganizer(t *testing.T) {
	a := newAPI(t)
	created := a.createTrip(uuid.New())

	res := a.do(http.MethodPatch, tripPath(created), uuid.New(), map[string]any{"title": "mine now"})
	require.Equal(t, http.StatusNotFound, res.Code, "a draft is invisible even to a would-be editor")
}

func TestUpdateTripValidatesTheMergedResult(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	// end_at alone, moved before the stored start_at.
	start, err := time.Parse(time.RFC3339, created["start_at"].(string))
	require.NoError(t, err)

	res := a.do(http.MethodPatch, tripPath(created), organizer, map[string]any{
		"end_at": start.Add(-time.Hour).Format(time.RFC3339),
	})
	require.Equal(t, http.StatusBadRequest, res.Code)
	require.Contains(t, fieldPaths(t, res), "end_at")
}

func TestUpdateTripRejectsAnEmptyRoute(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	res := a.raw(http.MethodPatch, tripPath(created), organizer, `{"points": []}`)
	require.Equal(t, http.StatusBadRequest, res.Code)
	require.Contains(t, fieldPaths(t, res), "points")
}

// ---------------------------------------------------------------------------
// DELETE /api/trips/{id}
// ---------------------------------------------------------------------------

func TestDeleteTrip(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	res := a.do(http.MethodDelete, tripPath(created), organizer, nil)
	require.Equal(t, http.StatusNoContent, res.Code)
	require.Empty(t, res.Body.String(), "204 carries no body")

	require.Equal(t, http.StatusNotFound, a.do(http.MethodGet, tripPath(created), organizer, nil).Code)
}

func TestDeleteAPublishedTripIsAConflict(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()
	created := a.createTrip(organizer)

	require.Equal(t, http.StatusOK, a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil).Code)

	res := a.do(http.MethodDelete, tripPath(created), organizer, nil)
	require.Equal(t, http.StatusConflict, res.Code)
	require.Equal(t, "trip_not_draft", errorOf(t, res)["code"])

	require.Equal(t, http.StatusOK, a.do(http.MethodGet, tripPath(created), organizer, nil).Code)
}

func TestDeleteByANonOrganizer(t *testing.T) {
	a := newAPI(t)
	created := a.createTrip(uuid.New())

	res := a.do(http.MethodDelete, tripPath(created), uuid.New(), nil)
	require.Equal(t, http.StatusNotFound, res.Code)
}

// ---------------------------------------------------------------------------
// Router-level behaviour
// ---------------------------------------------------------------------------

func TestHealthEndpointsNeedNoToken(t *testing.T) {
	a := newAPI(t)

	res := a.do(http.MethodGet, "/healthz", uuid.Nil, nil)
	require.Equal(t, http.StatusOK, res.Code)
	require.Equal(t, "ok", decode(t, res)["status"])
}

func TestReadyzChecksItsDependencies(t *testing.T) {
	a := newAPI(t)

	res := a.do(http.MethodGet, "/readyz", uuid.Nil, nil)
	require.Equal(t, http.StatusOK, res.Code)

	body := decode(t, res)
	require.Equal(t, "ready", body["status"])
	checks := body["checks"].(map[string]any)
	require.Equal(t, "ok", checks["database"])
	require.Equal(t, "ok", checks["jwks"])
}

func TestUnknownRoutesAndMethodsUseTheSameErrorShape(t *testing.T) {
	a := newAPI(t)

	notFound := a.do(http.MethodGet, "/api/nope", uuid.Nil, nil)
	require.Equal(t, http.StatusNotFound, notFound.Code)
	require.Equal(t, "not_found", errorOf(t, notFound)["code"])

	wrongMethod := a.do(http.MethodPut, "/api/trips/"+uuid.NewString(), uuid.New(), nil)
	require.Equal(t, http.StatusMethodNotAllowed, wrongMethod.Code)
	require.Equal(t, "method_not_allowed", errorOf(t, wrongMethod)["code"])
}

func TestEveryResponseCarriesARequestID(t *testing.T) {
	a := newAPI(t)

	generated := a.do(http.MethodGet, "/healthz", uuid.Nil, nil)
	require.NotEmpty(t, generated.Header().Get("X-Request-ID"))

	// An inbound id is honoured, so a trace survives the gateway hop.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("X-Request-ID", "trace-me")
	recorder := httptest.NewRecorder()
	a.handler.ServeHTTP(recorder, req)

	require.Equal(t, "trace-me", recorder.Header().Get("X-Request-ID"))
}

// TestAnOversizedBodyIsRejected: the endpoint buffers request bodies, so it
// caps them rather than letting a client decide how much memory to use.
func TestAnOversizedBodyIsRejected(t *testing.T) {
	a := newAPI(t)

	body := validBody()
	body["description"] = string(bytes.Repeat([]byte("x"), 128<<10))

	res := a.do(http.MethodPost, "/api/trips", uuid.New(), body)
	require.Equal(t, http.StatusBadRequest, res.Code)
	require.Equal(t, "invalid_body", errorOf(t, res)["code"])
}

// TestTheFullLifecycleOfATrip walks a trip from draft to completed through the
// endpoints, which is the sequence a client actually performs.
func TestTheFullLifecycleOfATrip(t *testing.T) {
	a := newAPI(t)
	organizer := uuid.New()

	created := a.createTrip(organizer)
	require.Equal(t, "draft", created["status"])

	// A draft can still be edited freely.
	edited := a.do(http.MethodPatch, tripPath(created), organizer, map[string]any{"capacity": 6})
	require.Equal(t, http.StatusOK, edited.Code)

	published := a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil)
	require.Equal(t, "recruiting", decode(t, published)["status"])

	// And once it is published it is no longer deletable.
	require.Equal(t, http.StatusConflict, a.do(http.MethodDelete, tripPath(created), organizer, nil).Code)

	// A recruiting trip is still editable.
	require.Equal(t, http.StatusOK,
		a.do(http.MethodPatch, tripPath(created), organizer, map[string]any{"title": "Chornohora, six of us"}).Code)

	cancelled := a.do(http.MethodPost, tripPath(created, "/cancel"), organizer, nil)
	require.Equal(t, "cancelled", decode(t, cancelled)["status"])

	// Terminal: nothing further is allowed.
	require.Equal(t, http.StatusConflict, a.do(http.MethodPost, tripPath(created, "/publish"), organizer, nil).Code)
	require.Equal(t, http.StatusConflict, a.do(http.MethodPost, tripPath(created, "/cancel"), organizer, nil).Code)
	require.Equal(t, http.StatusConflict,
		a.do(http.MethodPatch, tripPath(created), organizer, map[string]any{"title": "x"}).Code)
	require.Equal(t, http.StatusConflict, a.do(http.MethodDelete, tripPath(created), organizer, nil).Code)

	// But it is still readable, which is the point of cancelling rather than
	// deleting.
	require.Equal(t, http.StatusOK, a.do(http.MethodGet, tripPath(created), organizer, nil).Code)
}
