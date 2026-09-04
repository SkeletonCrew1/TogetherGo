package store_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/testsupport"
)

// The discovery tests run against one seeded world of fifty trips — real
// places, real coordinates, four trips that must never be returned — and each
// one asserts a single filter against the set the seeds say it should match.
//
// Expectations are computed from the seed list rather than written out as
// literals wherever the filter has a Go equivalent. A hand-written list of
// titles is a second implementation of the filter that drifts the moment the
// fixture changes; deriving it means the test asserts "the SQL agrees with the
// obvious answer over this data", which is the claim worth making.

type world struct {
	t     *testing.T
	store *store.Store
	pool  *pgxpool.Pool
	seeds []testsupport.TripSeed
	ids   map[string]uuid.UUID
	title map[uuid.UUID]string
}

func newWorld(t *testing.T) *world {
	t.Helper()

	pool := testsupport.Pool(t)
	seeds := testsupport.DiscoveryWorld([]uuid.UUID{uuid.New(), uuid.New(), uuid.New()})
	ids := testsupport.SeedTrips(t, pool, now, seeds)

	title := make(map[uuid.UUID]string, len(ids))
	for name, id := range ids {
		title[id] = name
	}

	return &world{
		t:     t,
		store: store.New(pool).WithClock(func() time.Time { return now }),
		pool:  pool,
		seeds: seeds,
		ids:   ids,
		title: title,
	}
}

// search normalizes and validates the filter before running it, so no test can
// accidentally assert the behaviour of a query the API would have refused.
func (w *world) search(filter domain.SearchFilter) *domain.SearchPage {
	w.t.Helper()

	filter = filter.Normalize()
	require.NoError(w.t, filter.Validate())

	page, err := w.store.SearchTrips(context.Background(), filter)
	require.NoError(w.t, err)
	return page
}

// titlesOf names the trips on a page, in the order they came back.
func (w *world) titlesOf(items []domain.TripListItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		name, known := w.title[item.ID]
		require.Truef(w.t, known, "result %s is not one of the seeded trips", item.ID)
		out = append(out, name)
	}
	return out
}

// expect is the set of seed titles that a predicate selects, with the two rules
// discovery applies unconditionally — recruiting, and not yet departed —
// already taken into account.
func (w *world) expect(match func(testsupport.TripSeed) bool) []string {
	out := make([]string, 0, len(w.seeds))
	for _, seed := range w.seeds {
		if seed.StartsInThePast {
			continue
		}
		if seed.Status != "" && seed.Status != domain.StatusRecruiting {
			continue
		}
		if match(seed) {
			out = append(out, seed.Title)
		}
	}
	return out
}

// all walks every page of a filter, so an assertion about a filter is never
// silently an assertion about the first twenty rows.
func (w *world) all(filter domain.SearchFilter) []string {
	w.t.Helper()

	var titles []string
	seen := map[string]bool{}
	for page := 0; ; page++ {
		require.Less(w.t, page, 100, "pagination did not terminate")

		result := w.search(filter)
		for _, name := range w.titlesOf(result.Items) {
			require.Falsef(w.t, seen[name], "%q was returned on two different pages", name)
			seen[name] = true
			titles = append(titles, name)
		}
		if result.Next == nil {
			return titles
		}
		filter.Cursor = result.Next
	}
}

func everySeed(testsupport.TripSeed) bool { return true }

// TestSearchAppliesTheDiscoveryScope: the two rules that are not filters.
//
// The fixture holds one draft, one cancelled, one completed and one already
// departed trip, all of them departing from inside the radius the other tests
// search and all of them mentioning Hoverla, so a query that lost a rule would
// fail this test and several others at once.
func TestSearchAppliesTheDiscoveryScope(t *testing.T) {
	w := newWorld(t)

	got := w.all(domain.SearchFilter{})
	require.ElementsMatch(t, w.expect(everySeed), got)

	for _, excluded := range []string{
		"Hoverla winter draft",
		"Cancelled Hoverla hike",
		"Completed Hoverla hike",
		"Departed Hoverla hike",
	} {
		require.NotContains(t, got, excluded)
	}
}

