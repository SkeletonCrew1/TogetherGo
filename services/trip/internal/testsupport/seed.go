package testsupport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/store"
)

// Seed data for the discovery tests.
//
// Real places with real coordinates, because the thing under test is a
// geographic predicate and made-up points would make a passing radius test
// prove nothing about the one the acceptance criteria describe. Distances from
// Ivano-Frankivsk, which every radius assertion is written around:
//
//	Ivano-Frankivsk   0 km      Yaremche      ~54 km     Lviv          ~115 km
//	Halych           ~22 km     Bukovel       ~66 km     Chernivtsi    ~114 km
//	Kalush           ~27 km     Hoverla       ~86 km     Kyiv          ~470 km
//
// The 50 km radius the acceptance criteria use therefore contains exactly
// Ivano-Frankivsk, Halych and Kalush, and the 100 km radius /similar uses adds
// Yaremche, Bukovel and Hoverla. Kolomyia is deliberately absent: at 49.9 km it
// would make every radius assertion depend on which spheroid PostGIS picked.

// Place is a named coordinate.
type Place struct {
	Name string
	Lat  float64
	Lng  float64
}

var (
	IvanoFrankivsk = Place{"Ivano-Frankivsk", 48.9226, 24.7111}
	Halych         = Place{"Halych", 49.1236, 24.7275}
	Kalush         = Place{"Kalush", 49.0186, 24.3670}
	Yaremche       = Place{"Yaremche", 48.4517, 24.5556}
	Bukovel        = Place{"Bukovel", 48.3600, 24.4100}
	Hoverla        = Place{"Hoverla", 48.1600, 24.5000}
	Lviv           = Place{"Lviv", 49.8397, 24.0297}
	Chernivtsi     = Place{"Chernivtsi", 48.2921, 25.9358}
	Kyiv           = Place{"Kyiv", 50.4501, 30.5234}
	KyivPodil      = Place{"Kyiv, Podil", 50.4661, 30.5147}
	Odesa          = Place{"Odesa", 46.4825, 30.7233}
	Zatoka         = Place{"Zatoka", 46.0714, 30.4694}
	Kharkiv        = Place{"Kharkiv", 49.9935, 36.2304}
	Krakow         = Place{"Kraków", 50.0647, 19.9450}
	Budapest       = Place{"Budapest", 47.4979, 19.0402}
)

// TripSeed describes one trip to create. Dates are given as whole days from the
// test's clock so that a trip's duration in days is exact and min_days/max_days
// assertions do not depend on the time of day.
type TripSeed struct {
	Title       string
	Description string
	Category    string
	Capacity    int
	Approved    int
	StartDay    int
	Days        int
	Route       []Place
	Status      domain.Status
	Organizer   uuid.UUID

	// StartsInThePast seeds a trip whose departure time has already gone by.
	// It cannot be created that way — the domain refuses a start_at in the
	// past — so the row is written forward and moved back afterwards. It is
	// the only fixture that edits a trip behind the store's back, and it
	// exists to prove discovery excludes trips that have already left.
	StartsInThePast bool
}

// SeedTrips creates every seed and returns their ids keyed by title.
//
// Creation goes through the real store rather than through hand-written INSERTs
// so that the denormalised departure/destination columns, the organizer's
// participant row and the status transitions are all produced by the code the
// service actually runs. Only two things are patched afterwards, both of them
// states this stage has no endpoint for yet: an approved_count above one (the
// join-request stage writes those) and a departure in the past.
func SeedTrips(t *testing.T, pool *pgxpool.Pool, now time.Time, seeds []TripSeed) map[string]uuid.UUID {
	t.Helper()

	ctx := context.Background()
	s := store.New(pool).WithClock(func() time.Time { return now })
	ids := make(map[string]uuid.UUID, len(seeds))

	for _, seed := range seeds {
		require.NotContains(t, ids, seed.Title, "seed titles are the test's handle on a trip and must be unique")

		organizer := seed.Organizer
		if organizer == uuid.Nil {
			organizer = uuid.New()
		}

		points := make([]domain.PointInput, len(seed.Route))
		for i, place := range seed.Route {
			points[i] = domain.PointInput{Name: place.Name, Lat: place.Lat, Lng: place.Lng}
		}

		startAt := now.AddDate(0, 0, seed.StartDay).Truncate(24 * time.Hour).Add(8 * time.Hour)
		if !startAt.After(now) {
			startAt = now.AddDate(0, 0, seed.StartDay+1).Truncate(24 * time.Hour).Add(8 * time.Hour)
		}
		endAt := startAt.AddDate(0, 0, seed.Days)

		description := seed.Description
		detail, err := s.CreateTrip(ctx, organizer, domain.TripInput{
			Title:       seed.Title,
			Description: &description,
			Category:    seed.Category,
			Capacity:    seed.Capacity,
			StartAt:     startAt,
			EndAt:       endAt,
			Points:      points,
		})
		require.NoErrorf(t, err, "seeding %q", seed.Title)
		ids[seed.Title] = detail.Trip.ID

		status := seed.Status
		if status == "" {
			status = domain.StatusRecruiting
		}
		for _, to := range pathTo(status) {
			_, err := s.ChangeStatus(ctx, detail.Trip.ID, organizer, to)
			require.NoErrorf(t, err, "seeding %q: -> %s", seed.Title, to)
		}

		if seed.Approved > 1 {
			_, err := pool.Exec(ctx, `UPDATE trips SET approved_count = $2 WHERE id = $1`,
				detail.Trip.ID, seed.Approved)
			require.NoError(t, err)
		}

		if seed.StartsInThePast {
			_, err := pool.Exec(ctx, `
				UPDATE trips
				SET start_at = $2, end_at = $3
				WHERE id = $1`,
				detail.Trip.ID,
				now.AddDate(0, 0, -seed.StartDay-1),
				now.AddDate(0, 0, -seed.StartDay-1).AddDate(0, 0, seed.Days),
			)
			require.NoError(t, err)
		}
	}

	return ids
}

