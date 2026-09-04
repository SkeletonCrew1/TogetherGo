package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/togethergo/trip/internal/domain"
)

// The discovery read path.
//
// This is the highest-traffic query in the system, so its shape is the design
// and the Go around it is bookkeeping. Four things about it are deliberate:
//
//  1. **Radius filters are ST_DWithin, never ST_Distance.** ST_DWithin is an
//     indexable operator: the planner rewrites it into a bounding-box search
//     that trips_departure_gist answers, then rechecks the survivors exactly.
//     `ST_Distance(a, b) < r` is a function call on every row of the table and
//     forces a sequential scan no matter what indexes exist. The two return
//     the same rows; only one of them can be served by an index.
//
//  2. **Free text is a tsvector match, never ILIKE.** See migration 00002.
//
//  3. **Pagination is keyset, never OFFSET.** The cursor compares against
//     (start_at, id) — the same pair, in the same order, as the trailing
//     columns of trips_status_start_idx — so "page 40" costs exactly what page
//     one costs. See domain.Cursor.
//
//  4. **The route summary is joined onto the page, not onto the candidate
//     set.** Selecting and ordering happen in a subquery with the LIMIT
//     already applied; the LATERAL that reads trip_points runs against its
//     output. A page of twenty trips reads twenty routes, whatever the filters
//     matched.

// discoveryScope is what every query in this file starts from and what none of
// them can opt out of: recruiting trips that have not left yet.
//
// It is written once, here, rather than in each query's WHERE clause, because
// it is the rule that keeps drafts — a trip's private workspace — out of a
// public listing. A filter is something a caller chooses; this is not.
const discoveryScope = `t.status = 'recruiting' AND t.start_at > `

// routeColumns is what a list card needs from a trip's route, on top of
// tripColumns.
//
// The lateral aggregates over the route once and slices what it needs out of
// the result, so a trip's trip_points rows are read a single time per page even
// though three of these expressions are derived from them. The coalesces cover
// the impossible case of a trip with no route: the domain requires two points
// and insertPoints is the only writer, but a LEFT JOIN that can produce NULL
// must not be scanned into a non-pointer string.
const routeColumns = `
	coalesce(route.names, '{}'::text[]),
	coalesce(route.total, 0),
	coalesce(route.first_name, ''),
	coalesce(route.last_name, '')`

const routeLateral = `
	LEFT JOIN LATERAL (
		SELECT (array_agg(p.name ORDER BY p.seq))[1:%d]     AS names,
		       count(*)::int                                AS total,
		       (array_agg(p.name ORDER BY p.seq))[1]        AS first_name,
		       (array_agg(p.name ORDER BY p.seq DESC))[1]   AS last_name
		FROM trip_points p
		WHERE p.trip_id = t.id
	) route ON TRUE`

// pageColumns is the inner query's projection, and pageOutput reads it back.
//
// Both list tripColumns' fields in tripColumns' order, so one tripScanTargets
// still describes how a trip row is scanned. They are spelled out here rather
// than reusing that constant because the CTE has to alias the two geography
// accessors — `ST_Y(...)` produces a column named `st_y`, and a second one
// named `st_y_1` — and because `SELECT t.*` would be worse: it would carry
// search_vector, a tsvector over a description of up to four thousand
// characters, through the CTE for every row on the busiest read path in the
// system, only for the outer SELECT to discard it.
const pageColumns = `
	t.id, t.organizer_id, t.title, t.description, t.category, t.status,
	t.capacity, t.approved_count, t.start_at, t.end_at,
	ST_Y(t.departure_location::geometry)   AS departure_lat,
	ST_X(t.departure_location::geometry)   AS departure_lng,
	ST_Y(t.destination_location::geometry) AS destination_lat,
	ST_X(t.destination_location::geometry) AS destination_lng,
	t.created_at, t.updated_at`

const pageOutput = `
	t.id, t.organizer_id, t.title, t.description, t.category, t.status,
	t.capacity, t.approved_count, t.start_at, t.end_at,
	t.departure_lat, t.departure_lng,
	t.destination_lat, t.destination_lng,
	t.created_at, t.updated_at`

// listQuery assembles a discovery query from accumulated predicates.
//
// Both discovery endpoints have this shape and differ only in what they filter
// on and how many rows they take, so the shape is written once. The two stages
// are the point: the inner query selects, orders and limits, and the outer one
// joins the route onto what survived. A page of twenty trips reads twenty
// routes, whatever the filters matched.
func listQuery(b *predicates, limit int) string {
	return fmt.Sprintf(`
		WITH page AS (
			SELECT %s
			FROM trips t
			WHERE %s
			ORDER BY t.start_at, t.id
			LIMIT %s
		)
		SELECT %s,%s
		FROM page t%s
		ORDER BY t.start_at, t.id`,
		pageColumns, b.where(), b.param(limit),
		pageOutput, routeColumns,
		fmt.Sprintf(routeLateral, domain.RouteSummaryMax),
	)
}