// TestSearchOrdersBySoonestFirst — the ordering the cursor depends on.
func TestSearchOrdersBySoonestFirst(t *testing.T) {
	w := newWorld(t)

	page := w.search(domain.SearchFilter{Limit: pageOf(domain.SearchLimitMax)})
	require.NotEmpty(t, page.Items)

	for i := 1; i < len(page.Items); i++ {
		previous, current := page.Items[i-1], page.Items[i]
		require.Falsef(t, current.StartAt.Before(previous.StartAt),
			"item %d starts before item %d", i, i-1)
		if current.StartAt.Equal(previous.StartAt) {
			require.Less(t, previous.ID.String(), current.ID.String(),
				"trips sharing a start_at must be ordered by id")
		}
	}
}

// TestSearchByDepartureRadius is the first acceptance criterion: 50 km around
// Ivano-Frankivsk finds the Hoverla hike and does not find the Kyiv trip.
func TestSearchByDepartureRadius(t *testing.T) {
	w := newWorld(t)

	got := w.all(domain.SearchFilter{Near: near(testsupport.IvanoFrankivsk, 50)})

	require.Contains(t, got, "Hoverla summit hike")
	require.NotContains(t, got, "Kyiv street food crawl")

	// The fixture is built so that exactly three trips depart from inside this
	// circle: Ivano-Frankivsk itself, Kalush at 27 km and Halych at 22 km.
	// Yaremche (54 km) and Bukovel (66 km) are the near misses that make the
	// radius mean something.
	require.ElementsMatch(t, []string{
		"Hoverla summit hike",
		"Carpathian long traverse",
		"Halych castle day trip",
	}, got)
}

// TestSearchByDepartureRadiusScales: widening the circle to 100 km picks up the
// trips that were just outside it, and no others.
func TestSearchByDepartureRadiusScales(t *testing.T) {
	w := newWorld(t)

	fifty := w.all(domain.SearchFilter{Near: near(testsupport.IvanoFrankivsk, 50)})
	hundred := w.all(domain.SearchFilter{Near: near(testsupport.IvanoFrankivsk, 100)})

	require.Subset(t, hundred, fifty)
	require.Greater(t, len(hundred), len(fifty))
	require.Contains(t, hundred, "Bukovel ski week")
	require.NotContains(t, hundred, "Lviv coffee weekend", "Lviv is 115 km away")
}

// TestSearchByDestinationRadius filters on the other geography column.
func TestSearchByDestinationRadius(t *testing.T) {
	w := newWorld(t)

	got := w.all(domain.SearchFilter{Destination: near(testsupport.Hoverla, 5)})

	require.ElementsMatch(t, w.expect(func(seed testsupport.TripSeed) bool {
		return seed.Route[len(seed.Route)-1].Name == testsupport.Hoverla.Name
	}), got)
	require.Contains(t, got, "Hoverla summit hike")
}

// TestSearchByDateWindow: overlap, not containment. A trip that started before
// the window and is still running inside it is one the caller can join.
func TestSearchByDateWindow(t *testing.T) {
	w := newWorld(t)

	from := now.AddDate(0, 0, 20)
	to := now.AddDate(0, 0, 25)

	got := w.all(domain.SearchFilter{DateFrom: &from, DateTo: &to})
	require.NotEmpty(t, got)

	for _, name := range got {
		seed := w.seed(name)
		start, end := w.window(seed)
		require.Falsef(t, end.Before(from), "%q ends before the window opens", name)
		require.Falsef(t, start.After(to), "%q starts after the window closes", name)
	}

	// The long traverse leaves on day 30, outside the window, but only after
	// it: the half-open ends are checked separately.
	require.NotContains(t, got, "Carpathian long traverse")
	require.Contains(t, got, "Odesa seaside weekend", "it leaves on day 21")
}

func TestSearchByDateWindowIsOverlapNotContainment(t *testing.T) {
	w := newWorld(t)

	// A one-second window in the middle of the ten-day traverse. Nothing about
	// the trip is contained in it; the trip contains it.
	middle := w.mustStart("Carpathian long traverse").AddDate(0, 0, 5)
	end := middle.Add(time.Second)

	got := w.all(domain.SearchFilter{DateFrom: &middle, DateTo: &end})
	require.Contains(t, got, "Carpathian long traverse")
}