// Rewind moves a trip's start_at and end_at into the past.
//
// The domain refuses to create a trip that starts in the past and refuses to
// edit one that is already under way, so a trip whose clock has run out cannot
// be produced through the API at all — which is precisely the state the
// lifecycle scheduler exists to act on. This is the one fixture that writes
// those two columns behind the store's back, and it exists so a scheduler test
// can assert on a due trip without sleeping through one.
//
// Only the dates move. The status, the roster, the outbox and
// completed_event_emitted are whatever the real code paths left them.
func Rewind(t *testing.T, pool *pgxpool.Pool, tripID uuid.UUID, startAt, endAt time.Time) {
	t.Helper()

	tag, err := pool.Exec(context.Background(),
		`UPDATE trips SET start_at = $2, end_at = $3 WHERE id = $1`, tripID, startAt, endAt)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected(), "rewinding a trip that is not there")
}

// pathTo is the sequence of transitions from draft to the wanted status. The
// state machine has no shortcuts, so a completed trip really is published,
// started and finished in order.
func pathTo(status domain.Status) []domain.Status {
	switch status {
	case domain.StatusDraft:
		return nil
	case domain.StatusRecruiting:
		return []domain.Status{domain.StatusRecruiting}
	case domain.StatusCancelled:
		return []domain.Status{domain.StatusCancelled}
	case domain.StatusInProgress:
		return []domain.Status{domain.StatusRecruiting, domain.StatusInProgress}
	case domain.StatusCompleted:
		return []domain.Status{domain.StatusRecruiting, domain.StatusInProgress, domain.StatusCompleted}
	default:
		panic(fmt.Sprintf("unknown seed status %q", status))
	}
}

// LvivRynok and the other in-city second stops exist because a route needs two
// distinct points; a trip that begins and ends in the same city still has to
// say where in it.
var LvivRynok = Place{"Lviv, Rynok Square", 49.8419, 24.0315}

