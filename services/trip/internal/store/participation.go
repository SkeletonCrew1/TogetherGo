package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/events"
)

// The participation flow: asking to join a trip, deciding on the ask, leaving,
// being removed, and being invited.
//
// Every method here is one transaction that starts with `SELECT ... FOR UPDATE`
// on the trip. That is not ceremony. The rules being enforced — "there is room",
// "this request is still open", "this person is on the roster" — are statements
// about rows that a concurrent request is trying to change, and checking them
// on a snapshot taken before the write is checking them against a past that may
// no longer be true. The row lock is the mechanism; there is no application
// mutex anywhere in this package, and adding one would be strictly worse
// (it would not survive a second replica, which the lock does).

const joinRequestColumns = `
	jr.id, jr.trip_id, jr.user_id, jr.status, jr.message, jr.decision_reason,
	jr.created_at, jr.decided_at, jr.decided_by`

func joinRequestScanTargets(r *domain.JoinRequest) []any {
	return []any{
		&r.ID, &r.TripID, &r.UserID, &r.Status, &r.Message, &r.DecisionReason,
		&r.CreatedAt, &r.DecidedAt, &r.DecidedBy,
	}
}

func scanJoinRequest(row pgx.Row) (domain.JoinRequest, error) {
	var r domain.JoinRequest
	err := row.Scan(joinRequestScanTargets(&r)...)
	return r, err
}