// TestSearchByDuration — days, counted as a person counts them.
func TestSearchByDuration(t *testing.T) {
	w := newWorld(t)

	sevenPlus := 7
	got := w.all(domain.SearchFilter{MinDays: &sevenPlus})
	require.ElementsMatch(t, w.expect(func(seed testsupport.TripSeed) bool {
		return seed.Days >= 7
	}), got)
	require.Contains(t, got, "Carpathian long traverse")
	require.NotContains(t, got, "Hoverla summit hike", "three days")

	two := 2
	got = w.all(domain.SearchFilter{MaxDays: &two})
	require.ElementsMatch(t, w.expect(func(seed testsupport.TripSeed) bool {
		return seed.Days <= 2
	}), got)
	require.Contains(t, got, "Kyiv street food crawl")

	three := 3
	got = w.all(domain.SearchFilter{MinDays: &three, MaxDays: &three})
	require.ElementsMatch(t, w.expect(func(seed testsupport.TripSeed) bool {
		return seed.Days == 3
	}), got)
	require.Contains(t, got, "Hoverla summit hike")
}

// TestSearchDurationRoundsUp: a Friday-evening-to-Sunday-afternoon trip is 2.1
// elapsed days and is advertised as a three-day weekend. min_days=3 has to find
// it, which is why the SQL takes a ceiling rather than a truncation.
func TestSearchDurationRoundsUp(t *testing.T) {
	w := newWorld(t)

	id := w.publish(domain.TripInput{
		Title:    "Long weekend by the lake",
		Category: "nature",
		Capacity: 4,
		StartAt:  now.AddDate(0, 0, 3).Add(18 * time.Hour),
		EndAt:    now.AddDate(0, 0, 5).Add(20 * time.Hour),
		Points: []domain.PointInput{
			{Name: testsupport.Lviv.Name, Lat: testsupport.Lviv.Lat, Lng: testsupport.Lviv.Lng},
			{Name: testsupport.LvivRynok.Name, Lat: testsupport.LvivRynok.Lat, Lng: testsupport.LvivRynok.Lng},
		},
	})

	three := 3
	page := w.search(domain.SearchFilter{MinDays: &three, MaxDays: &three, Limit: pageOf(domain.SearchLimitMax)})
	require.Contains(t, ids(page.Items), id, "2.08 elapsed days is a three-day trip")
}

// TestSearchByFreeSlots — capacity minus the people already on it.
func TestSearchByFreeSlots(t *testing.T) {
	w := newWorld(t)

	one := 1
	got := w.all(domain.SearchFilter{MinFreeSlots: &one})
	require.NotContains(t, got, "Kyiv street food crawl", "six of six seats are taken")

	five := 5
	got = w.all(domain.SearchFilter{MinFreeSlots: &five})
	require.ElementsMatch(t, w.expect(func(seed testsupport.TripSeed) bool {
		return seed.Capacity-max(seed.Approved, 1) >= 5
	}), got)
	require.Contains(t, got, "Hoverla summit hike", "eight seats, three taken")
}

// TestSearchByCategories — the allowlist, comma-separated by the caller.
func TestSearchByCategories(t *testing.T) {
	w := newWorld(t)

	got := w.all(domain.SearchFilter{Categories: []string{"hiking"}})
	require.ElementsMatch(t, w.expect(func(seed testsupport.TripSeed) bool {
		return seed.Category == "hiking"
	}), got)

	got = w.all(domain.SearchFilter{Categories: []string{"hiking", "food"}})
	require.ElementsMatch(t, w.expect(func(seed testsupport.TripSeed) bool {
		return seed.Category == "hiking" || seed.Category == "food"
	}), got)
	require.Contains(t, got, "Lviv coffee weekend")
}

// TestSearchByFreeText is the tsvector column doing its job.
//
// The fixture puts "Hoverla" in the title of four trips, three of which are out
// of scope, so a matching query that forgot the scope returns four rows here.
func TestSearchByFreeText(t *testing.T) {
	w := newWorld(t)

	require.Equal(t, []string{"Hoverla summit hike"},
		w.all(domain.SearchFilter{Query: "hoverla"}))

	require.Equal(t, []string{"Hoverla summit hike"},
		w.all(domain.SearchFilter{Query: "Chornohora"}),
		"the description is indexed too")

	require.Equal(t, []string{"Hoverla summit hike"},
		w.all(domain.SearchFilter{Query: "hikes"}),
		"'hikes' and 'hike' stem to the same lexeme — this is what ILIKE cannot do")

	require.Empty(t, w.all(domain.SearchFilter{Query: "kilimanjaro"}))
}

