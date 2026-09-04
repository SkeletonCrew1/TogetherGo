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
)

// The dashboard tests build a small world by hand rather than reusing
// DiscoveryWorld.
//
// Discovery's fixture is fifty trips chosen so that every geographic and
// full-text filter has something to find; none of that matters here. What
// matters is *relationships* — who organizes what, who was approved onto what,
// who is still waiting — and those cannot be seeded by the discovery helper at
// all, because they are written by the join-request flow and not by CreateTrip.
// So the world below is a dozen trips with the relationships spelled out, and
// every one of them goes through the real store methods.

type dashboard struct {
	t     *testing.T
	store *store.Store
	pool  *pgxpool.Pool
	ctx   context.Context

	title map[uuid.UUID]string
	id    map[string]uuid.UUID
}

func newDashboard(t *testing.T) *dashboard {
	t.Helper()

	s, pool := newStore(t)
	return &dashboard{
		t:     t,
		store: s,
		pool:  pool,
		ctx:   context.Background(),
		title: map[uuid.UUID]string{},
		id:    map[string]uuid.UUID{},
	}
}

// trip creates a draft and walks it to `status` through the real state machine.
//
// startDays spaces the departures apart so that (start_at DESC, id DESC) is a
// total order the assertions can be written against without knowing which uuid
// happened to sort higher.
func (d *dashboard) trip(title string, organizer uuid.UUID, startDays int, status domain.Status) uuid.UUID {
	d.t.Helper()

	in := validInput()
	in.Title = title
	in.StartAt = now.Add(time.Duration(startDays) * 24 * time.Hour)
	in.EndAt = in.StartAt.Add(48 * time.Hour)

	detail, err := d.store.CreateTrip(d.ctx, organizer, in)
	require.NoErrorf(d.t, err, "creating %q", title)

	d.title[detail.Trip.ID] = title
	d.id[title] = detail.Trip.ID

	d.advance(detail.Trip.ID, organizer, status)
	return detail.Trip.ID
}

// advance walks a trip from draft to `status`. The state machine has no
// shortcuts, so a completed trip really is published, started and finished.
func (d *dashboard) advance(tripID, organizer uuid.UUID, status domain.Status) {
	d.t.Helper()

	var path []domain.Status
	switch status {
	case domain.StatusDraft:
	case domain.StatusRecruiting:
		path = []domain.Status{domain.StatusRecruiting}
	case domain.StatusCancelled:
		path = []domain.Status{domain.StatusCancelled}
	case domain.StatusInProgress:
		path = []domain.Status{domain.StatusRecruiting, domain.StatusInProgress}
	case domain.StatusCompleted:
		path = []domain.Status{domain.StatusRecruiting, domain.StatusInProgress, domain.StatusCompleted}
	default:
		d.t.Fatalf("unknown status %q", status)
	}

	d.promote(tripID, organizer, path...)
}

// promote walks a trip through the transitions named, from wherever it is now.
// Unlike advance it assumes nothing about the starting state, which is what a
// trip that has already been published and joined needs.
func (d *dashboard) promote(tripID, organizer uuid.UUID, path ...domain.Status) {
	d.t.Helper()

	for _, to := range path {
		_, err := d.store.ChangeStatus(d.ctx, tripID, organizer, to)
		require.NoErrorf(d.t, err, "%s -> %s", d.title[tripID], to)
	}
}

// request leaves a pending application on a trip.
func (d *dashboard) request(tripID, user uuid.UUID) uuid.UUID {
	d.t.Helper()

	req, err := d.store.CreateJoinRequest(d.ctx, tripID, user, nil)
	require.NoErrorf(d.t, err, "%s requesting %s", user, d.title[tripID])
	return req.ID
}

// join applies and is approved, which is the only way a participants row with
// role 'participant' comes into existence.
func (d *dashboard) join(tripID, organizer, user uuid.UUID) {
	d.t.Helper()

	requestID := d.request(tripID, user)
	_, err := d.store.ApproveJoinRequest(d.ctx, tripID, requestID, organizer)
	require.NoErrorf(d.t, err, "approving %s onto %s", user, d.title[tripID])
}