// CreateJoinRequest records one user's application to join a trip.
//
// Four distinct refusals, and they are distinct on purpose — each one means a
// different thing is wrong and a different thing should happen next:
//
//	ErrOwnTrip            the organizer is already aboard; the button was a bug
//	ErrTripNotRecruiting  the trip is not taking applications; refetch it
//	ErrAlreadyParticipant already in; the client's copy is stale
//	ErrRequestPending     already asked; wait, or cancel the request first
//
// A trip nobody is allowed to see answers ErrTripNotFound before any of them,
// for the same reason GET does: a 403 on somebody's draft would confirm the id
// names a real trip.
func (s *Store) CreateJoinRequest(ctx context.Context, tripID, userID uuid.UUID, message *string) (*domain.JoinRequest, error) {
	if err := domain.ValidateJoinMessage(message); err != nil {
		return nil, err
	}

	var created domain.JoinRequest

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if !trip.IsVisibleTo(userID) {
			return domain.ErrTripNotFound
		}
		if trip.OrganizerID == userID {
			return domain.ErrOwnTrip
		}
		if trip.Status != domain.StatusRecruiting {
			return fmt.Errorf("%w: status is %s", domain.ErrTripNotRecruiting, trip.Status)
		}

		onRoster, err := isParticipant(ctx, tx, tripID, userID)
		if err != nil {
			return err
		}
		if onRoster {
			return domain.ErrAlreadyParticipant
		}

		created = domain.JoinRequest{
			ID:      uuid.New(),
			TripID:  tripID,
			UserID:  userID,
			Status:  domain.JoinPending,
			Message: message,
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO join_requests (id, trip_id, user_id, status, message)
			VALUES ($1, $2, $3, 'pending', $4)
			RETURNING created_at`,
			created.ID, tripID, userID, message,
		).Scan(&created.CreatedAt)
		if err != nil {
			// The partial unique index is the check, rather than a SELECT
			// followed by an INSERT. Both would work — the trip row lock above
			// already serialises requests on one trip — but a constraint that
			// the database enforces cannot be skipped by a future code path
			// that forgets to look first, and "did this insert violate it" is
			// exactly as much information as the SELECT would have given.
			if uniqueViolation(err, "join_requests_one_pending_idx") {
				return domain.ErrRequestPending
			}
			return fmt.Errorf("insert join request: %w", err)
		}

		return insertOutbox(ctx, tx, tripID, events.TypeJoinRequestCreated, events.JoinRequestCreated{
			JoinRequestID: created.ID,
			TripID:        tripID,
			Title:         trip.Title,
			OrganizerID:   trip.OrganizerID,
			RequesterID:   userID,
			Message:       message,
			RequestedAt:   events.Timestamp(created.CreatedAt),
		})
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// CancelJoinRequest withdraws the caller's own pending request.
//
// No event. Nothing outside this service acted on the request — the organizer
// was told about it and can see it disappear from their queue on the next read,
// and there is no membership, no room and no mail to undo. An event nobody
// consumes is a queue that fills up.
//
// approved_count is not touched, because a pending request never occupied a
// seat. That is the whole reason the counter is incremented at approval time
// and not at request time.
func (s *Store) CancelJoinRequest(ctx context.Context, tripID, userID uuid.UUID) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		// The trip is locked even though only join_requests is written: the
		// cancel races an approval of the same request, and both have to agree
		// on who got there first.
		if _, err := lockTrip(ctx, tx, tripID); err != nil {
			return err
		}

		tag, err := tx.Exec(ctx, `
			UPDATE join_requests
			SET status = 'cancelled', decided_at = now(), decided_by = $2
			WHERE trip_id = $1 AND user_id = $2 AND status = 'pending'`,
			tripID, userID,
		)
		if err != nil {
			return fmt.Errorf("cancel join request: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// No open request: either there never was one, or it has already
			// been decided. Both are "there is nothing here to cancel", and the
			// caller gets one 404 rather than a taxonomy of absences.
			return domain.ErrRequestNotFound
		}
		return nil
	})
}

// JoinRequests is the organizer's queue for one trip: pending first, newest
// first within each group.
//
// Pending first because that is the only part the organizer can act on, and a
// decided request is history that should not push a live one below the fold.
// Newest first within each group because a queue read on a phone is read from
// the top and the interesting request is the one that just arrived.
//
// The requester's name, photo and rating are *not* here. Users belong to
// identity and there is no join across that boundary (CLAUDE.md rule 1); the
// HTTP layer resolves the whole page's user ids in one batch call, exactly as
// the discovery endpoint resolves its organizers.
func (s *Store) JoinRequests(ctx context.Context, tripID, actorID uuid.UUID, query domain.JoinRequestQuery) (*domain.JoinRequestPage, error) {
	query = query.Normalize()

	trip, err := s.Trip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if err := requireOrganizer(trip, actorID); err != nil {
		return nil, err
	}

	// One row more than asked for. Its existence is what says there is a next
	// page; a count(*) would be a second query answering a question with one
	// bit in it.
	args := []any{tripID, query.Limit + 1}
	keyset := ""
	if c := query.Cursor; c != nil {
		// `(status <> 'pending')` is the pending-first rank, as an integer so
		// it can be compared with `>`. The second half is the ordinary keyset
		// comparison, applied only within the group the cursor points into.
		keyset = `
		  AND ( (jr.status <> 'pending')::int > $3
		        OR ( (jr.status <> 'pending')::int = $3
		             AND (jr.created_at, jr.id) < ($4, $5) ) )`
		rank := 0
		if c.Decided {
			rank = 1
		}
		args = append(args, rank, c.CreatedAt, c.ID)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT`+joinRequestColumns+`
		FROM join_requests jr
		WHERE jr.trip_id = $1`+keyset+`
		ORDER BY (jr.status <> 'pending')::int, jr.created_at DESC, jr.id DESC
		LIMIT $2`, args...)
	if err != nil {
		return nil, fmt.Errorf("select join requests: %w", err)
	}
	defer rows.Close()

	items := make([]domain.JoinRequest, 0, query.Limit)
	for rows.Next() {
		var r domain.JoinRequest
		if err := rows.Scan(joinRequestScanTargets(&r)...); err != nil {
			return nil, fmt.Errorf("scan join request: %w", err)
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate join requests: %w", err)
	}

	page := &domain.JoinRequestPage{Items: items}
	if len(items) > query.Limit {
		page.Items = items[:query.Limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &domain.JoinRequestCursor{
			Decided:   last.Status != domain.JoinPending,
			CreatedAt: last.CreatedAt,
			ID:        last.ID,
		}
	}
	return page, nil
}

// MyJoinRequest reads the caller's own most recent request on a trip.
//
// The most recent, not the pending one: a user who was rejected has no pending
// request but does have a rejection to read, and the organizer's free-text
// reason is served here and nowhere else — contracts/events.md leaves it off
// the bus precisely so that this endpoint is the only way to see it.
//
// There is deliberately no "read anybody's request by id" method. An
// application is between two people; the organizer sees the whole queue through
// JoinRequests, the requester sees their own through this, and nobody else sees
// any of it.
func (s *Store) MyJoinRequest(ctx context.Context, tripID, userID uuid.UUID) (*domain.JoinRequest, error) {
	trip, err := s.Trip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if !trip.IsVisibleTo(userID) {
		return nil, domain.ErrTripNotFound
	}

	request, err := scanJoinRequest(s.pool.QueryRow(ctx,
		`SELECT`+joinRequestColumns+`
		 FROM join_requests jr
		 WHERE jr.trip_id = $1 AND jr.user_id = $2
		 ORDER BY jr.created_at DESC, jr.id DESC
		 LIMIT 1`,
		tripID, userID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRequestNotFound
		}
		return nil, fmt.Errorf("select join request: %w", err)
	}
	return &request, nil
}

// ApproveJoinRequest is the capacity invariant.
//
// Everything below happens in one transaction, in this order, and the order is
// the point:
//
//  1. SELECT the trip FOR UPDATE
//  2. the trip is recruiting and approved_count < capacity
//  3. the request is still pending
//  4. the request becomes approved
//  5. a participants row appears
//  6. approved_count goes up by one
//  7. join_request.approved goes into the outbox
//
// Step 1 is what makes steps 2 to 7 mean anything. Two organizers — or one
// organizer's double-click, or twelve concurrent requests, which is what
// TestConcurrentApprovalsRespectCapacity fires — all reach step 1, and Postgres
// lets exactly one of them past it at a time. The others block, and when they
// are let through they re-read `approved_count` and see the increment the
// winner committed. There is no window between "there is room" and "the seat is
// taken" for a second transaction to look into.
//
// An application-level mutex would give the same answer on one process and the
// wrong answer on two. The row lock is the mechanism, and it is the reason the
// service can be scaled without a word of coordination code.
//
// The trips_approved_fits CHECK constraint is the backstop under all of this.
// If this method were ever wrong, the database would refuse the write rather
// than record an overbooked trip.
func (s *Store) ApproveJoinRequest(ctx context.Context, tripID, requestID, actorID uuid.UUID) (*domain.JoinRequest, error) {
	var approved domain.JoinRequest

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// 1.
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if err := requireOrganizer(trip, actorID); err != nil {
			return err
		}

		// 2.
		if trip.Status != domain.StatusRecruiting {
			return fmt.Errorf("%w: status is %s", domain.ErrTripNotRecruiting, trip.Status)
		}
		if trip.ApprovedCount >= trip.Capacity {
			return fmt.Errorf("%w: %d of %d seats taken", domain.ErrTripFull, trip.ApprovedCount, trip.Capacity)
		}

		// 3.
		request, err := lockJoinRequest(ctx, tx, tripID, requestID)
		if err != nil {
			return err
		}
		if !request.IsPending() {
			return fmt.Errorf("%w: status is %s", domain.ErrRequestNotPending, request.Status)
		}
		// Unreachable through the API — a participant cannot open a request and
		// a pending request cannot survive its own approval — but the insert in
		// step 5 would otherwise answer a corrupt state with a primary-key
		// violation and a 500. This turns it into the truth.
		onRoster, err := isParticipant(ctx, tx, tripID, request.UserID)
		if err != nil {
			return err
		}
		if onRoster {
			return domain.ErrAlreadyParticipant
		}

		// 4.
		approved = request
		approved.Status = domain.JoinApproved
		approved.DecidedBy = &actorID
		if err := tx.QueryRow(ctx, `
			UPDATE join_requests
			SET status = 'approved', decided_at = now(), decided_by = $2
			WHERE id = $1
			RETURNING decided_at`,
			requestID, actorID,
		).Scan(&approved.DecidedAt); err != nil {
			return fmt.Errorf("approve join request: %w", err)
		}

		// 5.
		if _, err := tx.Exec(ctx, `
			INSERT INTO participants (trip_id, user_id, role)
			VALUES ($1, $2, $3)`,
			tripID, request.UserID, domain.RoleParticipant,
		); err != nil {
			return fmt.Errorf("insert participant: %w", err)
		}

		// 6. `approved_count + 1`, not a literal computed in Go: the read that
		// produced trip.ApprovedCount is protected by the lock, but writing the
		// increment as an expression means the row and the number can never
		// disagree even if that ever stops being true.
		var participantCount int
		if err := tx.QueryRow(ctx, `
			UPDATE trips
			SET approved_count = approved_count + 1, updated_at = now()
			WHERE id = $1
			RETURNING approved_count`, tripID,
		).Scan(&participantCount); err != nil {
			if constraint, ok := checkViolation(err); ok {
				// trips_approved_fits fired. Reaching this means the check
				// above did not hold, which would be a real bug — but the
				// database has already refused the overbooking, which is what
				// the constraint is for.
				return fmt.Errorf("%w: %s", domain.ErrTripFull, constraint)
			}
			return fmt.Errorf("increment approved_count: %w", err)
		}

		// 7. Fat on purpose: chat has to create the room membership and
		// notification has to write the mail, and neither may call back into
		// this service to find out the trip's title or who organises it.
		return insertOutbox(ctx, tx, tripID, events.TypeJoinRequestApproved, events.JoinRequestApproved{
			JoinRequestID:    approved.ID,
			TripID:           tripID,
			Title:            trip.Title,
			OrganizerID:      trip.OrganizerID,
			ParticipantID:    approved.UserID,
			ApprovedAt:       events.Timestamp(*approved.DecidedAt),
			ParticipantCount: participantCount,
			MaxParticipants:  trip.Capacity,
		})
	})
	if err != nil {
		return nil, err
	}
	return &approved, nil
}

