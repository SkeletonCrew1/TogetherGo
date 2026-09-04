package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/togethergo/chat/internal/domain"
)

// Access is what the socket handshake and every read endpoint need to know
// about one caller and one room, answered in a single round trip.
//
// One query rather than three, because the three answers are only useful
// together and asking separately would let them disagree: a room could be
// cancelled between "does it exist" and "am I still in it", and the handler
// would be acting on a state that never existed.
type Access struct {
	Room     domain.Room
	IsMember bool
}

// Authorize answers "may this user open this room, and is it still open".
//
// It returns domain.ErrRoomNotFound when the projection has not caught up.
// That is a normal condition and not an error in the system: this service never
// asks the trip service whether a room ought to exist (CLAUDE.md rule 1). The
// connection is refused, the client retries, and by then the event has almost
// certainly arrived — the gap is one outbox poll plus one broker hop.
func (s *Store) Authorize(ctx context.Context, tripID, userID uuid.UUID) (Access, error) {
	var access Access

	err := s.pool.QueryRow(ctx, `
		SELECT r.trip_id, r.title, r.status, r.created_at,
		       m.user_id IS NOT NULL AS is_member
		FROM rooms r
		LEFT JOIN room_members m
		       ON m.trip_id = r.trip_id AND m.user_id = $2 AND m.left_at IS NULL
		WHERE r.trip_id = $1`,
		tripID, userID,
	).Scan(
		&access.Room.TripID,
		&access.Room.Title,
		&access.Room.Status,
		&access.Room.CreatedAt,
		&access.IsMember,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Access{}, domain.ErrRoomNotFound
	}
	if err != nil {
		return Access{}, fmt.Errorf("authorize %s in room %s: %w", userID, tripID, err)
	}
	return access, nil
}

// IsMember is Authorize when the caller only needs the membership half — the
// ticket endpoint, which grants a ticket for a cancelled room too because its
// history is still readable.
func (s *Store) IsMember(ctx context.Context, tripID, userID uuid.UUID) (bool, error) {
	access, err := s.Authorize(ctx, tripID, userID)
	if err != nil {
		return false, err
	}
	return access.IsMember, nil
}

// RoomsCursor is the position of the last row of a page of rooms, in the order
// the list sorts by: (last_activity DESC, trip_id DESC).
//
// Both halves are needed. A timestamp alone is not unique — two rooms can have
// been created in the same transaction, and two messages can share a
// microsecond — so a cursor holding only the timestamp either skips rows or
// repeats them forever. The trip id is what makes the ordering total.
//
// OFFSET would need none of this and is not used, here or anywhere (CLAUDE.md):
// it makes the database count and discard every row of every preceding page,
// and it silently drops or repeats rows when a message arrives between two
// requests — which, in a chat room list ordered by recency, is constantly.
type RoomsCursor struct {
	LastActivity time.Time
	TripID       uuid.UUID
}

// ListRooms returns the caller's rooms, most recently active first.
//
// Rooms the caller has left are excluded. Their messages stay visible to
// everyone still in the room, but the room itself is no longer theirs to open —
// which is the same rule Authorize applies, expressed once more here because
// this query cannot reuse it without a round trip per room.
func (s *Store) ListRooms(ctx context.Context, userID uuid.UUID, cursor *RoomsCursor, limit int) ([]domain.RoomSummary, error) {
	// The two lateral joins are what keep this to one query instead of one plus
	// two per room. Each is driven by messages_trip_id_desc_idx: the first is a
	// single backwards index read per room, and the second is a range count
	// over the tail of the same index.
	//
	// The unread count excludes the caller's own messages. A room is not unread
	// because you were the last to speak in it, and counting your own words
	// would leave every sender with a badge they cannot clear.
	const query = `
		SELECT r.trip_id, r.title, r.status, r.created_at,
		       last.id, last.sender_id, last.body, last.created_at,
		       unread.count,
		       COALESCE(last.created_at, r.created_at) AS last_activity
		FROM room_members m
		JOIN rooms r ON r.trip_id = m.trip_id
		LEFT JOIN LATERAL (
			SELECT id, sender_id, body, created_at
			FROM messages
			WHERE trip_id = m.trip_id
			ORDER BY id DESC
			LIMIT 1
		) last ON true
		CROSS JOIN LATERAL (
			SELECT count(*) AS count
			FROM messages
			WHERE trip_id = m.trip_id
			  AND id > m.last_read_message_id
			  AND sender_id <> m.user_id
		) unread
		WHERE m.user_id = $1
		  AND m.left_at IS NULL
		  AND ($2::timestamptz IS NULL OR
		       (COALESCE(last.created_at, r.created_at), r.trip_id) < ($2::timestamptz, $3::uuid))
		ORDER BY last_activity DESC, r.trip_id DESC
		LIMIT $4`

	var (
		cursorTime *time.Time
		cursorID   *uuid.UUID
	)
	if cursor != nil {
		cursorTime, cursorID = &cursor.LastActivity, &cursor.TripID
	}

	rows, err := s.pool.Query(ctx, query, userID, cursorTime, cursorID, limit)
	if err != nil {
		return nil, fmt.Errorf("list rooms for %s: %w", userID, err)
	}
	defer rows.Close()

	summaries := make([]domain.RoomSummary, 0, limit)
	for rows.Next() {
		var (
			summary   domain.RoomSummary
			messageID *int64
			senderID  *uuid.UUID
			body      *string
			messageAt *time.Time
		)
		if err := rows.Scan(
			&summary.Room.TripID, &summary.Room.Title, &summary.Room.Status, &summary.Room.CreatedAt,
			&messageID, &senderID, &body, &messageAt,
			&summary.UnreadCount,
			&summary.LastActivity,
		); err != nil {
			return nil, fmt.Errorf("scan room summary: %w", err)
		}
		if messageID != nil {
			summary.LastMessage = &domain.Message{
				ID:        *messageID,
				TripID:    summary.Room.TripID,
				SenderID:  *senderID,
				Body:      *body,
				CreatedAt: *messageAt,
			}
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list rooms for %s: %w", userID, err)
	}
	return summaries, nil
}