// my runs one dashboard query, normalised and validated first so that no test
// asserts the behaviour of a filter the API would have refused.
func (d *dashboard) my(user uuid.UUID, filter domain.MyTripsFilter) *domain.SearchPage {
	d.t.Helper()

	filter = filter.Normalize()
	require.NoError(d.t, filter.Validate())

	page, err := d.store.MyTrips(d.ctx, user, filter)
	require.NoError(d.t, err)
	return page
}

// titlesOf names the trips on a page, in the order they came back.
func (d *dashboard) titlesOf(items []domain.TripListItem) []string {
	d.t.Helper()

	out := make([]string, 0, len(items))
	for _, item := range items {
		name, known := d.title[item.ID]
		require.Truef(d.t, known, "result %s is not one of the seeded trips", item.ID)
		out = append(out, name)
	}
	return out
}

// membership indexes a page by title, so an assertion about one row does not
// depend on its position.
func (d *dashboard) membership(items []domain.TripListItem) map[string]domain.MembershipStatus {
	d.t.Helper()

	out := make(map[string]domain.MembershipStatus, len(items))
	for _, item := range items {
		out[d.title[item.ID]] = item.Membership
	}
	return out
}

// --- the world --------------------------------------------------------------

// people is the cast. Named so the assertions read as sentences.
type people struct {
	organizer uuid.UUID
	other     uuid.UUID
	traveller uuid.UUID
	bystander uuid.UUID
	stranger  uuid.UUID
}

// seedWorld builds the fixture the tab tests assert against:
//
//	organizer  runs a draft, a recruiting trip, a completed one and a cancelled one
//	other      runs a recruiting trip and a draft of their own
//	traveller  is approved on organizer's recruiting trip and waiting on other's
//	bystander  is waiting on organizer's recruiting trip, so it has a queue of 1
//	stranger   has nothing to do with any of it
//
// Departure days are spaced so the newest-first ordering is unambiguous.
func seedWorld(d *dashboard) people {
	d.t.Helper()

	p := people{
		organizer: uuid.New(),
		other:     uuid.New(),
		traveller: uuid.New(),
		bystander: uuid.New(),
		stranger:  uuid.New(),
	}

	d.trip("organizer draft", p.organizer, 40, domain.StatusDraft)
	recruiting := d.trip("organizer recruiting", p.organizer, 30, domain.StatusRecruiting)
	d.trip("organizer completed", p.organizer, 20, domain.StatusCompleted)
	d.trip("organizer cancelled", p.organizer, 10, domain.StatusCancelled)

	otherRecruiting := d.trip("other recruiting", p.other, 25, domain.StatusRecruiting)
	d.trip("other draft", p.other, 15, domain.StatusDraft)

	d.join(recruiting, p.organizer, p.traveller)
	d.request(recruiting, p.bystander)
	d.request(otherRecruiting, p.traveller)

	return p
}

// --- the tabs ---------------------------------------------------------------

func TestMyTripsOrganizerTabReturnsEveryStatusIncludingDrafts(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	page := d.my(p.organizer, domain.MyTripsFilter{Role: domain.DashboardOrganizer})

	// Newest departure first, and the draft and the cancelled trip are both on
	// it. This is the only listing in the service that shows either.
	require.Equal(t, []string{
		"organizer draft",
		"organizer recruiting",
		"organizer completed",
		"organizer cancelled",
	}, d.titlesOf(page.Items))
	require.Nil(t, page.Next)

	for _, item := range page.Items {
		require.Equal(t, domain.MembershipOrganizer, item.Membership,
			"%s", d.title[item.ID])
	}
}

