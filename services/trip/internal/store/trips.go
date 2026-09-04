package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/events"
)

// tripColumns is the projection every trip read uses.
//
// The geography columns come back as lat/lng floats rather than as WKB: casting
// to geometry is what gives ST_Y/ST_X a planar accessor to read, and it costs
// nothing here because the values are points. Doing it this way means the Go
// side needs no PostGIS type codec at all.
const tripColumns = `
	t.id, t.organizer_id, t.title, t.description, t.category, t.status,
	t.capacity, t.approved_count, t.start_at, t.end_at,
	ST_Y(t.departure_location::geometry), ST_X(t.departure_location::geometry),
	ST_Y(t.destination_location::geometry), ST_X(t.destination_location::geometry),
	t.created_at, t.updated_at`

// tripScanTargets returns the scan destinations for tripColumns, in its order.
//
// Split out from scanTrip because the discovery queries select these columns
// followed by their own, and a second hand-written list of sixteen pointers is
// a second list to get out of step with the projection.
func tripScanTargets(t *domain.Trip) []any {
	return []any{
		&t.ID, &t.OrganizerID, &t.Title, &t.Description, &t.Category, &t.Status,
		&t.Capacity, &t.ApprovedCount, &t.StartAt, &t.EndAt,
		&t.Departure.Lat, &t.Departure.Lng,
		&t.Destination.Lat, &t.Destination.Lng,
		&t.CreatedAt, &t.UpdatedAt,
	}
}

func scanTrip(row pgx.Row) (domain.Trip, error) {
	var t domain.Trip
	err := row.Scan(tripScanTargets(&t)...)
	return t, err
}