// SearchTrips answers GET /api/trips.
//
// The filter is validated by the caller; what arrives here is already typed and
// in range. Returns at most filter.Limit items and the cursor that continues
// the page, or a nil cursor when there is nothing after it.
func (s *Store) SearchTrips(ctx context.Context, filter domain.SearchFilter) (*domain.SearchPage, error) {
	b := &predicates{}

	// The scope first, so it is the first thing in the WHERE clause and the
	// first thing anyone reading an EXPLAIN sees.
	b.add(discoveryScope + b.param(s.now()))

	if filter.DateFrom != nil {
		// Overlap, not containment: a trip that started before the window and
		// is still running inside it is a trip the caller can join.
		b.add("t.end_at >= " + b.param(*filter.DateFrom))
	}
	if filter.DateTo != nil {
		b.add("t.start_at <= " + b.param(*filter.DateTo))
	}

	if filter.Near.Complete() {
		b.add(b.dwithin("t.departure_location", filter.Near))
	}
	if filter.Destination.Complete() {
		b.add(b.dwithin("t.destination_location", filter.Destination))
	}

	if filter.MinDays != nil {
		b.add(durationDays + " >= " + b.param(*filter.MinDays))
	}
	if filter.MaxDays != nil {
		b.add(durationDays + " <= " + b.param(*filter.MaxDays))
	}

	if filter.MinFreeSlots != nil {
		b.add("(t.capacity - t.approved_count) >= " + b.param(*filter.MinFreeSlots))
	}

	if len(filter.Categories) > 0 {
		b.add("t.category = ANY(" + b.param(filter.Categories) + "::text[])")
	}

	if filter.Query != "" {
		// websearch_to_tsquery rather than plainto_: it understands quoted
		// phrases, `or`, and a leading `-` for exclusion, and — unlike
		// to_tsquery — it cannot be made to raise a syntax error by a user
		// typing an unbalanced parenthesis into a search box.
		b.add("t.search_vector @@ websearch_to_tsquery('english', " + b.param(filter.Query) + ")")
	}

	if filter.Cursor != nil {
		// A row comparison, not `start_at > $a OR (start_at = $a AND id > $b)`.
		// They select the same rows, but only the row form is a single index
		// qual on (start_at, id); the OR form makes the planner choose between
		// two ranges and usually decline the index.
		b.add("(t.start_at, t.id) > (" + b.param(filter.Cursor.StartAt) + "::timestamptz, " +
			b.param(filter.Cursor.ID) + "::uuid)")
	}

	// One row more than the page, which is how the next cursor is known to
	// exist without a second COUNT query over the whole result set.
	pageSize := filter.PageSize()
	probe := pageSize + 1

	items, err := s.queryListItems(ctx, nil, listQuery(b, probe), b.args...)
	if err != nil {
		return nil, err
	}
	return paginate(items, pageSize), nil
}

// paginate splits the probe row off the end of a result set.
//
// Every keyset listing in the service reads one row more than it returns; this
// is the one place that turns that extra row into a cursor. Next is nil when
// the probe found nothing, which is how the last page is known without a
// count(*) over the whole result set.
func paginate(items []domain.TripListItem, pageSize int) *domain.SearchPage {
	page := &domain.SearchPage{Items: items}
	if len(items) > pageSize {
		page.Items = items[:pageSize]
		last := page.Items[len(page.Items)-1]
		page.Next = &domain.Cursor{StartAt: last.StartAt, ID: last.ID}
	}
	return page
}

// SimilarTrips answers GET /api/trips/{id}/similar: recruiting trips in the
// same category whose departure is near this one's, excluding it.
//
// Ordered by departure time rather than by distance. Every candidate is already
// within SimilarRadiusKm, so distance has stopped being the interesting axis;
// "which of these can I still join, soonest" is the question the panel exists
// to answer. Ordering by distance would also mean an ST_Distance per candidate
// row, which is the cost the WHERE clause is careful to avoid.
func (s *Store) SimilarTrips(ctx context.Context, trip domain.Trip) ([]domain.TripListItem, error) {
	b := &predicates{}

	b.add(discoveryScope + b.param(s.now()))
	b.add("t.category = " + b.param(trip.Category))
	b.add("t.id <> " + b.param(trip.ID))
	b.add(b.dwithinAt("t.departure_location", trip.Departure, domain.SimilarRadiusKm*1000))

	return s.queryListItems(ctx, nil, listQuery(b, domain.SimilarLimit), b.args...)
}