func TestMyTripsOrganizerTabCountsPendingRequests(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	page := d.my(p.organizer, domain.MyTripsFilter{Role: domain.DashboardOrganizer})

	counts := map[string]int{}
	for _, item := range page.Items {
		require.NotNilf(t, item.PendingRequests, "%s has no count", d.title[item.ID])
		counts[d.title[item.ID]] = *item.PendingRequests
	}

	// One waiting applicant on the recruiting trip: the traveller's request was
	// approved and is no longer pending, the bystander's is. Zero everywhere
	// else — and zero is a number the row carries, not a missing field.
	require.Equal(t, map[string]int{
		"organizer draft":      0,
		"organizer recruiting": 1,
		"organizer completed":  0,
		"organizer cancelled":  0,
	}, counts)
}

func TestMyTripsParticipantTabUnionsApprovedAndRequested(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	page := d.my(p.traveller, domain.MyTripsFilter{Role: domain.DashboardParticipant})

	// Both halves of the union, in departure order: the trip they were approved
	// onto and the one they are still waiting on.
	require.Equal(t, []string{"organizer recruiting", "other recruiting"},
		d.titlesOf(page.Items))
	require.Equal(t, map[string]domain.MembershipStatus{
		"organizer recruiting": domain.MembershipApproved,
		"other recruiting":     domain.MembershipRequested,
	}, d.membership(page.Items))

	// The pending count is the organizer's business and is absent here, even on
	// a trip that has one.
	for _, item := range page.Items {
		require.Nilf(t, item.PendingRequests, "%s leaked a pending count", d.title[item.ID])
	}
}

func TestMyTripsParticipantTabExcludesTripsTheCallerOrganizes(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	// The organizer has a participants row on every trip they run. Without the
	// explicit exclusion every one of them would appear on both tabs.
	page := d.my(p.organizer, domain.MyTripsFilter{Role: domain.DashboardParticipant})
	require.Empty(t, d.titlesOf(page.Items))
}

func TestMyTripsNeverShowsDraftsToAnyoneButTheirOrganizer(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	// A draft is unfindable, so nobody can apply to one through the API. The
	// row is written directly to prove the exclusion is a rule of the query and
	// not a consequence of nothing having produced the state yet.
	draft := d.id["other draft"]
	_, err := d.pool.Exec(d.ctx, `
		INSERT INTO join_requests (id, trip_id, user_id, status)
		VALUES ($1, $2, $3, 'pending')`, uuid.New(), draft, p.traveller)
	require.NoError(t, err)

	_, err = d.pool.Exec(d.ctx, `
		INSERT INTO participants (trip_id, user_id, role)
		VALUES ($1, $2, $3)`, draft, p.bystander, domain.RoleParticipant)
	require.NoError(t, err)

	for _, user := range []uuid.UUID{p.traveller, p.bystander} {
		page := d.my(user, domain.MyTripsFilter{Role: domain.DashboardParticipant})
		require.NotContains(t, d.titlesOf(page.Items), "other draft")
		require.NotContains(t, d.titlesOf(page.Items), "organizer draft")
	}

	// And somebody else's draft is not on their organizer tab either: that tab
	// is `organizer_id = you` and nothing else.
	page := d.my(p.other, domain.MyTripsFilter{Role: domain.DashboardOrganizer})
	require.Equal(t, []string{"other recruiting", "other draft"}, d.titlesOf(page.Items))
	require.NotContains(t, d.titlesOf(page.Items), "organizer draft")
}

func TestMyTripsIsEmptyForSomebodyWithNoTrips(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	for _, role := range domain.DashboardRoles {
		page := d.my(p.stranger, domain.MyTripsFilter{Role: role})
		require.Emptyf(t, page.Items, "role=%s", role)
		require.Nilf(t, page.Next, "role=%s", role)
	}
}

func TestMyTripsFiltersByStatus(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	page := d.my(p.organizer, domain.MyTripsFilter{
		Role:     domain.DashboardOrganizer,
		Statuses: []domain.Status{domain.StatusDraft, domain.StatusCancelled},
	})
	require.Equal(t, []string{"organizer draft", "organizer cancelled"}, d.titlesOf(page.Items))
}