func TestSearchByFreeTextHandlesOperators(t *testing.T) {
	w := newWorld(t)

	// websearch_to_tsquery, so a user typing an unbalanced quote or a stray
	// parenthesis gets no results rather than a SQL error surfacing as a 500.
	for _, query := range []string{`"hoverla summit"`, `hoverla or kilimanjaro`, `hoverla -kilimanjaro`, `((("`, `& | !`} {
		filter := domain.SearchFilter{Query: query}.Normalize()
		require.NoError(t, filter.Validate())
		_, err := w.store.SearchTrips(context.Background(), filter)
		require.NoErrorf(t, err, "query %q", query)
	}

	require.Equal(t, []string{"Hoverla summit hike"},
		w.all(domain.SearchFilter{Query: `"summit hike"`}),
		"a quoted phrase matches as a phrase")
}

// TestSearchCombinesFilters — two filters, intersected.
func TestSearchCombinesFilters(t *testing.T) {
	w := newWorld(t)

	got := w.all(domain.SearchFilter{
		Categories: []string{"hiking"},
		Near:       near(testsupport.IvanoFrankivsk, 50),
	})
	require.ElementsMatch(t, []string{"Hoverla summit hike", "Carpathian long traverse"}, got)

	// The same radius with a category that has nothing inside it.
	require.Empty(t, w.all(domain.SearchFilter{
		Categories: []string{"abroad"},
		Near:       near(testsupport.IvanoFrankivsk, 50),
	}))
}

func TestSearchCombinesRadiusAndFreeText(t *testing.T) {
	w := newWorld(t)

	require.Equal(t, []string{"Hoverla summit hike"}, w.all(domain.SearchFilter{
		Query: "hoverla",
		Near:  near(testsupport.IvanoFrankivsk, 50),
	}))
	require.Empty(t, w.all(domain.SearchFilter{
		Query: "hoverla",
		Near:  near(testsupport.Kyiv, 50),
	}))
}

// TestSearchPaginationWalksTheWholeResultSet: limit=7, from the first page to
// the last, asserting no duplicates and no gaps.
func TestSearchPaginationWalksTheWholeResultSet(t *testing.T) {
	w := newWorld(t)

	expected := w.expect(everySeed)
	require.Greater(t, len(expected), 40, "the fixture has to be bigger than a page for this to mean anything")

	var walked []string
	var pages int
	filter := domain.SearchFilter{Limit: pageOf(7)}

	for {
		page := w.search(filter)
		pages++
		require.LessOrEqual(t, len(page.Items), 7)
		walked = append(walked, w.titlesOf(page.Items)...)

		if page.Next == nil {
			// The last page is the short one; every page before it was full.
			require.LessOrEqual(t, len(page.Items), 7)
			break
		}
		require.Len(t, page.Items, 7, "only the last page may be short")
		filter.Cursor = page.Next
	}

	require.Greater(t, pages, 5)
	require.ElementsMatch(t, expected, walked, "no gaps")
	require.Len(t, walked, len(expected), "no duplicates")
}

