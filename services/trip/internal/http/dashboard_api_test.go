package httpapi_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The dashboard endpoints, end to end over the same harness the participation
// tests use: the real router, the real store over a real PostGIS database, and
// a real HTTP stand-in for identity's batch resolver.
//
// The store tests cover which rows each tab returns. What is asserted here is
// the surface — the status codes, the field names and the two optional keys
// that a client of the already-shipped discovery endpoint must never see
// appear.

// myTrips runs one dashboard query and asserts it succeeded.
func (a *joinAPI) myTrips(user uuid.UUID, query string) map[string]any {
	a.t.Helper()

	res := a.do(http.MethodGet, "/api/my/trips?"+query, user, nil)
	require.Equalf(a.t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())
	return decode(a.t, res)
}

// items pulls the rows out of a page, each still a raw map so that a test can
// assert a key is *absent* and not merely zero.
func items(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()

	raw, ok := body["items"].([]any)
	require.True(t, ok, "a page must carry an items list")

	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(map[string]any)
		require.True(t, ok)
		out = append(out, row)
	}
	return out
}

func titlesOfItems(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row["title"].(string))
	}
	return out
}

// ---------------------------------------------------------------------------
// The query string
// ---------------------------------------------------------------------------

func TestMyTripsRequiresAToken(t *testing.T) {
	a := newJoinAPI(t)

	res := a.do(http.MethodGet, "/api/my/trips?role=organizer", uuid.Nil, nil)

	require.Equal(t, http.StatusUnauthorized, res.Code)
	require.Equal(t, "Bearer", res.Header().Get("WWW-Authenticate"))
	require.Equal(t, "invalid_access_token", errorOf(t, res)["code"])
}

func TestMyTripsRequiresExactlyOneOfTwoRoles(t *testing.T) {
	a := newJoinAPI(t)
	caller := uuid.New()

	for _, query := range []string{
		"",                                // absent
		"role=",                           // present and empty
		"role=Organizer",                  // the vocabulary is case-sensitive
		"role=member",                     // a plausible guess that is not a role
		"role=organizer&role=participant", // both at once
	} {
		t.Run("?"+query, func(t *testing.T) {
			res := a.do(http.MethodGet, "/api/my/trips?"+query, caller, nil)

			require.Equalf(t, http.StatusBadRequest, res.Code, "unexpected body: %s", res.Body.String())
			require.Contains(t, fieldPaths(t, res), "role")
		})
	}
}

func TestMyTripsRejectsAnUnrecognisedParameter(t *testing.T) {
	a := newJoinAPI(t)

	// `statuses` rather than `status`: a client whose filter is silently
	// ignored has been told it worked.
	res := a.do(http.MethodGet, "/api/my/trips?role=organizer&statuses=draft", uuid.New(), nil)

	require.Equal(t, http.StatusBadRequest, res.Code)
	require.Contains(t, fieldPaths(t, res), "statuses")
}

func TestMyTripsRejectsAnUnknownStatusAndAnOutOfRangeLimit(t *testing.T) {
	a := newJoinAPI(t)
	caller := uuid.New()

	for _, tc := range []struct{ query, field string }{
		{"role=organizer&status=archived", "status"},
		{"role=organizer&status=draft,archived", "status"},
		{"role=organizer&limit=0", "limit"},
		{"role=organizer&limit=51", "limit"},
		{"role=organizer&limit=many", "limit"},
		{"role=organizer&cursor=not-a-cursor", "cursor"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			res := a.do(http.MethodGet, "/api/my/trips?"+tc.query, caller, nil)

			require.Equalf(t, http.StatusBadRequest, res.Code, "unexpected body: %s", res.Body.String())
			require.Contains(t, fieldPaths(t, res), tc.field)
		})
	}
}

// ---------------------------------------------------------------------------
// The two tabs
// ---------------------------------------------------------------------------

func TestMyTripsOrganizerTabCarriesDraftsAndAPendingCount(t *testing.T) {
	a := newJoinAPI(t)

	organizer := uuid.New()
	applicant := uuid.New()

	draft := a.createTrip(organizer, func(body map[string]any) {
		body["title"] = "Still a draft"
	})
	recruiting := a.recruiting(organizer, 4)
	a.request(recruiting, applicant, nil)

	rows := items(t, a.myTrips(organizer, "role=organizer"))
	require.Len(t, rows, 2)

	byTitle := map[string]map[string]any{}
	for _, row := range rows {
		byTitle[row["title"].(string)] = row
	}

	// The draft is here and nowhere else in the API.
	require.Contains(t, byTitle, "Still a draft")
	require.Equal(t, draft["id"], byTitle["Still a draft"]["id"])
	require.Equal(t, "draft", byTitle["Still a draft"]["status"])
	require.Equal(t, float64(0), byTitle["Still a draft"]["pending_requests_count"])

	published := byTitle["Carpathians in autumn"]
	require.Equal(t, recruiting, published["id"])
	require.Equal(t, float64(1), published["pending_requests_count"],
		"the waiting applicant is what the dashboard renders the approve/reject list from")

	for _, row := range rows {
		require.Equal(t, "organizer", row["membership_status"])
		// The rest of the card is a search card, unchanged.
		require.Contains(t, row, "route_summary")
		require.Contains(t, row, "organizer")
		require.Contains(t, row, "free_slots")
	}
}

