package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/togethergo/chat/internal/domain"
)

// InsertMessage persists one message, or explains why it would not.
//
// The membership and openness checks are *in the INSERT*, not before it. A
// handler that read the room, decided the sender was still in it and then
// inserted would be deciding on a snapshot: a `participant.removed` committed
// in between would be ignored, and the removed user's message would land in a
// room they had already been thrown out of. As one statement there is no
// window, and the check is against the same rows the insert takes its lock on.
//
// The cost is that a refusal comes back as zero rows rather than as a reason,
// so the reason is looked up afterwards — one extra query on a path that is
// only taken when something has already gone wrong.
func (s *Store) InsertMessage(ctx context.Context, tripID, senderID uuid.UUID, body string) (domain.Message, error) {
	message := domain.Message{TripID: tripID, SenderID: senderID, Body: body}

	err := s.pool.QueryRow(ctx, `
		INSERT INTO messages (trip_id, sender_id, body)
		SELECT $1, $2, $3
		WHERE EXISTS (
			SELECT 1
			FROM rooms r
			JOIN room_members m ON m.trip_id = r.trip_id
			WHERE r.trip_id = $1
			  AND m.user_id = $2
			  AND m.left_at IS NULL
			  AND r.status <> $4
		)
		RETURNING id, created_at`,
		tripID, senderID, body, domain.StatusCancelled,
	).Scan(&message.ID, &message.CreatedAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Message{}, s.refusalReason(ctx, tripID, senderID)
	}
	if err != nil {
		return domain.Message{}, fmt.Errorf("insert message into room %s: %w", tripID, err)
	}
	return message, nil
}

// refusalReason turns "the INSERT matched nothing" into the sentinel that says
// why, so the sender gets an error frame they can act on rather than a generic
// failure.
//
// It re-reads, and by then the state could have changed again — a room
// cancelled in the microsecond between the insert and this query would be
// reported as ErrRoomClosed when the insert actually failed on membership. That
// is acceptable and unavoidable: both answers are "no", the sender's next move
// is the same, and the alternative is holding a transaction open across a check
// that exists only to produce a nicer message.
func (s *Store) refusalReason(ctx context.Context, tripID, senderID uuid.UUID) error {
	access, err := s.Authorize(ctx, tripID, senderID)
	if err != nil {
		return err
	}
	if !access.IsMember {
		return domain.ErrNotMember
	}
	if !access.Room.IsOpen() {
		return domain.ErrRoomClosed
	}
	// Membership and status both say yes, so the row must have appeared
	// between the two queries. Report it as a retryable failure rather than
	// inventing a reason.
	return fmt.Errorf("message into room %s was refused for no discoverable reason", tripID)
}

// ListMessages returns a page of history, newest first.
//
// Newest first is the order a chat is read in: the client wants the bottom of
// the room, and pages backwards from there. `beforeID` of 0 means "start at the
// bottom", which is what a client with no cursor sends and what an empty
// `before_id` parameter decodes to.
//
// The extra row is how `next_cursor` is decided without a second count query:
// limit+1 rows are asked for, and if the last one comes back it is dropped and
// the page is reported as having more.
func (s *Store) ListMessages(ctx context.Context, tripID uuid.UUID, beforeID int64, limit int) (messages []domain.Message, hasMore bool, err error) {
	// $2 = 0 is the "no cursor" case rather than a separate query, so both
	// paths use the same plan: a backwards range scan of
	// messages_trip_id_desc_idx.
	rows, err := s.pool.Query(ctx, `
		SELECT id, trip_id, sender_id, body, created_at
		FROM messages
		WHERE trip_id = $1 AND ($2 = 0 OR id < $2)
		ORDER BY id DESC
		LIMIT $3`,
		tripID, beforeID, limit+1,
	)
	if err != nil {
		return nil, false, fmt.Errorf("list messages in room %s: %w", tripID, err)
	}
	defer rows.Close()

	messages = make([]domain.Message, 0, limit)
	for rows.Next() {
		var message domain.Message
		if err := rows.Scan(&message.ID, &message.TripID, &message.SenderID, &message.Body, &message.CreatedAt); err != nil {
			return nil, false, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list messages in room %s: %w", tripID, err)
	}

	if len(messages) > limit {
		return messages[:limit], true, nil
	}
	return messages, false, nil
}

// MarkRead moves a member's read watermark.
//
// GREATEST, so the watermark only ever advances. Two tabs open on the same room
// will both send `read` frames, and the one that is scrolled further back would
// otherwise undo the other's — leaving an unread badge that reappears every
// time the user looks away from the wrong window.
//
// A watermark past the newest message is not rejected. It is harmless (the
// unread count is a range that comes back empty) and refusing it would mean a
// round trip to find the newest id first, on the hottest and least important
// write in the service.
func (s *Store) MarkRead(ctx context.Context, tripID, userID uuid.UUID, lastMessageID int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE room_members
		SET last_read_message_id = GREATEST(last_read_message_id, $3)
		WHERE trip_id = $1 AND user_id = $2 AND left_at IS NULL`,
		tripID, userID, lastMessageID,
	)
	if err != nil {
		return fmt.Errorf("mark room %s read for %s: %w", tripID, userID, err)
	}
	return nil
}