// TestSearchPaginationAcrossTiedStartTimes is the case a cursor holding only a
// timestamp gets wrong.
//
// Twenty trips leaving at three instants, with the page boundaries deliberately
// falling inside a group that shares one. A cursor of `start_at > x` skips the
// rest of the group; `start_at >= x` returns it forever. Only the (start_at,
// id) pair walks it exactly once.
func TestSearchPaginationAcrossTiedStartTimes(t *testing.T) {
	pool := testsupport.Pool(t)
	s := store.New(pool).WithClock(func() time.Time { return now })

	seeds := make([]testsupport.TripSeed, 0, 20)
	for i, day := range []int{10, 10, 10, 10, 10, 11, 11, 11, 11, 11, 11, 11, 11, 11, 12, 12, 12, 12, 12, 12} {
		seeds = append(seeds, testsupport.TripSeed{
			Title:       fmt.Sprintf("Tied trip %02d", i),
			Description: "Leaves at the same moment as several others.",
			Category:    "other", Capacity: 4, StartDay: day, Days: 1,
			Route: []testsupport.Place{testsupport.Lviv, testsupport.LvivRynok},
		})
	}
	ids := testsupport.SeedTrips(t, pool, now, seeds)

	byID := make(map[uuid.UUID]string, len(ids))
	for title, id := range ids {
		byID[id] = title
	}

	seen := map[uuid.UUID]bool{}
	var order []uuid.UUID
	filter := domain.SearchFilter{Limit: pageOf(7)}

	for page := 0; ; page++ {
		require.Less(t, page, 20, "pagination did not terminate")

		filter = filter.Normalize()
		require.NoError(t, filter.Validate())
		result, err := s.SearchTrips(context.Background(), filter)
		require.NoError(t, err)

		for _, item := range result.Items {
			require.Falsef(t, seen[item.ID], "%s returned twice", byID[item.ID])
			seen[item.ID] = true
			order = append(order, item.ID)
		}
		if result.Next == nil {
			break
		}
		filter.Cursor = result.Next
	}

	require.Len(t, order, len(seeds), "every trip exactly once: no gaps, no duplicates")

	// And the walk is in the total order the index provides, across the ties.
	previous := domain.Cursor{}
	for _, id := range order {
		trip, err := s.Trip(context.Background(), id)
		require.NoError(t, err)
		if !previous.StartAt.IsZero() {
			require.True(t,
				trip.StartAt.After(previous.StartAt) ||
					(trip.StartAt.Equal(previous.StartAt) && trip.ID.String() > previous.ID.String()),
				"the walk must be strictly increasing in (start_at, id)")
		}
		previous = domain.Cursor{StartAt: trip.StartAt, ID: trip.ID}
	}
}

// TestSearchPaginationSurvivesTheLimitBoundary: a result set that is an exact
// multiple of the page size must not end with an empty extra page.
func TestSearchPaginationSurvivesTheLimitBoundary(t *testing.T) {
	w := newWorld(t)

	total := len(w.expect(everySeed))
	page := w.search(domain.SearchFilter{Limit: pageOf(total)})
	require.Len(t, page.Items, total)
	require.Nil(t, page.Next, "a full last page is still the last page")
}

// TestSearchProjectsTheListItem — everything a card renders.
func TestSearchProjectsTheListItem(t *testing.T) {
	w := newWorld(t)

	item := w.find("Hoverla summit hike")

	require.Equal(t, "hiking", item.Category)
	require.Equal(t, domain.StatusRecruiting, item.Status)
	require.Equal(t, 8, item.Capacity)
	require.Equal(t, 3, item.ApprovedCount)
	require.Equal(t, 5, item.SpotsLeft())

	require.Equal(t, "Ivano-Frankivsk", item.DepartureName)
	require.InDelta(t, testsupport.IvanoFrankivsk.Lat, item.Departure.Lat, 1e-9)
	require.InDelta(t, testsupport.IvanoFrankivsk.Lng, item.Departure.Lng, 1e-9)

	require.Equal(t, "Hoverla", item.DestinationName)
	require.InDelta(t, testsupport.Hoverla.Lat, item.Destination.Lat, 1e-9)

	require.Equal(t, []string{"Ivano-Frankivsk", "Yaremche", "Hoverla"}, item.Route.Points)
	require.Equal(t, 3, item.Route.TotalPoints)
	require.False(t, item.Route.Truncated())
}

// TestSearchCapsTheRouteSummary — six names and a marker, whatever the route.
func TestSearchCapsTheRouteSummary(t *testing.T) {
	w := newWorld(t)

	route := []domain.PointInput{}
	for _, place := range []testsupport.Place{
		testsupport.Lviv, testsupport.Halych, testsupport.IvanoFrankivsk, testsupport.Kalush,
		testsupport.Yaremche, testsupport.Bukovel, testsupport.Hoverla, testsupport.Chernivtsi,
	} {
		route = append(route, domain.PointInput{Name: place.Name, Lat: place.Lat, Lng: place.Lng})
	}

	id := w.publish(domain.TripInput{
		Title:    "The whole ridge in one go",
		Category: "hiking",
		Capacity: 6,
		StartAt:  now.AddDate(0, 0, 2),
		EndAt:    now.AddDate(0, 0, 9),
		Points:   route,
	})

	item := w.findByID(id)
	require.Len(t, item.Route.Points, domain.RouteSummaryMax)
	require.Equal(t, 8, item.Route.TotalPoints)
	require.True(t, item.Route.Truncated())
	require.Equal(t, "Lviv", item.Route.Points[0], "the summary keeps the route's order")
	require.Equal(t, "Bukovel", item.Route.Points[domain.RouteSummaryMax-1])

	// The endpoints are still reported in full, which is why the summary can
	// afford to drop the last stop.
	require.Equal(t, "Lviv", item.DepartureName)
	require.Equal(t, "Chernivtsi", item.DestinationName)
}