func TestMyTripsParticipantTabDistinguishesApprovedFromRequested(t *testing.T) {
	a := newJoinAPI(t)

	organizer := uuid.New()
	traveller := uuid.New()

	joined := a.recruiting(organizer, 4)
	a.join(joined, organizer, traveller)

	// A second trip, departing later, that they have only applied to.
	later := a.createTrip(organizer, func(body map[string]any) {
		body["title"] = "Only asked"
		body["start_at"] = shiftDays(t, body["start_at"].(string), 10)
		body["end_at"] = shiftDays(t, body["end_at"].(string), 10)
	})
	res := a.do(http.MethodPost, tripPath(later, "/publish"), organizer, nil)
	require.Equal(t, http.StatusOK, res.Code)
	a.request(later["id"].(string), traveller, nil)

	rows := items(t, a.myTrips(traveller, "role=participant"))

	// Newest departure first: this is a history view, not a discovery feed.
	require.Equal(t, []string{"Only asked", "Carpathians in autumn"}, titlesOfItems(rows))
	require.Equal(t, "requested", rows[0]["membership_status"])
	require.Equal(t, "approved", rows[1]["membership_status"])

	// How many other people are queueing is the organizer's business.
	for _, row := range rows {
		require.NotContains(t, row, "pending_requests_count")
	}

	// And the trips they organize are not on this tab.
	require.Empty(t, items(t, a.myTrips(organizer, "role=participant")))
}

func TestMyTripsPagesWithTheSameCursorFormatAsSearch(t *testing.T) {
	a := newJoinAPI(t)
	organizer := uuid.New()

	for i := range 5 {
		a.createTrip(organizer, func(body map[string]any) {
			body["title"] = fmt.Sprintf("Trip %d", i)
			body["start_at"] = shiftDays(t, body["start_at"].(string), i*3)
			body["end_at"] = shiftDays(t, body["end_at"].(string), i*3)
		})
	}

	var titles []string
	seen := map[string]bool{}
	query := "role=organizer&limit=2"

	for page := 0; ; page++ {
		require.Less(t, page, 20, "pagination did not terminate")

		body := a.myTrips(organizer, query)
		rows := items(t, body)
		require.LessOrEqual(t, len(rows), 2)

		for _, title := range titlesOfItems(rows) {
			require.Falsef(t, seen[title], "%q was returned on two different pages", title)
			seen[title] = true
			titles = append(titles, title)
		}

		next, ok := body["next_cursor"].(string)
		if !ok {
			require.Nil(t, body["next_cursor"], "the last page's cursor is null, not a string")
			break
		}
		query = "role=organizer&limit=2&cursor=" + url.QueryEscape(next)
	}

	require.Equal(t, []string{"Trip 4", "Trip 3", "Trip 2", "Trip 1", "Trip 0"}, titles)
}

// The two dashboard fields are additive. GET /api/trips is a shipped contract
// and a client that has never heard of them must keep working, which means they
// have to be absent from a search result rather than null in it.
func TestSearchResultsDoNotCarryTheDashboardFields(t *testing.T) {
	a := newJoinAPI(t)

	organizer := uuid.New()
	a.recruiting(organizer, 4)

	res := a.do(http.MethodGet, "/api/trips", uuid.New(), nil)
	require.Equal(t, http.StatusOK, res.Code)

	rows := items(t, decode(t, res))
	require.NotEmpty(t, rows)
	for _, row := range rows {
		require.NotContains(t, row, "membership_status")
		require.NotContains(t, row, "pending_requests_count")
	}
}

// ---------------------------------------------------------------------------
// Viewer context on GET /api/trips/{id}
// ---------------------------------------------------------------------------

// viewerOf reads the viewer block off a trip detail response.
func (a *joinAPI) viewerOf(tripID string, user uuid.UUID) map[string]any {
	a.t.Helper()

	res := a.do(http.MethodGet, "/api/trips/"+tripID, user, nil)
	require.Equalf(a.t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())

	body := decode(a.t, res)
	viewer, ok := body["viewer"].(map[string]any)
	require.True(a.t, ok, "the detail endpoint must carry a viewer block: %s", res.Body.String())
	return viewer
}