// CreateTrip inserts a trip, its route and its organizer's participant row.
//
// All three in one transaction: a trip whose organizer is not on its roster, or
// whose route is missing, is not a half-created trip, it is a corrupt one.
func (s *Store) CreateTrip(ctx context.Context, organizerID uuid.UUID, in domain.TripInput) (*domain.TripDetail, error) {
	in = in.Normalize()
	if err := in.Validate(s.now(), true); err != nil {
		return nil, err
	}

	tripID := uuid.New()

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO trips (
				id, organizer_id, title, description, category, status,
				capacity, approved_count, start_at, end_at,
				departure_location, destination_location
			) VALUES (
				$1, $2, $3, $4, $5, $6,
				$7, 1, $8, $9,
				ST_SetSRID(ST_MakePoint($10, $11), 4326)::geography,
				ST_SetSRID(ST_MakePoint($12, $13), 4326)::geography
			)`,
			// The two geography columns are NOT NULL, so they have to be given
			// a value here; insertPoints re-derives them from the same route a
			// moment later and writes the identical pair. That second write is
			// redundant on this path and deliberately kept: making insertPoints
			// the only writer of those columns is what makes them impossible to
			// forget, and the cost is one UPDATE of a row this transaction
			// already holds locked.
			tripID, organizerID, in.Title, in.Description, in.Category, domain.StatusInitial,
			in.Capacity, in.StartAt, in.EndAt,
			in.Departure().Lng, in.Departure().Lat,
			in.Destination().Lng, in.Destination().Lat,
		)
		if err != nil {
			if constraint, ok := checkViolation(err); ok {
				return fmt.Errorf("insert trip violated %s: %w", constraint, err)
			}
			return fmt.Errorf("insert trip: %w", err)
		}

		if err := insertPoints(ctx, tx, tripID, in.Points); err != nil {
			return err
		}

		// approved_count is set to 1 in the insert above and this row is what
		// that 1 refers to. The two are written together, always.
		_, err = tx.Exec(ctx, `
			INSERT INTO participants (trip_id, user_id, role)
			VALUES ($1, $2, $3)`,
			tripID, organizerID, domain.RoleOrganizer,
		)
		if err != nil {
			return fmt.Errorf("insert organizer participant: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.TripDetail(ctx, tripID)
}

// Trip reads a single trip row without its route or its roster.
//
// The discovery paths need the trip itself and nothing else — /similar reads
// the source trip's category and departure to build its query — and loading a
// twenty-row route and a roster to use two columns is three round trips spent
// on data nothing looks at.
//
// Visibility is not applied here, for the same reason TripDetail does not apply
// it: who is asking is an HTTP fact. See Trip.IsVisibleTo.
func (s *Store) Trip(ctx context.Context, tripID uuid.UUID) (domain.Trip, error) {
	trip, err := scanTrip(s.pool.QueryRow(ctx, `SELECT`+tripColumns+` FROM trips t WHERE t.id = $1`, tripID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Trip{}, domain.ErrTripNotFound
		}
		return domain.Trip{}, fmt.Errorf("select trip: %w", err)
	}
	return trip, nil
}

// TripDetail reads a trip with its ordered route and its roster.
//
// Visibility is not applied here — the caller decides, because "who is asking"
// is an HTTP concern and this method is also used by the write paths, which
// have already authorised. See Trip.IsVisibleTo.
func (s *Store) TripDetail(ctx context.Context, tripID uuid.UUID) (*domain.TripDetail, error) {
	return loadDetail(ctx, s.pool, tripID)
}

func loadDetail(ctx context.Context, q querier, tripID uuid.UUID) (*domain.TripDetail, error) {
	trip, err := scanTrip(q.QueryRow(ctx, `SELECT`+tripColumns+` FROM trips t WHERE t.id = $1`, tripID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrTripNotFound
		}
		return nil, fmt.Errorf("select trip: %w", err)
	}

	points, err := loadPoints(ctx, q, tripID)
	if err != nil {
		return nil, err
	}
	participants, err := loadParticipants(ctx, q, tripID)
	if err != nil {
		return nil, err
	}

	return &domain.TripDetail{Trip: trip, Points: points, Participants: participants}, nil
}

func loadPoints(ctx context.Context, q querier, tripID uuid.UUID) ([]domain.Point, error) {
	rows, err := q.Query(ctx, `
		SELECT id, seq, name,
		       ST_Y(location::geometry), ST_X(location::geometry),
		       arrive_at, transport
		FROM trip_points
		WHERE trip_id = $1
		ORDER BY seq`, tripID)
	if err != nil {
		return nil, fmt.Errorf("select trip points: %w", err)
	}
	defer rows.Close()

	points := make([]domain.Point, 0, domain.PointsMax)
	for rows.Next() {
		var p domain.Point
		if err := rows.Scan(&p.ID, &p.Seq, &p.Name, &p.Lat, &p.Lng, &p.ArriveAt, &p.Transport); err != nil {
			return nil, fmt.Errorf("scan trip point: %w", err)
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trip points: %w", err)
	}
	return points, nil
}

func loadParticipants(ctx context.Context, q querier, tripID uuid.UUID) ([]domain.Participant, error) {
	// Ordered by join time, with user_id as the tiebreaker so the list is
	// stable: the organizer's row and an approval landing in the same
	// transaction share a joined_at.
	rows, err := q.Query(ctx, `
		SELECT user_id, role, joined_at
		FROM participants
		WHERE trip_id = $1
		ORDER BY joined_at, user_id`, tripID)
	if err != nil {
		return nil, fmt.Errorf("select participants: %w", err)
	}
	defer rows.Close()

	participants := make([]domain.Participant, 0, 8)
	for rows.Next() {
		var p domain.Participant
		if err := rows.Scan(&p.UserID, &p.Role, &p.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan participant: %w", err)
		}
		participants = append(participants, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate participants: %w", err)
	}
	return participants, nil
}

// lockTrip reads a trip inside a transaction and holds a row lock on it for the
// rest of that transaction. Every mutating path starts here.
func lockTrip(ctx context.Context, tx pgx.Tx, tripID uuid.UUID) (domain.Trip, error) {
	trip, err := scanTrip(tx.QueryRow(ctx, `SELECT`+tripColumns+` FROM trips t WHERE t.id = $1 FOR UPDATE`, tripID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Trip{}, domain.ErrTripNotFound
		}
		return domain.Trip{}, fmt.Errorf("select trip for update: %w", err)
	}
	return trip, nil
}

// requireOrganizer authorises a mutation.
//
// The order of the two checks is the whole point. An actor who cannot see the
// trip at all gets ErrTripNotFound — a draft answering 403 to a stranger would
// confirm that the id names a real trip, which is exactly what withholding
// drafts is meant to prevent. Only once the trip is known to be visible does a
// wrong caller get the honest 403.
func requireOrganizer(trip domain.Trip, actorID uuid.UUID) error {
	if !trip.IsVisibleTo(actorID) {
		return domain.ErrTripNotFound
	}
	if trip.OrganizerID != actorID {
		return domain.ErrNotOrganizer
	}
	return nil
}

// ChangeStatus is the organizer-driven entry point to the lifecycle: publish
// and cancel come through here, holding a row lock and passing the organizer
// check before anything is written.
//
// It is a thin authorising wrapper around changeStatus, which is the actual
// single writer of trips.status. The scheduler cannot use this door — it is
// nobody's organizer, and it holds its batch's row locks in a transaction of
// its own that this method's inTx would deadlock against — so it goes through
// the same core from inside its own transaction instead. There is exactly one
// UPDATE of that column in the service and both callers issue it.
//
// Creation is not a transition and does not pass through here: CreateTrip
// inserts domain.StatusInitial, which has no "from" to check.
func (s *Store) ChangeStatus(ctx context.Context, tripID, actorID uuid.UUID, to domain.Status) (*domain.TripDetail, error) {
	var detail *domain.TripDetail

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if err := requireOrganizer(trip, actorID); err != nil {
			return err
		}

		detail, _, err = changeStatus(ctx, tx, trip, to)
		return err
	})
	if err != nil {
		return nil, err
	}
	return detail, nil
}

// changeStatus performs one transition and records what it means on the bus.
//
// Three things happen here and they happen together, in the caller's
// transaction, under the caller's row lock on the trip:
//
//  1. domain.Can decides whether the edge is legal. The transition table in
//     internal/domain/status.go is consulted here and nowhere else.
//  2. The status is written.
//  3. A trip.status_changed outbox row is written, and — on the edge into
//     `completed` — a trip.completed one as well.
//
// `trip` must already be locked and must be the row as it was read under that
// lock; the old status in the event comes from it. Returns the reloaded detail
// and whether a trip.completed was emitted, which the scheduler counts.
func changeStatus(ctx context.Context, tx pgx.Tx, trip domain.Trip, to domain.Status) (*domain.TripDetail, bool, error) {
	if !domain.Can(trip.Status, to) {
		return nil, false, &domain.TransitionError{From: trip.Status, To: to}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE trips SET status = $2, updated_at = now() WHERE id = $1`,
		trip.ID, to,
	); err != nil {
		return nil, false, fmt.Errorf("update trip status: %w", err)
	}

	detail, err := loadDetail(ctx, tx, trip.ID)
	if err != nil {
		return nil, false, err
	}

	// changed_at is the row's new updated_at rather than a Go time.Now(): the
	// transition happened when the transaction wrote it, and taking the moment
	// from the row is what keeps the event and the record it describes agreeing
	// about when — including across a clock skew between this process and the
	// database.
	changedAt := detail.Trip.UpdatedAt
	if err := insertOutbox(ctx, tx, trip.ID, events.TypeTripStatusChanged, events.TripStatusChanged{
		TripID:      trip.ID,
		OrganizerID: trip.OrganizerID,
		OldStatus:   string(trip.Status),
		NewStatus:   string(to),
		ChangedAt:   events.Timestamp(changedAt),
	}); err != nil {
		return nil, false, err
	}

	// The three terminal-ish edges each have an event of their own alongside
	// the status change, because their consumers and payloads differ. A
	// consumer that only cares about the status filters on new_status; one
	// that has to act — provision a room, open a rating window, tell everyone
	// it is off — reads the event that carries what it needs.
	switch to {
	case domain.StatusRecruiting:
		// draft -> recruiting is publication, and publication is what
		// contracts/events.md means by "a trip has been created": a draft is
		// visible to nobody but its organizer, and a chat room for one would be
		// a room for a trip that may never be announced. The edge is only
		// reachable from draft (see internal/domain/status.go), so this cannot
		// fire twice for one trip.
		if err := insertOutbox(ctx, tx, trip.ID, events.TypeTripCreated, events.TripCreated{
			TripID:          trip.ID,
			OrganizerID:     trip.OrganizerID,
			Title:           detail.Trip.Title,
			Category:        detail.Trip.Category,
			StartsAt:        events.Timestamp(detail.Trip.StartAt),
			EndsAt:          events.Timestamp(detail.Trip.EndAt),
			MaxParticipants: detail.Trip.Capacity,
			Status:          "open",
		}); err != nil {
			return nil, false, err
		}

	case domain.StatusCancelled:
		affected, err := affectedByCancellation(ctx, tx, trip.ID, trip.OrganizerID)
		if err != nil {
			return nil, false, err
		}
		if err := insertOutbox(ctx, tx, trip.ID, events.TypeTripCancelled, events.TripCancelled{
			TripID:      trip.ID,
			Title:       detail.Trip.Title,
			OrganizerID: trip.OrganizerID,
			Reason:      events.CancelledByOrganizer,
			// Null, always: the cancel endpoint takes no body. See the payload.
			Comment:         nil,
			CancelledAt:     events.Timestamp(changedAt),
			AffectedUserIDs: affected,
		}); err != nil {
			return nil, false, err
		}

	case domain.StatusCompleted:
		emitted, err := emitTripCompleted(ctx, tx, detail, changedAt)
		if err != nil {
			return nil, false, err
		}
		return detail, emitted, nil
	}

	return detail, false, nil
}