// ExplainSearch returns the planner's chosen plan for a filter's WHERE clause.
//
// It exists for the test that proves the radius predicate is answered by
// trips_departure_gist rather than by a sequential scan — the one property of
// this file that cannot be asserted by looking at the rows that come back,
// because ST_DWithin and ST_Distance return exactly the same ones.
//
// The ORDER BY and LIMIT of the real query are left off deliberately. With them
// the planner has a second reason to prefer trips_status_start_idx — it
// produces rows already sorted — and the plan would then answer "which index
// serves the ordering", not "is the radius predicate indexable", which is the
// question being asked.
func (s *Store) ExplainSearch(ctx context.Context, filter domain.SearchFilter) (string, error) {
	// The plan is only meaningful once the planner has statistics; a table that
	// has never been analysed is assumed to hold a few pages and every plan
	// over it is a sequential scan.
	if _, err := s.pool.Exec(ctx, `ANALYZE trips`); err != nil {
		return "", fmt.Errorf("analyze trips: %w", err)
	}
	// In a transaction, so SET LOCAL is scoped to it. A plain SET on a pooled
	// connection would be handed to the next request that borrowed it.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin explain transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Seeded test data is a few dozen rows, where a sequential scan genuinely
	// is cheaper than any index and the planner is right to say so. Turning it
	// off asks the question the caller actually cares about: *can* this
	// predicate be served by the GIST index. ST_DWithin can, and answers with
	// an index scan; ST_Distance cannot, and would still answer with a
	// sequential scan even with seqscan disabled, because there is no index
	// qual for it to use.
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		return "", fmt.Errorf("disable seqscan: %w", err)
	}

	b := &predicates{}
	b.add(discoveryScope + b.param(s.now()))
	if filter.Near.Complete() {
		b.add(b.dwithin("t.departure_location", filter.Near))
	}
	if filter.Destination.Complete() {
		b.add(b.dwithin("t.destination_location", filter.Destination))
	}

	rows, err := tx.Query(ctx, fmt.Sprintf(
		`EXPLAIN SELECT t.id FROM trips t WHERE %s`, b.where()), b.args...)
	if err != nil {
		return "", fmt.Errorf("explain search: %w", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", fmt.Errorf("scan plan line: %w", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate plan: %w", err)
	}
	return plan.String(), nil
}

// queryListItems reads a list-card query into domain types.
//
// `extra` names the scan destinations for whatever the query selected after the
// route columns, and is nil for the discovery queries, which select nothing
// else. It is a function rather than a slice because the destinations are
// pointers into the row being scanned and there is a new one per iteration.
func (s *Store) queryListItems(
	ctx context.Context,
	extra func(*domain.TripListItem) []any,
	query string,
	args ...any,
) ([]domain.TripListItem, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search trips: %w", err)
	}
	defer rows.Close()

	items := make([]domain.TripListItem, 0, domain.SearchLimitDefault)
	for rows.Next() {
		var item domain.TripListItem
		targets := append(tripScanTargets(&item.Trip),
			&item.Route.Points, &item.Route.TotalPoints,
			&item.DepartureName, &item.DestinationName)
		if extra != nil {
			targets = append(targets, extra(&item)...)
		}

		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scan trip list item: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trip list items: %w", err)
	}
	return items, nil
}

// durationDays is how long a trip lasts, as a human counts it.
//
// Rounded up, and never less than one: a trip leaving Friday evening and
// returning Sunday afternoon is 2.1 elapsed days and is advertised as a
// three-day weekend, so min_days=3 has to find it. A trip that starts and ends
// on the same day is one day, not zero.
//
// The expression is not indexed and does not need to be. It is a filter applied
// to rows the access path has already narrowed, not something the planner would
// ever drive a scan from.
const durationDays = `GREATEST(1, ceil(EXTRACT(EPOCH FROM (t.end_at - t.start_at)) / 86400.0))`

// predicates accumulates WHERE clauses and their bind parameters together, so a
// condition and the values it refers to are written in one place and the
// placeholder numbers cannot drift.
//
// Every value goes through param. There is no path in this file that formats a
// caller-supplied value into SQL text; the only things interpolated are
// compile-time constants.
type predicates struct {
	clauses []string
	args    []any
}

// param registers a bind value and returns its placeholder.
func (p *predicates) param(value any) string {
	p.args = append(p.args, value)
	return "$" + strconv.Itoa(len(p.args))
}

func (p *predicates) add(clause string) {
	p.clauses = append(p.clauses, clause)
}

func (p *predicates) where() string {
	return strings.Join(p.clauses, "\n\t\t\t  AND ")
}

// dwithin builds the indexable radius predicate for a filter group.
func (p *predicates) dwithin(column string, filter domain.RadiusFilter) string {
	return p.dwithinAt(column, domain.Coordinates{Lat: *filter.Lat, Lng: *filter.Lng}, filter.Meters())
}

// dwithinAt is the same predicate around a known point.
//
// ST_MakePoint takes longitude first. That reversal is confined to this package
// (see the comment on domain.Coordinates), and this is one of the two places in
// it that has to remember.
func (p *predicates) dwithinAt(column string, at domain.Coordinates, meters float64) string {
	return fmt.Sprintf("ST_DWithin(%s, ST_SetSRID(ST_MakePoint(%s, %s), 4326)::geography, %s)",
		column, p.param(at.Lng), p.param(at.Lat), p.param(meters))
}
