package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/togethergo/chat/internal/domain"
	"github.com/togethergo/chat/internal/events"
)

// This file is the write half of the room projection: the four handlers behind
// the `chat.trip-events` consumer.
//
// Nothing here is authoritative. The trip service owns who is on a trip and
// what it is called; `rooms` and `room_members` are a copy fed by events, and
// their only job is to let the socket handshake answer "may this user read this
// room" out of the local database instead of over HTTP. This service never
// calls trip synchronously — that is the rule this whole file exists to keep.
//
// Every handler has the same shape, and it is the one contracts/events.md
// prescribes:
//
//  1. claim the event_id in processed_events, inside the transaction;
//  2. if the claim found the row already there, the event has been handled —
//     commit and report `false`, and the consumer acks without acting;
//  3. otherwise apply the write and commit.
//
// The claim comes first so it takes the row lock before any work happens: two
// consumers handed the same event_id concurrently serialise on it, and the
// loser finds the row on its retry. The writes are idempotent on their own too
// — every one of them is an upsert — because processed_events is the belt and
// the upsert is the braces.

// markProcessed records that an event has been handled, and reports whether
// this call is the one that recorded it.
//
// `ON CONFLICT DO NOTHING` rather than a SELECT followed by an INSERT: the
// check and the claim are one statement, so two consumers racing on a
// redelivery cannot both decide they are first.
func markProcessed(ctx context.Context, tx pgx.Tx, eventID uuid.UUID) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO processed_events (event_id) VALUES ($1)
		ON CONFLICT (event_id) DO NOTHING`, eventID)
	if err != nil {
		return false, fmt.Errorf("mark event %s processed: %w", eventID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkProcessed records an event that needs no projection write — an
// event_type this build does not recognise, which contracts/events.md says to
// log, mark processed and ack rather than to dead-letter.
func (s *Store) MarkProcessed(ctx context.Context, eventID uuid.UUID) (bool, error) {
	var first bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = markProcessed(ctx, tx, eventID)
		return err
	})
	return first, err
}

// ensureRoom provisions a room if there is not one, and refreshes its title if
// there is.
//
// Called by all three of the events that can be the *first* one this service
// sees for a trip, which is why it exists at all. There is no ordering
// guarantee on the bus, so `join_request.approved` can arrive before the
// `trip.created` that ought to have preceded it; all three payloads carry the
// title, so any of them can create the room and the others become a title
// refresh.
//
// The status is never written here, only on insert. That is the important half:
// a `trip.cancelled` handled before a late `trip.created` must not be undone by
// it, and an approval that races a cancellation must not reopen the room. The
// one writer of an existing room's status is ApplyTripCancelled, and it only
// ever moves it one way.
func ensureRoom(ctx context.Context, tx pgx.Tx, tripID uuid.UUID, title, status string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO rooms (trip_id, title, status)
		VALUES ($1, $2, $3)
		ON CONFLICT (trip_id) DO UPDATE SET title = EXCLUDED.title`,
		tripID, title, status,
	)
	if err != nil {
		return fmt.Errorf("upsert room %s: %w", tripID, err)
	}
	return nil
}