// affectedByCancellation lists everyone the cancellation is bad news for,
// except the organizer who caused it.
//
// Two groups, and the second is the one that is easy to forget: the approved
// participants, who were going, and the people whose join request was still
// open, who were waiting for an answer they are now never going to get. A
// cancellation that only told the first group would leave the second refreshing
// a page.
//
// UNION rather than UNION ALL, because a user can be in both sets in exactly
// one situation — a pending request from somebody who was separately added —
// and telling them twice is a duplicate email.
func affectedByCancellation(ctx context.Context, tx pgx.Tx, tripID, organizerID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `
		SELECT user_id FROM participants
		WHERE trip_id = $1 AND user_id <> $2
		UNION
		SELECT user_id FROM join_requests
		WHERE trip_id = $1 AND status = 'pending' AND user_id <> $2`,
		tripID, organizerID,
	)
	if err != nil {
		return nil, fmt.Errorf("read the users affected by cancelling %s: %w", tripID, err)
	}
	defer rows.Close()

	// Never nil: `affected_user_ids` is a required array in the catalogue, and
	// a nil slice marshals to `null`, which every consumer would then have to
	// guard.
	affected := []uuid.UUID{}
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("scan affected user: %w", err)
		}
		affected = append(affected, userID)
	}
	return affected, rows.Err()
}