// --- pagination -------------------------------------------------------------

// walk pages through a filter and returns every title in order, asserting that
// nothing is repeated and that the walk terminates.
func (d *dashboard) walk(user uuid.UUID, filter domain.MyTripsFilter) []string {
	d.t.Helper()

	var titles []string
	seen := map[string]bool{}

	for page := 0; ; page++ {
		require.Less(d.t, page, 100, "pagination did not terminate")

		result := d.my(user, filter)
		require.LessOrEqual(d.t, len(result.Items), filter.PageSize())

		for _, name := range d.titlesOf(result.Items) {
			require.Falsef(d.t, seen[name], "%q was returned on two different pages", name)
			seen[name] = true
			titles = append(titles, name)
		}

		if result.Next == nil {
			return titles
		}
		filter.Cursor = result.Next
	}
}

func TestMyTripsPaginatesTheUnionWithoutGapsOrDuplicates(t *testing.T) {
	d := newDashboard(t)

	organizer := uuid.New()
	traveller := uuid.New()

	// Seven trips, alternating between the two branches of the union and
	// between statuses, so that a page boundary is guaranteed to fall inside a
	// run of one branch *and* to cross from one branch to the other. This is
	// the case the keyset has to survive: applied per branch instead of to the
	// merged set, the cursor from an `approved` row would resume the
	// `requested` branch from a position it never reached.
	//
	// Departure days descend, so the seeded order is the expected order.
	kinds := []struct {
		title    string
		approved bool
		// then is what the trip is walked through after the roster is settled,
		// since approval is only legal while a trip is recruiting.
		then []domain.Status
	}{
		{title: "day 70 approved", approved: true},
		{title: "day 60 requested"},
		{title: "day 50 approved", approved: true, then: []domain.Status{domain.StatusInProgress}},
		{title: "day 40 approved", approved: true, then: []domain.Status{domain.StatusInProgress, domain.StatusCompleted}},
		{title: "day 30 requested"},
		{title: "day 20 requested"},
		{title: "day 10 approved", approved: true},
	}

	expected := make([]string, 0, len(kinds))
	for i, kind := range kinds {
		// Created as recruiting first: approval is only legal there, and a trip
		// that is under way or finished still has the participants row that
		// approval wrote.
		id := d.trip(kind.title, organizer, 70-i*10, domain.StatusRecruiting)
		if kind.approved {
			d.join(id, organizer, traveller)
		} else {
			d.request(id, traveller)
		}
		d.promote(id, organizer, kind.then...)
		expected = append(expected, kind.title)
	}

	limit := 2
	titles := d.walk(traveller, domain.MyTripsFilter{
		Role:  domain.DashboardParticipant,
		Limit: &limit,
	})

	// Every trip, exactly once, newest departure first — the same list a single
	// unpaginated query returns.
	require.Equal(t, expected, titles)

	whole := d.my(traveller, domain.MyTripsFilter{Role: domain.DashboardParticipant})
	require.Equal(t, expected, d.titlesOf(whole.Items))
}

func TestMyTripsPaginatesTheOrganizerTab(t *testing.T) {
	d := newDashboard(t)

	organizer := uuid.New()
	expected := make([]string, 0, 5)
	for i, status := range []domain.Status{
		domain.StatusDraft,
		domain.StatusRecruiting,
		domain.StatusCancelled,
		domain.StatusCompleted,
		domain.StatusRecruiting,
	} {
		title := string(rune('A'+i)) + " trip"
		d.trip(title, organizer, 50-i*10, status)
		expected = append(expected, title)
	}

	limit := 2
	titles := d.walk(organizer, domain.MyTripsFilter{
		Role:  domain.DashboardOrganizer,
		Limit: &limit,
	})
	require.Equal(t, expected, titles)
}

