package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/togethergo/trip/internal/domain"
)

// The dashboard read path: GET /api/my/trips, both tabs.
//
// It is the same two-stage shape as discovery — select and order and limit in a
// subquery, join the route onto what survived — for the same reason, and it
// reuses discovery's projection constants so there is one definition of what a
// list card selects. What differs is deliberate and is confined to this file:
//
//  1. **The ordering is reversed.** (start_at DESC, id DESC), because this is a
//     history and not a feed. The keyset comparison flips with it — `<` where
//     discovery uses `>` — and the cursor format does not, which is what lets
//     one parser serve both (see domain.Cursor).
//
//  2. **There is no discoveryScope.** This is the only place drafts and
//     cancelled trips are ever listed, and it is safe here precisely because
//     the scope is replaced by a stricter one: every row is a trip the caller
//     organizes, is on, or has applied to. `WHERE organizer_id = $me` is not a
//     filter the caller chose, it is the whole query.
//
//  3. **The participant tab is a union.** "The trips I am on" and "the trips I
//     have asked to be on" are two different tables, and the tab shows both.

// dashboardOrder is the ordering both tabs use, in the inner query and the
// outer one. Written once because the two must agree: the CTE's LIMIT decides
// *which* rows are on the page, and only the outer ORDER BY decides what order
// they come back in — a mismatch would page correctly and render shuffled.
const dashboardOrder = `t.start_at DESC, t.id DESC`

// pendingRequestsLateral counts the applications waiting on each trip.
//
// It runs in the outer query, against the page, for the same reason the route
// lateral does: inside the CTE it would count requests for every candidate row
// the WHERE clause matched — every trip an organizer has ever created — and
// then throw all but twenty of the counts away. Out here a page of twenty trips
// costs twenty index lookups on join_requests_trip_idx, whose leading columns
// are exactly (trip_id, status).
const pendingRequestsLateral = `
	LEFT JOIN LATERAL (
		SELECT count(*)::int AS pending
		FROM join_requests jr
		WHERE jr.trip_id = t.id AND jr.status = 'pending'
	) requests ON TRUE`

// membershipSource is the "I participate" tab's candidate set: one row per
// trip, with a rank saying which branch it came from.
//
// The GROUP BY is not decoration. A user who was approved, left, and applied
// again has a participants row *and* a pending request, and a plain UNION ALL
// would put that trip on the page twice — once as `approved` and once as
// `requested` — which is both wrong on its face and fatal to the keyset cursor,
// since the two copies share a (start_at, id) and no comparison can separate
// them. Collapsing to min(rank) makes the set one row per trip by construction,
// and picks the stronger standing when both are true.
//
// %[1]s is the caller's id, bound once and used in both branches.
const membershipSource = `
			SELECT trip_id, min(rank) AS rank
			FROM (
				SELECT p.trip_id, 0 AS rank
				FROM participants p
				WHERE p.user_id = %[1]s AND p.role = '` + domain.RoleParticipant + `'
				UNION ALL
				SELECT jr.trip_id, 1
				FROM join_requests jr
				WHERE jr.user_id = %[1]s AND jr.status = '` + string(domain.JoinPending) + `'
			) sources
			GROUP BY trip_id`

// MyTrips answers GET /api/my/trips.
//
// The filter is validated by the caller; what arrives here is already typed and
// in range. `userID` comes from the token and is the only thing that decides
// which rows exist — it is never a parameter the client can supply.
func (s *Store) MyTrips(ctx context.Context, userID uuid.UUID, filter domain.MyTripsFilter) (*domain.SearchPage, error) {
	filter = filter.Normalize()

	// One row more than the page, which is how the next cursor is known to
	// exist without a second COUNT query over the whole result set.
	pageSize := filter.PageSize()
	probe := pageSize + 1

	var query string
	var args []any

	switch filter.Role {
	case domain.DashboardOrganizer:
		query, args = organizedQuery(userID, filter, probe)
	case domain.DashboardParticipant:
		query, args = participatingQuery(userID, filter, probe)
	default:
		// Unreachable through the API — the role is validated before it gets
		// here — and an error rather than a default branch, because silently
		// answering one tab's question with the other's is the worst available
		// failure.
		return nil, fmt.Errorf("unknown dashboard role %q", filter.Role)
	}

	items, err := s.queryListItems(ctx, dashboardTargets, query, args...)
	if err != nil {
		return nil, err
	}
	return paginate(items, pageSize), nil
}

// dashboardTargets scans the two columns the dashboard selects on top of a list
// card. See queryListItems.
func dashboardTargets(item *domain.TripListItem) []any {
	return []any{&item.Membership, &item.PendingRequests}
}