// RejectJoinRequest declines an application.
//
// approved_count is untouched: a pending request never held a seat, so there is
// nothing to give back. The organizer's free-text reason is stored and served
// to the requester; what goes on the bus is the reason *code*, because a
// notification template can act on `declined_by_organizer` and cannot act on a
// sentence (contracts/events.md, join_request.rejected).
//
// Unlike approval this does not require the trip to still be recruiting. An
// organizer who has filled their trip must still be able to clear the queue,
// and refusing to let them would leave requests pending forever.
func (s *Store) RejectJoinRequest(ctx context.Context, tripID, requestID, actorID uuid.UUID, reason *string) (*domain.JoinRequest, error) {
	if err := domain.ValidateDecisionReason(reason); err != nil {
		return nil, err
	}

	var rejected domain.JoinRequest

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if err := requireOrganizer(trip, actorID); err != nil {
			return err
		}

		request, err := lockJoinRequest(ctx, tx, tripID, requestID)
		if err != nil {
			return err
		}
		if !request.IsPending() {
			return fmt.Errorf("%w: status is %s", domain.ErrRequestNotPending, request.Status)
		}

		rejected = request
		rejected.Status = domain.JoinRejected
		rejected.DecisionReason = reason
		rejected.DecidedBy = &actorID
		if err := tx.QueryRow(ctx, `
			UPDATE join_requests
			SET status = 'rejected', decided_at = now(), decided_by = $2, decision_reason = $3
			WHERE id = $1
			RETURNING decided_at`,
			requestID, actorID, reason,
		).Scan(&rejected.DecidedAt); err != nil {
			return fmt.Errorf("reject join request: %w", err)
		}

		return insertOutbox(ctx, tx, tripID, events.TypeJoinRequestRejected, events.JoinRequestRejected{
			JoinRequestID: rejected.ID,
			TripID:        tripID,
			Title:         trip.Title,
			OrganizerID:   trip.OrganizerID,
			RequesterID:   rejected.UserID,
			Reason:        events.RejectedByOrganizer,
			RejectedAt:    events.Timestamp(*rejected.DecidedAt),
		})
	})
	if err != nil {
		return nil, err
	}
	return &rejected, nil
}