// DiscoveryWorld is the fifty-trip fixture the discovery tests search.
//
// The first twelve are named and their properties are chosen so that each
// filter has something to find and something to miss; the rest are generated
// filler that makes pagination and the "one batch call per page" behaviour
// mean something. Two rules hold across the whole set, and the assertions
// depend on them:
//
//   - No generated trip departs from Ivano-Frankivsk, Halych or Kalush, so the
//     set of trips within 50 km of Ivano-Frankivsk is exactly three named ones.
//   - No generated title or description contains "Hoverla", "Chornohora" or
//     "banosh", so the free-text assertions have exact answers.
//
// organizers is cycled through, so a page has several distinct organizer ids
// without having fifty of them.
func DiscoveryWorld(organizers []uuid.UUID) []TripSeed {
	pick := func(i int) uuid.UUID {
		if len(organizers) == 0 {
			return uuid.Nil
		}
		return organizers[i%len(organizers)]
	}

	seeds := []TripSeed{
		{
			Title:       "Hoverla summit hike",
			Description: "Three days on the Chornohora ridge, tents and a sunrise above the clouds.",
			Category:    "hiking", Capacity: 8, Approved: 3,
			StartDay: 14, Days: 3,
			Route:     []Place{IvanoFrankivsk, Yaremche, Hoverla},
			Organizer: pick(0),
		},
		{
			Title:       "Kyiv street food crawl",
			Description: "Varenyky, banosh and a great deal of walking between them.",
			Category:    "food", Capacity: 6, Approved: 6,
			StartDay: 10, Days: 1,
			Route:     []Place{Kyiv, KyivPodil},
			Organizer: pick(1),
		},
		{
			Title:       "Odesa seaside weekend",
			Description: "Two slow days on the coast.",
			Category:    "city", Capacity: 10, Approved: 4,
			StartDay: 21, Days: 2,
			Route:     []Place{Odesa, Zatoka},
			Organizer: pick(2),
		},
		{
			Title:       "Carpathian long traverse",
			Description: "Ten days across the range at an unhurried pace.",
			Category:    "hiking", Capacity: 12, Approved: 2,
			StartDay: 30, Days: 10,
			Route:     []Place{Kalush, Yaremche, Bukovel},
			Organizer: pick(0),
		},
		{
			Title:       "Lviv coffee weekend",
			Description: "Roasteries, a brass band and one very long queue.",
			Category:    "food", Capacity: 8, Approved: 2,
			StartDay: 18, Days: 2,
			Route:     []Place{Lviv, LvivRynok},
			Organizer: pick(1),
		},
		{
			Title:       "Bukovel ski week",
			Description: "A week of snow, for people who can already turn.",
			Category:    "nature", Capacity: 20, Approved: 5,
			StartDay: 40, Days: 7,
			Route:     []Place{Bukovel, Yaremche},
			Organizer: pick(2),
		},
		{
			Title:       "Halych castle day trip",
			Description: "One day, one castle, one bus.",
			Category:    "city", Capacity: 5, Approved: 1,
			StartDay: 12, Days: 1,
			Route:     []Place{Halych, IvanoFrankivsk},
			Organizer: pick(0),
		},
		{
			Title:       "Krakow and Budapest by train",
			Description: "Eight days, two capitals, no flights.",
			Category:    "abroad", Capacity: 6, Approved: 1,
			StartDay: 60, Days: 8,
			Route:     []Place{Lviv, Krakow, Budapest},
			Organizer: pick(1),
		},

		// The four below must never appear in a discovery result. Each breaks
		// one of the two rules the store applies to every query, and all four
		// depart from inside the 50 km radius and mention Hoverla, so a query
		// that forgot a rule returns them and fails loudly.
		{
			Title:       "Hoverla winter draft",
			Description: "Not published yet.",
			Category:    "hiking", Capacity: 6, StartDay: 15, Days: 3,
			Route:  []Place{IvanoFrankivsk, Hoverla},
			Status: domain.StatusDraft, Organizer: pick(0),
		},
		{
			Title:       "Cancelled Hoverla hike",
			Description: "Called off.",
			Category:    "hiking", Capacity: 6, StartDay: 16, Days: 3,
			Route:  []Place{IvanoFrankivsk, Hoverla},
			Status: domain.StatusCancelled, Organizer: pick(0),
		},
		{
			Title:       "Completed Hoverla hike",
			Description: "Already happened.",
			Category:    "hiking", Capacity: 6, StartDay: 17, Days: 3,
			Route:  []Place{IvanoFrankivsk, Hoverla},
			Status: domain.StatusCompleted, Organizer: pick(0),
		},
		{
			Title:       "Departed Hoverla hike",
			Description: "Still recruiting on paper, but it left last week.",
			Category:    "hiking", Capacity: 6, StartDay: 7, Days: 3,
			Route:           []Place{IvanoFrankivsk, Hoverla},
			StartsInThePast: true, Organizer: pick(0),
		},
	}

	// Filler. Twelve route templates, none of them departing from inside the
	// 50 km radius, cycled until the world is fifty trips.
	templates := []struct {
		category string
		from, to Place
	}{
		{"hiking", Yaremche, Hoverla},
		{"hiking", Bukovel, Hoverla},
		{"city", Kyiv, Lviv},
		{"city", Kharkiv, Odesa},
		{"food", Odesa, Zatoka},
		{"food", Chernivtsi, Kyiv},
		{"nature", Yaremche, Bukovel},
		{"nature", Chernivtsi, Hoverla},
		{"abroad", Lviv, Krakow},
		{"abroad", Chernivtsi, Budapest},
		{"other", Kyiv, KyivPodil},
		{"other", Lviv, LvivRynok},
	}

	for i := 0; len(seeds) < 50; i++ {
		tpl := templates[i%len(templates)]
		capacity := 4 + i%10
		approved := 1 + i%4
		if approved > capacity {
			approved = capacity
		}
		seeds = append(seeds, TripSeed{
			Title:       fmt.Sprintf("Group outing %02d from %s", i+1, tpl.from.Name),
			Description: "A regular outing, seeded so that a page of results has something on it.",
			Category:    tpl.category,
			Capacity:    capacity,
			Approved:    approved,
			StartDay:    5 + i*2,
			Days:        1 + i%9,
			Route:       []Place{tpl.from, tpl.to},
			Organizer:   pick(i),
		})
	}

	return seeds
}
