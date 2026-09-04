package httpapi_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// The discovery endpoints are exercised end to end: the real router, the real
// store over a real PostGIS database, a real HTTP identity to resolve
// organizers against, and the seeded fifty-trip world the store tests search.
// Nothing between the request and Postgres is faked, because the acceptance
// criteria are statements about status codes and bodies.

// The clock the seeded world is built around. The service itself uses
// time.Now, so the fixture is anchored to the same instant rather than to a
// fixed date; only the offsets matter.
var searchNow = time.Now().UTC()

type searchAPI struct {
	t        *testing.T
	handler  http.Handler
	identity *testsupport.Identity
	users    *organizerStub
	ids      map[string]uuid.UUID
	caller   uuid.UUID
}

func newSearchAPI(t *testing.T) *searchAPI {
	t.Helper()

	pool := testsupport.Pool(t)
	identity := testsupport.NewIdentity(t)
	organizers := newOrganizerStub(t)

	seeds := testsupport.DiscoveryWorld(organizers.ids)
	ids := testsupport.SeedTrips(t, pool, searchNow, seeds)

	return &searchAPI{
		t:        t,
		identity: identity,
		users:    organizers,
		ids:      ids,
		caller:   uuid.New(),
		handler: httpapi.NewRouter(httpapi.Deps{
			Store:    store.New(pool),
			Verifier: auth.NewVerifier(auth.NewKeySet(identity.JWKSURL, time.Minute, nil)),
			Logger:   httpapi.NewLogger("error"),
			Users:    organizers.client(),
		}),
	}
}

// get issues an authenticated GET.
func (a *searchAPI) get(path string) *httptest.ResponseRecorder {
	a.t.Helper()

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+a.identity.Token(a.t, a.caller))

	recorder := httptest.NewRecorder()
	a.handler.ServeHTTP(recorder, req)
	return recorder
}

// search runs a query and asserts it succeeded, returning the decoded page.
func (a *searchAPI) search(query string) searchBody {
	a.t.Helper()

	res := a.get("/api/trips?" + query)
	require.Equalf(a.t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())

	var body searchBody
	require.NoError(a.t, json.Unmarshal(res.Body.Bytes(), &body), "body was: %s", res.Body.String())
	return body
}

// titles walks every page of a query and returns the titles it saw.
func (a *searchAPI) titles(query string) []string {
	a.t.Helper()

	var titles []string
	cursor := ""
	for page := 0; page < 100; page++ {
		q := query
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		body := a.search(q)
		for _, item := range body.Items {
			titles = append(titles, item.Title)
		}
		if body.NextCursor == nil {
			return titles
		}
		cursor = *body.NextCursor
	}
	a.t.Fatal("pagination did not terminate")
	return nil
}