func TestViewerContextForEveryKindOfCaller(t *testing.T) {
	a := newJoinAPI(t)

	organizer := uuid.New()
	participant := uuid.New()
	requester := uuid.New()
	rejected := uuid.New()
	stranger := uuid.New()

	tripID := a.recruiting(organizer, 6)
	a.join(tripID, organizer, participant)
	a.request(tripID, requester, nil)

	turnedDown := a.request(tripID, rejected, nil)
	res := a.do(http.MethodPost,
		fmt.Sprintf("/api/trips/%s/requests/%s/reject", tripID, turnedDown["id"]), organizer, nil)
	require.Equalf(t, http.StatusOK, res.Code, "unexpected body: %s", res.Body.String())

	for _, tc := range []struct {
		name          string
		user          uuid.UUID
		isOrganizer   bool
		isParticipant bool
		requestStatus any
	}{
		// The organizer occupies a seat, so they are a participant too. A
		// client deciding what button to draw reads is_organizer first.
		{"organizer", organizer, true, true, nil},
		// Their request was approved and became the participants row; the
		// paperwork is no longer the answer to anything.
		{"approved participant", participant, false, true, nil},
		{"pending requester", requester, false, false, "pending"},
		{"rejected requester", rejected, false, false, "rejected"},
		{"stranger", stranger, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viewer := a.viewerOf(tripID, tc.user)

			require.Equal(t, tc.isOrganizer, viewer["is_organizer"])
			require.Equal(t, tc.isParticipant, viewer["is_participant"])

			// Present and null, never absent: a client must not have to tell an
			// absent key from a null one.
			require.Contains(t, viewer, "join_request_status")
			require.Equal(t, tc.requestStatus, viewer["join_request_status"])
		})
	}
}

func TestViewerContextForgetsAWithdrawnRequest(t *testing.T) {
	a := newJoinAPI(t)

	organizer := uuid.New()
	traveller := uuid.New()

	tripID := a.recruiting(organizer, 4)
	a.request(tripID, traveller, nil)

	res := a.do(http.MethodDelete, "/api/trips/"+tripID+"/requests/me", traveller, nil)
	require.Equal(t, http.StatusNoContent, res.Code)

	// Withdrawing puts the caller back where they started: they may ask again,
	// and the detail page must offer "Request to join" rather than a status.
	viewer := a.viewerOf(tripID, traveller)
	require.Equal(t, false, viewer["is_participant"])
	require.Nil(t, viewer["join_request_status"])
}

// The viewer block is answered after the visibility check, so a draft is still
// a 404 to everybody but its organizer — a caller who may not know a trip
// exists must not learn anything about it, including what they did to it.
func TestViewerContextIsNotAWayIntoSomebodyElsesDraft(t *testing.T) {
	a := newJoinAPI(t)

	organizer := uuid.New()
	trip := a.createTrip(organizer)

	res := a.do(http.MethodGet, tripPath(trip), uuid.New(), nil)
	require.Equal(t, http.StatusNotFound, res.Code)
	require.Equal(t, "trip_not_found", errorOf(t, res)["code"])

	// Its organizer sees it, with a viewer block that says so.
	viewer := a.viewerOf(trip["id"].(string), organizer)
	require.Equal(t, true, viewer["is_organizer"])
	require.Equal(t, true, viewer["is_participant"])
	require.Nil(t, viewer["join_request_status"])
}

// The mutations that return a trip do not carry a viewer block: publishing a
// trip leaves nobody wondering whether they organize it.
func TestOnlyTheDetailEndpointCarriesAViewerBlock(t *testing.T) {
	a := newJoinAPI(t)

	organizer := uuid.New()
	trip := a.createTrip(organizer)
	require.NotContains(t, trip, "viewer")

	res := a.do(http.MethodPost, tripPath(trip, "/publish"), organizer, nil)
	require.Equal(t, http.StatusOK, res.Code)
	require.NotContains(t, decode(t, res), "viewer")
}

// shiftDays moves an RFC 3339 timestamp forward, so a fixture can space several
// trips apart without rebuilding the whole body. Departure order is what the
// dashboard sorts by, and every trip in a fixture leaving on the same day would
// make the assertions depend on which uuid happened to sort higher.
func shiftDays(t *testing.T, timestamp string, days int) string {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, timestamp)
	require.NoError(t, err)
	return parsed.AddDate(0, 0, days).Format(time.RFC3339)
}