// --- /similar ---------------------------------------------------------------

func TestSimilarTrips(t *testing.T) {
	w := newWorld(t)

	source, err := w.store.Trip(context.Background(), w.ids["Hoverla summit hike"])
	require.NoError(t, err)

	items, err := w.store.SimilarTrips(context.Background(), source)
	require.NoError(t, err)

	require.NotEmpty(t, items)
	require.LessOrEqual(t, len(items), domain.SimilarLimit)

	for _, item := range items {
		require.Equal(t, "hiking", item.Category, "same category")
		require.Equal(t, domain.StatusRecruiting, item.Status)
		require.NotEqual(t, source.ID, item.ID, "never the trip itself")
		require.True(t, item.StartAt.After(now))
		require.LessOrEqual(t, haversineKm(source.Departure, item.Departure), 100.0)
	}

	titles := w.titlesOf(items)
	require.Contains(t, titles, "Carpathian long traverse")
	require.NotContains(t, titles, "Lviv coffee weekend", "wrong category, and 115 km away")
	require.NotContains(t, titles, "Cancelled Hoverla hike")
}

func TestSimilarTripsExcludesFarAndForeignCategories(t *testing.T) {
	w := newWorld(t)

	source, err := w.store.Trip(context.Background(), w.ids["Kyiv street food crawl"])
	require.NoError(t, err)

	items, err := w.store.SimilarTrips(context.Background(), source)
	require.NoError(t, err)

	for _, item := range items {
		require.Equal(t, "food", item.Category)
		require.LessOrEqual(t, haversineKm(source.Departure, item.Departure), 100.0)
	}
	require.NotContains(t, w.titlesOf(items), "Lviv coffee weekend", "food, but 470 km away")
}

// --- the plan ---------------------------------------------------------------

// TestRadiusPredicateUsesTheGistIndex is the point of writing the filter as
// ST_DWithin.
//
// The two forms select identical rows, so no assertion about results can tell
// them apart; the difference is only visible in the plan, and it is the
// difference between an Index Cond and a Filter:
//
//	ST_DWithin   ->  Bitmap Index Scan on trips_departure_gist
//	                 Index Cond: (departure_location && _st_expand(..., 50000))
//
//	ST_Distance  ->  Filter: (st_distance(departure_location, ...) < 50000)
//	                 ...and no mention of trips_departure_gist at all
//
// An Index Cond narrows the rows the scan produces. A Filter is evaluated on
// every row the scan already produced — one PostGIS distance computation per
// trip in the table. Both plans below are taken with sequential scans disabled,
// so this is not the planner preferring a scan on small data: given every
// chance to use the GIST index, the ST_Distance form still cannot.
func TestRadiusPredicateUsesTheGistIndex(t *testing.T) {
	w := newWorld(t)

	plan, err := w.store.ExplainSearch(context.Background(), domain.SearchFilter{
		Near: near(testsupport.IvanoFrankivsk, 50),
	}.Normalize())
	require.NoError(t, err)
	t.Logf("ST_DWithin plan:\n%s", plan)

	require.Contains(t, plan, "trips_departure_gist",
		"the radius filter must be answered by the GIST index")
	require.Contains(t, plan, "Index Cond: (departure_location &&",
		"the radius has to become a bounding-box index qual, not a per-row filter")
	require.NotContains(t, plan, "Seq Scan on trips")

	distancePlan := mustExplainDistance(t, w.pool)
	require.NotContains(t, distancePlan, "trips_departure_gist",
		"ST_Distance in a WHERE clause cannot be served by the GIST index, whatever else the planner does")
	require.Contains(t, distancePlan, "Filter: (st_distance(",
		"it is evaluated once per row the scan produced")
}