type searchBody struct {
	Items      []itemBody `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

type itemBody struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Category      string `json:"category"`
	Status        string `json:"status"`
	StartAt       string `json:"start_at"`
	EndAt         string `json:"end_at"`
	Capacity      int    `json:"capacity"`
	ApprovedCount int    `json:"approved_count"`
	FreeSlots     int    `json:"free_slots"`

	Departure   placeBody `json:"departure"`
	Destination placeBody `json:"destination"`

	RouteSummary struct {
		Points      []string `json:"points"`
		TotalPoints int      `json:"total_points"`
		Truncated   bool     `json:"truncated"`
	} `json:"route_summary"`

	Organizer struct {
		ID        string   `json:"id"`
		FullName  *string  `json:"full_name"`
		PhotoURL  *string  `json:"photo_url"`
		RatingAvg *float64 `json:"rating_avg"`
	} `json:"organizer"`
}

type placeBody struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
}

// organizerStub is identity's /internal/users, over real HTTP, counting calls.
type organizerStub struct {
	server *httptest.Server
	ids    []uuid.UUID
	names  map[uuid.UUID]string
	calls  atomic.Int64
	down   atomic.Bool
}

const stubInternalToken = "test-internal-token"

func newOrganizerStub(t *testing.T) *organizerStub {
	t.Helper()

	stub := &organizerStub{names: map[uuid.UUID]string{}}
	for _, name := range []string{"Olena Kovalenko", "Andrii Bondar", "Marta Hrytsenko"} {
		id := uuid.New()
		stub.ids = append(stub.ids, id)
		stub.names[id] = name
	}

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
			name, known := stub.names[id]
			if !known {
				continue
			}
			found = append(found, map[string]any{
				"id":           id.String(),
				"full_name":    name,
				"photo_url":    "https://example.test/" + id.String() + ".jpg",
				"rating_avg":   4.7,
				"rating_count": 9,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"users": found}))
	}))
	t.Cleanup(stub.server.Close)

	return stub
}

func (s *organizerStub) client() users.Resolver {
	return users.New(users.Options{
		BaseURL:       s.server.URL,
		InternalToken: stubInternalToken,
		Timeout:       2 * time.Second,
	})
}

// ---------------------------------------------------------------------------
// Acceptance criteria
// ---------------------------------------------------------------------------

// TestSearchNearIvanoFrankivsk is the first acceptance criterion, over HTTP.
func TestSearchNearIvanoFrankivsk(t *testing.T) {
	a := newSearchAPI(t)

	titles := a.titles(fmt.Sprintf("near_lat=%v&near_lng=%v&radius_km=50",
		testsupport.IvanoFrankivsk.Lat, testsupport.IvanoFrankivsk.Lng))

	require.Contains(t, titles, "Hoverla summit hike")
	require.NotContains(t, titles, "Kyiv street food crawl",
		"Kyiv is 470 km from Ivano-Frankivsk")
}

// TestSearchRejectsAHalfGivenRadius is the second acceptance criterion.
func TestSearchRejectsAHalfGivenRadius(t *testing.T) {
	a := newSearchAPI(t)

	res := a.get(fmt.Sprintf("/api/trips?near_lat=%v&radius_km=50", testsupport.IvanoFrankivsk.Lat))
	require.Equal(t, http.StatusBadRequest, res.Code)

	require.Equal(t, []string{"near_lng"}, fieldPaths(t, res))

	// The message has to say what to do about it, not merely that something
	// was wrong.
	errObj := errorOf(t, res)
	details := errObj["details"].(map[string]any)
	field := details["fields"].([]any)[0].(map[string]any)
	require.Contains(t, field["message"], "near_lat")
	require.Contains(t, field["message"], "required")
}

func TestSearchRejectsEveryIncompleteRadiusGroup(t *testing.T) {
	a := newSearchAPI(t)

	for _, tc := range []struct {
		query  string
		fields []string
	}{
		{"near_lat=48.9", []string{"near_lng", "radius_km"}},
		{"near_lng=24.7", []string{"near_lat", "radius_km"}},
		{"radius_km=50", []string{"near_lat", "near_lng"}},
		{"near_lat=48.9&near_lng=24.7", []string{"radius_km"}},
		{"dest_lat=48.1", []string{"dest_lng", "dest_radius_km"}},
		{"dest_lat=48.1&dest_lng=24.5", []string{"dest_radius_km"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			res := a.get("/api/trips?" + tc.query)
			require.Equal(t, http.StatusBadRequest, res.Code)
			require.ElementsMatch(t, tc.fields, fieldPaths(t, res))
		})
	}
}

// ---------------------------------------------------------------------------
// The response
// ---------------------------------------------------------------------------

func TestSearchProjectsEveryDocumentedField(t *testing.T) {
	a := newSearchAPI(t)

	body := a.search("q=hoverla")
	require.Len(t, body.Items, 1)
	item := body.Items[0]

	require.Equal(t, a.ids["Hoverla summit hike"].String(), item.ID)
	require.Equal(t, "Hoverla summit hike", item.Title)
	require.Equal(t, "hiking", item.Category)
	require.Equal(t, "recruiting", item.Status)
	require.Equal(t, 8, item.Capacity)
	require.Equal(t, 3, item.ApprovedCount)
	require.Equal(t, 5, item.FreeSlots)

	// UTC, RFC 3339, on the way out — whatever time zone the database session
	// happened to be in.
	require.True(t, strings.HasSuffix(item.StartAt, "Z"), "start_at was %q", item.StartAt)
	require.True(t, strings.HasSuffix(item.EndAt, "Z"))

	require.Equal(t, "Ivano-Frankivsk", item.Departure.Name)
	require.InDelta(t, testsupport.IvanoFrankivsk.Lat, item.Departure.Lat, 1e-9)
	require.InDelta(t, testsupport.IvanoFrankivsk.Lng, item.Departure.Lng, 1e-9)
	require.Equal(t, "Hoverla", item.Destination.Name)

	require.Equal(t, []string{"Ivano-Frankivsk", "Yaremche", "Hoverla"}, item.RouteSummary.Points)
	require.Equal(t, 3, item.RouteSummary.TotalPoints)
	require.False(t, item.RouteSummary.Truncated)

	require.Contains(t, a.users.names, uuid.MustParse(item.Organizer.ID))
	require.NotNil(t, item.Organizer.FullName)
	require.Equal(t, a.users.names[uuid.MustParse(item.Organizer.ID)], *item.Organizer.FullName)
	require.NotNil(t, item.Organizer.PhotoURL)
	require.NotNil(t, item.Organizer.RatingAvg)
}

// TestSearchResolvesOrganizersInOneCall is the N+1 this endpoint is built to
// avoid: a page of twenty trips is one request to identity, not twenty.
func TestSearchResolvesOrganizersInOneCall(t *testing.T) {
	a := newSearchAPI(t)

	body := a.search("limit=20")
	require.Len(t, body.Items, 20)
	require.EqualValues(t, 1, a.users.calls.Load())

	for _, item := range body.Items {
		require.NotNil(t, item.Organizer.FullName, "every organizer on the page resolved")
	}
}

// TestSearchSurvivesIdentityBeingDown is the rule the endpoint is designed
// around: a search page never 500s because a different service is unavailable.
func TestSearchSurvivesIdentityBeingDown(t *testing.T) {
	a := newSearchAPI(t)
	a.users.down.Store(true)

	res := a.get("/api/trips?limit=5")
	require.Equal(t, http.StatusOK, res.Code, "identity is down; the trips are not")

	var body searchBody
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))
	require.Len(t, body.Items, 5)

	for _, item := range body.Items {
		require.NotEmpty(t, item.Organizer.ID, "the id is this service's own data and is always there")
		require.Nil(t, item.Organizer.FullName)
		require.Nil(t, item.Organizer.PhotoURL)
		require.Nil(t, item.Organizer.RatingAvg)

		// The trip itself is complete: nothing about the outage leaks into it.
		require.NotEmpty(t, item.Title)
		require.NotEmpty(t, item.Departure.Name)
	}
}

func TestSearchDefaultsToTwentyItems(t *testing.T) {
	a := newSearchAPI(t)

	body := a.search("")
	require.Len(t, body.Items, 20)
	require.NotNil(t, body.NextCursor, "there are more than twenty trips in the fixture")
}

// ---------------------------------------------------------------------------
// Pagination over the wire
// ---------------------------------------------------------------------------

func TestSearchPaginatesWithAnOpaqueCursor(t *testing.T) {
	a := newSearchAPI(t)

	all := a.titles("limit=7")
	require.Greater(t, len(all), 40)

	seen := map[string]bool{}
	for _, title := range all {
		require.Falsef(t, seen[title], "%q appeared twice", title)
		seen[title] = true
	}

	// The same set, one page at a time or all at once.
	require.ElementsMatch(t, a.titles("limit=50"), all)
}

func TestSearchCursorSurvivesUrlEncoding(t *testing.T) {
	a := newSearchAPI(t)

	first := a.search("limit=3")
	require.NotNil(t, first.NextCursor)

	// base64url, so nothing in it needs escaping — but the client will escape
	// it anyway, and both forms have to work.
	raw := *first.NextCursor
	require.Equal(t, raw, url.QueryEscape(raw), "the cursor is URL-safe as emitted")

	second := a.search("limit=3&cursor=" + url.QueryEscape(raw))
	require.Len(t, second.Items, 3)

	firstIDs := map[string]bool{}
	for _, item := range first.Items {
		firstIDs[item.ID] = true
	}
	for _, item := range second.Items {
		require.False(t, firstIDs[item.ID], "the second page repeats nothing from the first")
	}
}

// TestSearchRejectsAMalformedCursor: a cursor is opaque, so a client will
// eventually send something that is not one. That is a 400, never a panic.
func TestSearchRejectsAMalformedCursor(t *testing.T) {
	a := newSearchAPI(t)

	for _, cursor := range []string{
		"not-base64!!",
		"aGVsbG8",                     // base64 of "hello"
		"MjAyNi0wOS0xNFQwODowMDowMFo", // a timestamp with no id
		"fA",                          // base64 of "|"
		strings.Repeat("A", 4096),     // far too long
		"eyJzdGFydF9hdCI6ICJub3ciIH0", // base64 of a JSON object
	} {
		t.Run(cursor[:min(len(cursor), 16)], func(t *testing.T) {
			res := a.get("/api/trips?cursor=" + url.QueryEscape(cursor))
			require.Equal(t, http.StatusBadRequest, res.Code)
			require.Equal(t, []string{"cursor"}, fieldPaths(t, res))
		})
	}
}

// ---------------------------------------------------------------------------
// Query validation
// ---------------------------------------------------------------------------

func TestSearchValidatesTheQueryString(t *testing.T) {
	a := newSearchAPI(t)

	for _, tc := range []struct {
		name, query string
		fields      []string
	}{
		{"limit above the maximum", "limit=51", []string{"limit"}},
		{"limit of zero", "limit=0", []string{"limit"}},
		{"limit that is not a number", "limit=twenty", []string{"limit"}},
		{"radius below the minimum", "near_lat=48.9&near_lng=24.7&radius_km=0.5", []string{"radius_km"}},
		{"radius above the maximum", "near_lat=48.9&near_lng=24.7&radius_km=501", []string{"radius_km"}},
		{"a latitude off the planet", "near_lat=91&near_lng=24.7&radius_km=50", []string{"near_lat"}},
		{"a date that is not a date", "date_from=next+tuesday", []string{"date_from"}},
		{"an inverted date window", "date_from=2026-10-01T00:00:00Z&date_to=2026-09-01T00:00:00Z", []string{"date_to"}},
		{"a category outside the allowlist", "categories=spelunking", []string{"categories"}},
		{"one bad category among good ones", "categories=hiking,spelunking", []string{"categories"}},
		{"zero days", "min_days=0", []string{"min_days"}},
		{"max_days below min_days", "min_days=5&max_days=2", []string{"max_days"}},
		{"negative free slots", "min_free_slots=-1", []string{"min_free_slots"}},
		{"a parameter nobody recognises", "catgories=hiking", []string{"catgories"}},
		{"a parameter given twice", "limit=5&limit=50", []string{"limit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := a.get("/api/trips?" + tc.query)
			require.Equalf(t, http.StatusBadRequest, res.Code, "body: %s", res.Body.String())
			require.ElementsMatch(t, tc.fields, fieldPaths(t, res))
		})
	}
}

// TestSearchReportsEveryProblemAtOnce — one round trip per form submission, not
// one per mistake.
func TestSearchReportsEveryProblemAtOnce(t *testing.T) {
	a := newSearchAPI(t)

	res := a.get("/api/trips?near_lat=48.9&limit=900&categories=spelunking")
	require.Equal(t, http.StatusBadRequest, res.Code)
	require.ElementsMatch(t,
		[]string{"near_lng", "radius_km", "limit", "categories"},
		fieldPaths(t, res))
}

func TestSearchAcceptsEveryFilterTogether(t *testing.T) {
	a := newSearchAPI(t)

	body := a.search(fmt.Sprintf(
		"date_from=%s&date_to=%s"+
			"&near_lat=%v&near_lng=%v&radius_km=100"+
			"&dest_lat=%v&dest_lng=%v&dest_radius_km=25"+
			"&min_days=1&max_days=30&min_free_slots=1"+
			"&categories=hiking,nature&q=hoverla&limit=10",
		url.QueryEscape(searchNow.Format(time.RFC3339)),
		url.QueryEscape(searchNow.AddDate(1, 0, 0).Format(time.RFC3339)),
		testsupport.IvanoFrankivsk.Lat, testsupport.IvanoFrankivsk.Lng,
		testsupport.Hoverla.Lat, testsupport.Hoverla.Lng,
	))

	require.Len(t, body.Items, 1)
	require.Equal(t, "Hoverla summit hike", body.Items[0].Title)
	require.Nil(t, body.NextCursor)
}

func TestSearchRequiresAToken(t *testing.T) {
	a := newSearchAPI(t)

	for _, path := range []string{
		"/api/trips",
		"/api/trips?q=hoverla",
		"/api/trips/" + a.ids["Hoverla summit hike"].String() + "/similar",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		a.handler.ServeHTTP(recorder, req)

		require.Equal(t, http.StatusUnauthorized, recorder.Code, "path: %s", path)
		require.Equal(t, "invalid_access_token", errorOf(t, recorder)["code"])
	}
}

// ---------------------------------------------------------------------------
// /similar
// ---------------------------------------------------------------------------

func TestSimilarTripsEndpoint(t *testing.T) {
	a := newSearchAPI(t)

	source := a.ids["Hoverla summit hike"]
	res := a.get("/api/trips/" + source.String() + "/similar")
	require.Equalf(t, http.StatusOK, res.Code, "body: %s", res.Body.String())

	var body struct {
		Items []itemBody `json:"items"`
	}
	require.NoError(t, json.Unmarshal(res.Body.Bytes(), &body))

	require.NotEmpty(t, body.Items)
	require.LessOrEqual(t, len(body.Items), 5)

	for _, item := range body.Items {
		require.Equal(t, "hiking", item.Category)
		require.Equal(t, "recruiting", item.Status)
		require.NotEqual(t, source.String(), item.ID)
		require.NotNil(t, item.Organizer.FullName, "similar trips carry the same organizer block")
	}
}

func TestSimilarTripsRejectsAnUnknownTrip(t *testing.T) {
	a := newSearchAPI(t)

	res := a.get("/api/trips/" + uuid.New().String() + "/similar")
	require.Equal(t, http.StatusNotFound, res.Code)
	require.Equal(t, "trip_not_found", errorOf(t, res)["code"])

	res = a.get("/api/trips/not-a-uuid/similar")
	require.Equal(t, http.StatusBadRequest, res.Code)
}

// TestSimilarTripsHidesSomeoneElsesDraft: the same rule the detail endpoint
// applies. A 403 would confirm that the id names a real trip.
func TestSimilarTripsHidesSomeoneElsesDraft(t *testing.T) {
	a := newSearchAPI(t)

	res := a.get("/api/trips/" + a.ids["Hoverla winter draft"].String() + "/similar")
	require.Equal(t, http.StatusNotFound, res.Code)
	require.Equal(t, "trip_not_found", errorOf(t, res)["code"])
}