// RemoveParticipant is the organizer taking somebody off the trip.
//
// The organizer cannot remove themselves — see removeParticipant — and the
// removal is not allowed once the trip has ended: a completed trip's roster has
// already gone out on trip.completed and identity has opened a rating window
// against exactly those people. Rewriting it afterwards would make the two
// disagree with no way to reconcile them.
func (s *Store) RemoveParticipant(ctx context.Context, tripID, userID, actorID uuid.UUID) (*domain.TripDetail, error) {
	var detail *domain.TripDetail

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if err := requireOrganizer(trip, actorID); err != nil {
			return err
		}
		if err := removeParticipant(ctx, tx, trip, userID, actorID, events.RemovedByOrganizer); err != nil {
			return err
		}
		detail, err = loadDetail(ctx, tx, tripID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return detail, nil
}

// LeaveTrip is a participant taking themselves off it.
//
// The same write as a removal, and the same event: chat has to revoke access
// either way, and `removed_by` plus `reason` are what tell the two apart.
func (s *Store) LeaveTrip(ctx context.Context, tripID, userID uuid.UUID) (*domain.TripDetail, error) {
	var detail *domain.TripDetail

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if !trip.IsVisibleTo(userID) {
			return domain.ErrTripNotFound
		}
		if err := removeParticipant(ctx, tx, trip, userID, userID, events.RemovedVoluntarily); err != nil {
			return err
		}
		detail, err = loadDetail(ctx, tx, tripID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return detail, nil
}

// removeParticipant is the shared write behind both departures.
//
// The order is delete, decrement, publish, and the decrement is guarded by the
// delete: `RowsAffected() == 0` is how "was this person actually on the trip"
// is answered, so a repeated removal cannot drive approved_count below the
// organizer's own seat. The trips_approved_fits constraint would catch it if it
// could, but only after the counter had already been wrong.
func removeParticipant(ctx context.Context, tx pgx.Tx, trip domain.Trip, userID, removedBy uuid.UUID, reason string) error {
	if trip.OrganizerID == userID {
		return domain.ErrCannotRemoveOrganizer
	}
	if domain.IsTerminal(trip.Status) {
		return fmt.Errorf("%w: status is %s", domain.ErrTripEnded, trip.Status)
	}

	tag, err := tx.Exec(ctx, `
		DELETE FROM participants
		WHERE trip_id = $1 AND user_id = $2 AND role = $3`,
		trip.ID, userID, domain.RoleParticipant,
	)
	if err != nil {
		return fmt.Errorf("delete participant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotParticipant
	}

	var participantCount int
	if err := tx.QueryRow(ctx, `
		UPDATE trips
		SET approved_count = approved_count - 1, updated_at = now()
		WHERE id = $1
		RETURNING approved_count`, trip.ID,
	).Scan(&participantCount); err != nil {
		return fmt.Errorf("decrement approved_count: %w", err)
	}

	// The approved join request is deliberately left as `approved`. It is the
	// record of a decision that really was made; overwriting it would erase the
	// fact that the organizer once said yes, and the roster — not the request
	// log — is what says who is on the trip now.

	return insertOutbox(ctx, tx, trip.ID, events.TypeParticipantRemoved, events.ParticipantRemoved{
		TripID:           trip.ID,
		ParticipantID:    userID,
		RemovedBy:        removedBy,
		Reason:           reason,
		RemovedAt:        events.Timestamp(time.Now()),
		ParticipantCount: participantCount,
	})
}

// InviteUsers invites named users to a trip directly.
//
// All or nothing. An organizer who names twenty people and has already invited
// one of them gets a 409 listing that one and no rows are written, rather than
// nineteen invitations and a silence about the twentieth. The conflicting ids
// travel in the error (see domain.UserConflictError) so the SPA can say which
// row to fix instead of making the organizer bisect their own request.
func (s *Store) InviteUsers(ctx context.Context, tripID, actorID uuid.UUID, userIDs []uuid.UUID) ([]domain.Invite, error) {
	wanted, err := domain.NormalizeInviteIDs(userIDs)
	if err != nil {
		return nil, err
	}

	var invites []domain.Invite

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		trip, err := lockTrip(ctx, tx, tripID)
		if err != nil {
			return err
		}
		if err := requireOrganizer(trip, actorID); err != nil {
			return err
		}
		// A draft is invisible to the invitee, so an invitation to one is a
		// link to a 404. Publish first, then invite.
		if trip.Status != domain.StatusRecruiting {
			return fmt.Errorf("%w: status is %s", domain.ErrTripNotRecruiting, trip.Status)
		}

		for _, id := range wanted {
			if id == trip.OrganizerID {
				return &domain.UserConflictError{Reason: domain.ErrCannotInviteSelf, UserIDs: []uuid.UUID{id}}
			}
		}
		if clash, err := conflictingUsers(ctx, tx, `
			SELECT user_id FROM participants WHERE trip_id = $1 AND user_id = ANY($2)`,
			tripID, wanted); err != nil {
			return err
		} else if len(clash) > 0 {
			return &domain.UserConflictError{Reason: domain.ErrAlreadyParticipant, UserIDs: clash}
		}
		if clash, err := conflictingUsers(ctx, tx, `
			SELECT invited_user_id FROM trip_invites WHERE trip_id = $1 AND invited_user_id = ANY($2)`,
			tripID, wanted); err != nil {
			return err
		} else if len(clash) > 0 {
			return &domain.UserConflictError{Reason: domain.ErrAlreadyInvited, UserIDs: clash}
		}

		invites = make([]domain.Invite, 0, len(wanted))
		for _, id := range wanted {
			invite := domain.Invite{ID: uuid.New(), TripID: tripID, InvitedUserID: id}
			if err := tx.QueryRow(ctx, `
				INSERT INTO trip_invites (id, trip_id, invited_user_id)
				VALUES ($1, $2, $3)
				RETURNING created_at`,
				invite.ID, tripID, id,
			).Scan(&invite.CreatedAt); err != nil {
				if uniqueViolation(err, "trip_invites_trip_id_invited_user_id_key") {
					// Lost a race with another invitation of the same person.
					// The trip lock makes that all but impossible; reported
					// honestly rather than as a 500 if it ever happens.
					return &domain.UserConflictError{Reason: domain.ErrAlreadyInvited, UserIDs: []uuid.UUID{id}}
				}
				return fmt.Errorf("insert trip invite: %w", err)
			}

			if err := insertOutbox(ctx, tx, tripID, events.TypeTripInviteSent, events.TripInviteSent{
				TripID:    tripID,
				Title:     trip.Title,
				InviteID:  invite.ID,
				InviterID: trip.OrganizerID,
				InviteeID: id,
				ExpiresAt: events.Timestamp(invite.ExpiresAt()),
				SentAt:    events.Timestamp(invite.CreatedAt),
			}); err != nil {
				return err
			}
			invites = append(invites, invite)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return invites, nil
}

// Invites lists a trip's invitations. Organizer only: who was asked and who
// declined to answer is the organizer's business.
func (s *Store) Invites(ctx context.Context, tripID, actorID uuid.UUID) ([]domain.Invite, error) {
	trip, err := s.Trip(ctx, tripID)
	if err != nil {
		return nil, err
	}
	if err := requireOrganizer(trip, actorID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, trip_id, invited_user_id, created_at
		FROM trip_invites
		WHERE trip_id = $1
		ORDER BY created_at DESC, id DESC`, tripID)
	if err != nil {
		return nil, fmt.Errorf("select trip invites: %w", err)
	}
	defer rows.Close()

	invites := make([]domain.Invite, 0, 8)
	for rows.Next() {
		var i domain.Invite
		if err := rows.Scan(&i.ID, &i.TripID, &i.InvitedUserID, &i.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan trip invite: %w", err)
		}
		invites = append(invites, i)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate trip invites: %w", err)
	}
	return invites, nil
}

// lockJoinRequest reads a request inside a transaction and holds it locked.
//
// The trip is already locked by the time this runs, which serialises every
// decision on every request of that trip — but locking the request row too is
// what makes the "still pending" check mean something under a future change
// that stops taking the trip lock first.
func lockJoinRequest(ctx context.Context, tx pgx.Tx, tripID, requestID uuid.UUID) (domain.JoinRequest, error) {
	request, err := scanJoinRequest(tx.QueryRow(ctx,
		`SELECT`+joinRequestColumns+` FROM join_requests jr WHERE jr.id = $1 AND jr.trip_id = $2 FOR UPDATE`,
		requestID, tripID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Includes "exists, but on a different trip". A request id is only
			// meaningful in the context of its trip, and treating a mismatch as
			// a 404 keeps the two from being probed against each other.
			return domain.JoinRequest{}, domain.ErrRequestNotFound
		}
		return domain.JoinRequest{}, fmt.Errorf("select join request for update: %w", err)
	}
	return request, nil
}

func isParticipant(ctx context.Context, q querier, tripID, userID uuid.UUID) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM participants WHERE trip_id = $1 AND user_id = $2)`,
		tripID, userID,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check participant: %w", err)
	}
	return exists, nil
}

// conflictingUsers runs a one-column id query and collects the results, so the
// two membership checks in InviteUsers are one code path rather than two
// near-identical loops.
func conflictingUsers(ctx context.Context, q querier, sql string, tripID uuid.UUID, ids []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.Query(ctx, sql, tripID, ids)
	if err != nil {
		return nil, fmt.Errorf("check invite conflicts: %w", err)
	}
	defer rows.Close()

	var found []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan invite conflict: %w", err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate invite conflicts: %w", err)
	}
	return found, nil
}

// uniqueViolation reports whether err is a Postgres unique violation on the
// named constraint or index.
//
// Named rather than matched on the SQLSTATE alone: two different unique
// constraints on one statement mean two different domain errors, and "23505"
// on its own does not say which.
func uniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