// mustExplainDistance plans the same question written the forbidden way.
//
// It lives in the test rather than in the store on purpose: there is no
// production code path in this service that puts ST_Distance in a WHERE clause,
// and there should not be one to point at.
func mustExplainDistance(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	ctx := context.Background()
	_, err := pool.Exec(ctx, `ANALYZE trips`)
	require.NoError(t, err)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `SET LOCAL enable_seqscan = off`)
	require.NoError(t, err)

	rows, err := tx.Query(ctx, `
		EXPLAIN SELECT t.id
		FROM trips t
		WHERE t.status = 'recruiting'
		  AND ST_Distance(t.departure_location,
		                  ST_SetSRID(ST_MakePoint($1, $2), 4326)::geography) < $3`,
		testsupport.IvanoFrankivsk.Lng, testsupport.IvanoFrankivsk.Lat, 50_000.0)
	require.NoError(t, err)
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	require.NoError(t, rows.Err())

	t.Logf("ST_Distance plan:\n%s", plan.String())
	return plan.String()
}

// --- helpers ----------------------------------------------------------------

// pageOf is the page size as the filter takes it: a pointer, because
// `?limit=0` has to be distinguishable from "no limit given".
func pageOf(n int) *int { return &n }

func near(place testsupport.Place, radiusKm float64) domain.RadiusFilter {
	return domain.RadiusFilter{Lat: &place.Lat, Lng: &place.Lng, RadiusKm: &radiusKm}
}

func ids(items []domain.TripListItem) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		out = append(out, item.ID)
	}
	return out
}

func (w *world) seed(title string) testsupport.TripSeed {
	w.t.Helper()
	for _, seed := range w.seeds {
		if seed.Title == title {
			return seed
		}
	}
	w.t.Fatalf("no seeded trip named %q", title)
	return testsupport.TripSeed{}
}

func (w *world) window(seed testsupport.TripSeed) (time.Time, time.Time) {
	start := now.AddDate(0, 0, seed.StartDay).Truncate(24 * time.Hour).Add(8 * time.Hour)
	return start, start.AddDate(0, 0, seed.Days)
}

func (w *world) mustStart(title string) time.Time {
	start, _ := w.window(w.seed(title))
	return start
}

// find returns the list item for a seeded trip, by walking pages until it turns
// up — so a projection assertion cannot pass because the trip happened to be on
// the first page.
func (w *world) find(title string) domain.TripListItem {
	w.t.Helper()
	return w.findByID(w.ids[title])
}

func (w *world) findByID(id uuid.UUID) domain.TripListItem {
	w.t.Helper()

	filter := domain.SearchFilter{Limit: pageOf(domain.SearchLimitMax)}
	for page := 0; page < 100; page++ {
		result := w.search(filter)
		for _, item := range result.Items {
			if item.ID == id {
				return item
			}
		}
		if result.Next == nil {
			break
		}
		filter.Cursor = result.Next
	}
	w.t.Fatalf("trip %s is not in any page of results", id)
	return domain.TripListItem{}
}

// publish creates a trip and moves it to recruiting, returning its id.
func (w *world) publish(in domain.TripInput) uuid.UUID {
	w.t.Helper()

	organizer := uuid.New()
	detail, err := w.store.CreateTrip(context.Background(), organizer, in)
	require.NoError(w.t, err)

	_, err = w.store.ChangeStatus(context.Background(), detail.Trip.ID, organizer, domain.StatusRecruiting)
	require.NoError(w.t, err)

	w.title[detail.Trip.ID] = in.Title
	w.ids[in.Title] = detail.Trip.ID
	return detail.Trip.ID
}

// haversineKm is a rough great-circle distance, used only to assert that what
// PostGIS returned is in the neighbourhood the test asked for. It is not how
// the query filters — that is ST_DWithin on a spheroid — and it does not need
// to agree to the metre.
func haversineKm(a, b domain.Coordinates) float64 {
	const earthKm = 6371.0
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }

	dLat := rad(b.Lat - a.Lat)
	dLng := rad(b.Lng - a.Lng)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(a.Lat))*math.Cos(rad(b.Lat))*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthKm * math.Asin(math.Sqrt(h))
}