// emitTripCompleted writes the roster event, at most once per trip, and reports
// whether it wrote one.
//
// Two guards, and they are not the same guard twice:
//
//   - **Nobody to rate.** A trip whose roster is its organizer and no one else
//     completes normally but emits nothing. `trip.completed` exists to open a
//     rating window, and a window in which one person may rate themselves is a
//     mail that wastes somebody's afternoon.
//
//   - **Exactly once.** trips.completed_event_emitted is flipped in the same
//     statement that tests it, so the emit is decided by the database rather
//     than by a read the caller did a moment earlier. The status check would
//     very nearly do — `completed` is terminal, so the edge cannot be walked
//     twice — but "very nearly" is not the right strength of claim about the
//     one event that opens a rating window, and this column is what makes a
//     future backfill, replay or hand-run of the scheduler safe by
//     construction.
func emitTripCompleted(ctx context.Context, tx pgx.Tx, detail *domain.TripDetail, completedAt time.Time) (bool, error) {
	roster := make([]events.TripCompletedParticipant, 0, len(detail.Participants))
	others := 0
	for _, p := range detail.Participants {
		if p.Role != domain.RoleOrganizer {
			others++
		}
		roster = append(roster, events.TripCompletedParticipant{
			UserID:   p.UserID,
			Role:     p.Role,
			JoinedAt: events.Timestamp(p.JoinedAt),
		})
	}
	if others == 0 {
		return false, nil
	}

	// The trip row is already locked by the caller, so this cannot race; the
	// predicate is here so that the flag and the outbox row are decided by one
	// statement rather than by a check followed by a write.
	tag, err := tx.Exec(ctx, `
		UPDATE trips
		SET completed_event_emitted = true
		WHERE id = $1 AND NOT completed_event_emitted`, detail.Trip.ID)
	if err != nil {
		return false, fmt.Errorf("claim trip.completed emission: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}

	return true, insertOutbox(ctx, tx, detail.Trip.ID, events.TypeTripCompleted, events.TripCompleted{
		TripID:      detail.Trip.ID,
		Title:       detail.Trip.Title,
		OrganizerID: detail.Trip.OrganizerID,
		StartedAt:   events.Timestamp(detail.Trip.StartAt),
		CompletedAt: events.Timestamp(completedAt),
		// Absolute rather than a duration, so every consumer agrees on the
		// deadline without sharing a constant across two languages.
		RatingWindowClosesAt: events.Timestamp(completedAt.Add(domain.RatingWindow)),
		Participants:         roster,
	})
}

// UpdateTrip applies a partial edit.
//
// The patch is merged onto the stored trip and the *result* is validated, not
// the patch: "end_at must not be before start_at" is a statement about the trip,
// and a request that moves only end_at can still break it.
func (s *Store) UpdateTrip(ctx context.Context, tripID, actorID uuid.UUID, patch domain.TripPatch) (*domain.TripDetail, error) {
	var detail *domain.TripDetail

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if err := requireOrganizer(trip, actorID); err != nil {
			return err
		}
		if !domain.IsEditable(trip.Status) {
			return fmt.Errorf("%w: status is %s", domain.ErrNotEditable, trip.Status)
		}

		current, err := currentInput(ctx, tx, trip, patch.Points != nil)
		if err != nil {
			return err
		}

		merged := patch.Apply(current).Normalize()
		// The future-start rule applies only when the request actually carries
		// start_at. A trip that begins in an hour must still accept a fix to
		// its description.
		if err := merged.Validate(s.now(), patch.StartAt != nil); err != nil {
			return err
		}

		// Capacity is bounded below by the people already approved, not just by
		// CapacityMin. This is the same invariant as the trips_approved_fits
		// check constraint; catching it here is what turns it into a 409 with a
		// reason instead of a 500.
		if merged.Capacity < trip.ApprovedCount {
			return fmt.Errorf("%w: %d approved, capacity %d",
				domain.ErrCapacityBelowApproved, trip.ApprovedCount, merged.Capacity)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE trips
			SET title = $2, description = $3, category = $4, capacity = $5,
			    start_at = $6, end_at = $7, updated_at = now()
			WHERE id = $1`,
			tripID, merged.Title, merged.Description, merged.Category, merged.Capacity,
			merged.StartAt, merged.EndAt,
		); err != nil {
			return fmt.Errorf("update trip: %w", err)
		}

		if patch.Points != nil {
			if err := replacePoints(ctx, tx, tripID, merged.Points); err != nil {
				return err
			}
		}

		detail, err = loadDetail(ctx, tx, tripID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return detail, nil
}

// DeleteTrip hard-deletes a draft.
//
// Draft only, and that is a product rule rather than a technical one: once a
// trip has been published other people have seen it, may have asked to join it
// and may be talking about it in a chat room. Such a trip is cancelled, which
// leaves a record and lets the notification service tell everyone. Erasing it
// would make those references dangle.
//
// The cascade on trip_points and participants means one DELETE is enough.
func (s *Store) DeleteTrip(ctx context.Context, tripID, actorID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if err := requireOrganizer(trip, actorID); err != nil {
			return err
		}
		if trip.Status != domain.StatusDraft {
			return fmt.Errorf("%w: status is %s", domain.ErrNotDraft, trip.Status)
		}

		if _, err := tx.Exec(ctx, `DELETE FROM trips WHERE id = $1`, tripID); err != nil {
			return fmt.Errorf("delete trip: %w", err)
		}
		return nil
	})
}

// currentInput rebuilds a TripInput from the stored trip, so a patch has
// something complete to merge onto.
//
// Points are read only when the patch is going to replace them; when it is not,
// they cannot fail validation and fetching them would be a round trip spent on
// data nothing looks at. The placeholder keeps the merged input's route long
// enough to satisfy the length check.
func currentInput(ctx context.Context, tx pgx.Tx, trip domain.Trip, pointsReplaced bool) (domain.TripInput, error) {
	in := domain.TripInput{
		Title:       trip.Title,
		Description: trip.Description,
		Category:    trip.Category,
		Capacity:    trip.Capacity,
		StartAt:     trip.StartAt,
		EndAt:       trip.EndAt,
	}

	if pointsReplaced {
		// The patch overwrites Points wholesale, so the stored route is never
		// read: whatever is there loses. Left empty rather than nil so that
		// TripPatch.Apply's "nil means unchanged" test still means what it says.
		in.Points = []domain.PointInput{}
		return in, nil
	}

	stored, err := loadPoints(ctx, tx, trip.ID)
	if err != nil {
		return domain.TripInput{}, err
	}
	in.Points = make([]domain.PointInput, len(stored))
	for i, p := range stored {
		in.Points[i] = domain.PointInput{
			Name: p.Name, Lat: p.Lat, Lng: p.Lng,
			ArriveAt: p.ArriveAt, Transport: p.Transport,
		}
	}
	return in, nil
}

// insertPoints writes the route and the two denormalised copies of its
// endpoints on the trips row.
//
// One statement for all the points: unnest zips the parallel arrays into rows,
// so a twenty-stop route is one round trip rather than twenty. ST_MakePoint
// cannot take unnest() as an argument — a set-returning function has to appear
// at the top level of FROM — which is why the arrays are unnested there and the
// geography is built from the resulting columns.
func insertPoints(ctx context.Context, tx pgx.Tx, tripID uuid.UUID, points []domain.PointInput) error {
	ids := make([]uuid.UUID, len(points))
	seqs := make([]int32, len(points))
	names := make([]string, len(points))
	lats := make([]float64, len(points))
	lngs := make([]float64, len(points))
	arrivals := make([]*time.Time, len(points))
	transports := make([]*string, len(points))

	for i, p := range points {
		ids[i] = uuid.New()
		seqs[i] = int32(i)
		names[i] = p.Name
		lats[i] = p.Lat
		lngs[i] = p.Lng
		arrivals[i] = p.ArriveAt
		transports[i] = p.Transport
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO trip_points (id, trip_id, seq, name, location, arrive_at, transport)
		SELECT p.id, $1, p.seq, p.name,
		       ST_SetSRID(ST_MakePoint(p.lng, p.lat), 4326)::geography,
		       p.arrive_at, p.transport
		FROM unnest($2::uuid[], $3::int[], $4::text[], $5::float8[], $6::float8[],
		            $7::timestamptz[], $8::text[])
		     AS p(id, seq, name, lat, lng, arrive_at, transport)`,
		tripID, ids, seqs, names, lats, lngs, arrivals, transports,
	); err != nil {
		return fmt.Errorf("insert trip points: %w", err)
	}

	return syncEndpoints(ctx, tx, tripID, points)
}

// replacePoints swaps a trip's whole route.
//
// Delete-then-insert rather than a diff: seq is dense and unique per trip, so
// reordering three stops out of five would need a temporary shuffle to avoid
// colliding with the rows still holding those numbers. The route is at most
// twenty rows and this runs inside the transaction that already holds the trip
// locked, so the simple version costs nothing worth optimising away.
func replacePoints(ctx context.Context, tx pgx.Tx, tripID uuid.UUID, points []domain.PointInput) error {
	if _, err := tx.Exec(ctx, `DELETE FROM trip_points WHERE trip_id = $1`, tripID); err != nil {
		return fmt.Errorf("delete trip points: %w", err)
	}
	return insertPoints(ctx, tx, tripID, points)
}

// syncEndpoints rewrites trips.departure_location and trips.destination_location
// from the route's first and last stop.
//
// This is the invariant the migration's comment promises: the denormalised
// columns are only ever written here, and this is only ever called from
// insertPoints, in the same transaction as the trip_points write. There is no
// path that changes the route without passing through it.
func syncEndpoints(ctx context.Context, tx pgx.Tx, tripID uuid.UUID, points []domain.PointInput) error {
	first := points[0]
	last := points[len(points)-1]

	if _, err := tx.Exec(ctx, `
		UPDATE trips
		SET departure_location   = ST_SetSRID(ST_MakePoint($2, $3), 4326)::geography,
		    destination_location = ST_SetSRID(ST_MakePoint($4, $5), 4326)::geography
		WHERE id = $1`,
		tripID, first.Lng, first.Lat, last.Lng, last.Lat,
	); err != nil {
		return fmt.Errorf("sync denormalised trip endpoints: %w", err)
	}
	return nil
}