// addMember adds a member, or brings one back who had left.
//
// `left_at = NULL` on conflict is what makes a re-approval work: a participant
// who left and was approved again is the same row, with their read watermark
// and their `joined_at` intact. Resetting the watermark would drop them back
// into a room showing every message as unread.
func addMember(ctx context.Context, tx pgx.Tx, tripID, userID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO room_members (trip_id, user_id)
		VALUES ($1, $2)
		ON CONFLICT (trip_id, user_id) DO UPDATE SET left_at = NULL`,
		tripID, userID,
	)
	if err != nil {
		return fmt.Errorf("add member %s to room %s: %w", userID, tripID, err)
	}
	return nil
}

// ApplyTripCreated provisions the room and seats the organizer in it.
//
// The room is created at `trip.created` rather than at the first approval so
// that the organizer has somewhere to post before anybody has joined — which is
// what contracts/events.md says this event is for.
func (s *Store) ApplyTripCreated(ctx context.Context, eventID uuid.UUID, p events.TripCreated) (bool, error) {
	var first bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = markProcessed(ctx, tx, eventID)
		if err != nil || !first {
			return err
		}
		if err := ensureRoom(ctx, tx, p.TripID, p.Title, domain.StatusRecruiting); err != nil {
			return err
		}
		return addMember(ctx, tx, p.TripID, p.OrganizerID)
	})
	return first, err
}

// ApplyJoinApproved adds the newly approved participant.
//
// This is the only event in the system that grants chat access. There is no
// other writer of room_members that adds anybody, and no endpoint that does —
// which is what makes "an unapproved user cannot open the websocket" a property
// of the schema rather than of a check somebody has to remember to write.
func (s *Store) ApplyJoinApproved(ctx context.Context, eventID uuid.UUID, p events.JoinRequestApproved) (bool, error) {
	var first bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = markProcessed(ctx, tx, eventID)
		if err != nil || !first {
			return err
		}
		if err := ensureRoom(ctx, tx, p.TripID, p.Title, domain.StatusRecruiting); err != nil {
			return err
		}
		return addMember(ctx, tx, p.TripID, p.ParticipantID)
	})
	return first, err
}

// ApplyParticipantRemoved revokes access without deleting anything.
//
// `left_at` is set, the row stays, and the messages the participant wrote stay
// in the room attributed to them. contracts/events.md is explicit that removal
// is forward-looking: it revokes access, it does not delete what the
// participant already said, and it must not leave the people still in the room
// reading a conversation with holes in it.
//
// The `WHERE left_at IS NULL` keeps a redelivery from moving the timestamp
// forward. It is not load-bearing — processed_events already stops the second
// delivery — but it means the column answers "when did they leave" rather than
// "when was the last time we were told".
func (s *Store) ApplyParticipantRemoved(ctx context.Context, eventID uuid.UUID, p events.ParticipantRemoved) (bool, error) {
	var first bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = markProcessed(ctx, tx, eventID)
		if err != nil || !first {
			return err
		}

		// No room upsert here, and no error when nothing is updated. An event
		// removing somebody from a room this service has never heard of is a
		// no-op, not a failure: there is no access to revoke, and the
		// `join_request.approved` that would have granted it will be discarded
		// by the same reasoning if it ever turns up.
		_, err = tx.Exec(ctx, `
			UPDATE room_members
			SET left_at = now()
			WHERE trip_id = $1 AND user_id = $2 AND left_at IS NULL`,
			p.TripID, p.ParticipantID,
		)
		if err != nil {
			return fmt.Errorf("remove member %s from room %s: %w", p.ParticipantID, p.TripID, err)
		}
		return nil
	})
	return first, err
}

// ApplyTripCancelled closes the room to new messages.
//
// Read-only, not gone. Everyone who was on the trip keeps the conversation, and
// InsertMessage is what enforces the closure — see the WHERE clause there,
// which is the same check on the write path rather than a flag anybody has to
// consult first.
func (s *Store) ApplyTripCancelled(ctx context.Context, eventID uuid.UUID, p events.TripCancelled) (bool, error) {
	var first bool
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var err error
		first, err = markProcessed(ctx, tx, eventID)
		if err != nil || !first {
			return err
		}

		// Insert-or-close rather than a plain UPDATE, so that a cancellation
		// which overtakes its own `trip.created` still lands. The room is then
		// born closed, which is the correct end state and saves a repair when
		// the creation event finally arrives (ensureRoom never rewrites a
		// status).
		_, err = tx.Exec(ctx, `
			INSERT INTO rooms (trip_id, title, status)
			VALUES ($1, $2, $3)
			ON CONFLICT (trip_id) DO UPDATE SET status = EXCLUDED.status`,
			p.TripID, p.Title, domain.StatusCancelled,
		)
		if err != nil {
			return fmt.Errorf("cancel room %s: %w", p.TripID, err)
		}
		return nil
	})
	return first, err
}