// organizedQuery builds the "I organized" tab.
//
// Every trip whose organizer_id is the caller, in every status — this is the
// only listing in the service that returns a draft or a cancelled trip, and it
// is allowed to because there is exactly one person it can return them to.
func organizedQuery(userID uuid.UUID, filter domain.MyTripsFilter, limit int) (string, []any) {
	b := &predicates{}
	b.add("t.organizer_id = " + b.param(userID))
	addDashboardFilters(b, filter)

	inner := fmt.Sprintf(`
			SELECT %s, '%s'::text AS membership_status
			FROM trips t
			WHERE %s
			ORDER BY %s
			LIMIT %s`,
		pageColumns, domain.MembershipOrganizer, b.where(), dashboardOrder, b.param(limit))

	return dashboardQuery(inner, pendingRequestsLateral, "requests.pending"), b.args
}

// participatingQuery builds the "I participate" tab: trips the caller is on,
// plus trips they have only applied to.
//
// The union is a subquery and both the keyset predicate and the ORDER BY are on
// the outer query, which is what makes the cursor survive the boundary between
// the two branches. Applied per branch instead, each would resume from its own
// position and the merged page would skip whichever branch the cursor did not
// come from — a gap that only appears once a page happens to end on a row from
// the other set, which is to say in production and not in a two-row test.
func participatingQuery(userID uuid.UUID, filter domain.MyTripsFilter, limit int) (string, []any) {
	b := &predicates{}
	viewer := b.param(userID)

	// The caller's own trips belong to the other tab. Their organizer
	// participants row would otherwise put every trip they run into both.
	b.add("t.organizer_id <> " + viewer)
	// A draft is its organizer's private workspace and this tab only ever shows
	// other people's trips, so no draft may appear on it. Nothing can currently
	// produce one — a draft is unfindable, so nobody can apply to it — which is
	// exactly why the rule is written down rather than left as a consequence of
	// two other rules that could change.
	b.add("t.status <> '" + string(domain.StatusDraft) + "'")
	addDashboardFilters(b, filter)

	inner := fmt.Sprintf(`
			SELECT %s,
			       (CASE WHEN m.rank = 0 THEN '%s' ELSE '%s' END)::text AS membership_status
			FROM (%s
			) m
			JOIN trips t ON t.id = m.trip_id
			WHERE %s
			ORDER BY %s
			LIMIT %s`,
		pageColumns,
		domain.MembershipApproved, domain.MembershipRequested,
		fmt.Sprintf(membershipSource, viewer),
		b.where(), dashboardOrder, b.param(limit))

	// No pending count on this tab: how many other people are queueing for a
	// trip is the organizer's business.
	return dashboardQuery(inner, "", "NULL::int"), b.args
}

// dashboardQuery wraps a tab's candidate query in the second stage both share:
// the route lateral, the membership column and the page ordering.
func dashboardQuery(inner, extraJoin, pending string) string {
	return fmt.Sprintf(`
		WITH page AS (%s
		)
		SELECT %s,%s,
			t.membership_status, %s
		FROM page t%s%s
		ORDER BY %s`,
		inner,
		pageOutput, routeColumns, pending,
		fmt.Sprintf(routeLateral, domain.RouteSummaryMax), extraJoin,
		dashboardOrder)
}

// addDashboardFilters adds the two predicates both tabs share.
func addDashboardFilters(b *predicates, filter domain.MyTripsFilter) {
	if len(filter.Statuses) > 0 {
		b.add("t.status = ANY(" + b.param(statusStrings(filter.Statuses)) + "::text[])")
	}
	if c := filter.Cursor; c != nil {
		// `<`, not `>`: this listing runs newest-first. A row comparison rather
		// than an OR of two ranges, for the reason SearchTrips gives.
		b.add("(t.start_at, t.id) < (" + b.param(c.StartAt) + "::timestamptz, " +
			b.param(c.ID) + "::uuid)")
	}
}

// statusStrings unwraps the status vocabulary for the bind parameter. pgx
// encodes []string as a text[]; a []domain.Status is a different type as far as
// it is concerned.
func statusStrings(statuses []domain.Status) []string {
	out := make([]string, len(statuses))
	for i, s := range statuses {
		out[i] = string(s)
	}
	return out
}

// ViewerJoinRequest returns the status of a user's most recent join request on
// one trip, or nil when they have never applied.
//
// Most recent, not "the open one": a rejected request is still the answer to
// "what happened when I asked", and the trip detail page shows it so a user is
// not left wondering whether their application went anywhere. Which of those
// statuses reach the client is domain.NewViewer's decision, not this query's.
//
// No visibility check here, for the same reason Store.Trip has none: who is
// asking is an HTTP fact, and every caller of this method has already applied
// it to the trip itself.
func (s *Store) ViewerJoinRequest(ctx context.Context, tripID, userID uuid.UUID) (*domain.JoinRequestStatus, error) {
	var status domain.JoinRequestStatus
	err := s.pool.QueryRow(ctx, `
		SELECT jr.status
		FROM join_requests jr
		WHERE jr.trip_id = $1 AND jr.user_id = $2
		ORDER BY jr.created_at DESC, jr.id DESC
		LIMIT 1`, tripID, userID).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Never having asked is not an error, it is the common case.
			return nil, nil
		}
		return nil, fmt.Errorf("select viewer join request: %w", err)
	}
	return &status, nil
}