// A user who was approved, left, and applied again holds both a participants
// row and a pending request. The union has to answer with one row for that
// trip, not two: a duplicate is wrong on its face, and it is fatal to the
// cursor, because the two copies share a (start_at, id) that no comparison can
// separate.
func TestMyTripsCollapsesATripThatMatchesBothBranches(t *testing.T) {
	d := newDashboard(t)

	organizer := uuid.New()
	traveller := uuid.New()

	tripID := d.trip("rejoined", organizer, 30, domain.StatusRecruiting)
	d.join(tripID, organizer, traveller)

	// A second, pending application alongside the participants row. It cannot
	// be made through the API — CreateJoinRequest refuses an existing
	// participant — which is why it is written directly.
	_, err := d.pool.Exec(d.ctx, `
		INSERT INTO join_requests (id, trip_id, user_id, status)
		VALUES ($1, $2, $3, 'pending')`, uuid.New(), tripID, traveller)
	require.NoError(t, err)

	page := d.my(traveller, domain.MyTripsFilter{Role: domain.DashboardParticipant})

	require.Equal(t, []string{"rejoined"}, d.titlesOf(page.Items))
	// The stronger standing wins: they are on the trip, whatever else is open.
	require.Equal(t, domain.MembershipApproved, page.Items[0].Membership)
}

// --- the list card ----------------------------------------------------------

func TestMyTripsCarriesTheSameCardAsSearch(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	page := d.my(p.organizer, domain.MyTripsFilter{
		Role:     domain.DashboardOrganizer,
		Statuses: []domain.Status{domain.StatusRecruiting},
	})
	require.Len(t, page.Items, 1)
	item := page.Items[0]

	// The route summary and the denormalised endpoints, exactly as a discovery
	// card carries them — this is the same serializer and the same query shape.
	require.Equal(t, "Lviv", item.DepartureName)
	require.Equal(t, "Hoverla", item.DestinationName)
	require.Equal(t, []string{"Lviv", "Vorokhta", "Hoverla"}, item.Route.Points)
	require.Equal(t, 3, item.Route.TotalPoints)
	require.False(t, item.Route.Truncated())

	require.Equal(t, p.organizer, item.OrganizerID)
	require.Equal(t, 8, item.Capacity)
	// The organizer plus the traveller they approved.
	require.Equal(t, 2, item.ApprovedCount)
	require.Equal(t, 6, item.SpotsLeft())
}

// --- the viewer's join request ----------------------------------------------

func TestViewerJoinRequestReportsTheLatestApplication(t *testing.T) {
	d := newDashboard(t)
	p := seedWorld(d)

	recruiting := d.id["organizer recruiting"]

	// Never asked.
	status, err := d.store.ViewerJoinRequest(d.ctx, recruiting, p.stranger)
	require.NoError(t, err)
	require.Nil(t, status)

	// Waiting.
	status, err = d.store.ViewerJoinRequest(d.ctx, recruiting, p.bystander)
	require.NoError(t, err)
	require.NotNil(t, status)
	require.Equal(t, domain.JoinPending, *status)

	// Approved — the request is decided and the participants row is the real
	// answer, which is domain.NewViewer's problem and not this query's.
	status, err = d.store.ViewerJoinRequest(d.ctx, recruiting, p.traveller)
	require.NoError(t, err)
	require.NotNil(t, status)
	require.Equal(t, domain.JoinApproved, *status)
}

func TestViewerJoinRequestPrefersTheMostRecentOfSeveral(t *testing.T) {
	d := newDashboard(t)

	organizer := uuid.New()
	traveller := uuid.New()
	tripID := d.trip("asked twice", organizer, 30, domain.StatusRecruiting)

	// Rejected in March, asks again in June. The partial unique index allows
	// the second application precisely because the first is no longer pending.
	requestID := d.request(tripID, traveller)
	_, err := d.store.RejectJoinRequest(d.ctx, tripID, requestID, organizer, nil)
	require.NoError(t, err)

	d.request(tripID, traveller)

	status, err := d.store.ViewerJoinRequest(d.ctx, tripID, traveller)
	require.NoError(t, err)
	require.NotNil(t, status)
	require.Equal(t, domain.JoinPending, *status)
}
