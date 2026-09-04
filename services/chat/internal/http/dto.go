package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/togethergo/chat/internal/domain"
	"github.com/togethergo/chat/internal/store"
)

// maxBodyBytes caps a request body before anything decodes it. Every body this
// service accepts is two short fields; a client sending more has a bug or an
// intention, and neither is worth an allocation.
const maxBodyBytes = 8 << 10

// page is the list shape CLAUDE.md fixes for every list endpoint in the system:
// the items, and an opaque cursor for the next page or null when there is none.
type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// messageResponse is one message on the wire. The field names match the socket's
// `message` frame exactly — `id`, `trip_id`, `sender_id`, `body`, `created_at` —
// so a client that renders a message from history and one that renders it from
// the socket use the same code.
type messageResponse struct {
	ID        int64     `json:"id"`
	TripID    uuid.UUID `json:"trip_id"`
	SenderID  uuid.UUID `json:"sender_id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// roomResponse is one row of GET /api/chat/rooms.
type roomResponse struct {
	TripID uuid.UUID `json:"trip_id"`
	Title  string    `json:"title"`
	Status string    `json:"status"`

	// IsOpen is `status != "cancelled"`, served alongside it because it is the
	// question the composer actually asks and every client would otherwise
	// derive it — three times, and wrongly once.
	IsOpen bool `json:"is_open"`

	// LastMessage is null in a room where nobody has said anything yet.
	LastMessage *messageResponse `json:"last_message"`

	UnreadCount int       `json:"unread_count"`
	CreatedAt   time.Time `json:"created_at"`
}

// ticketResponse is the websocket credential.
//
// `expires_in` in seconds rather than an absolute `expires_at`: the client's
// only use for it is to decide whether to reuse the ticket or fetch another,
// and a relative number cannot be got wrong by a browser whose clock is off.
type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresIn int    `json:"expires_in"`
}

// ticketRequest is the body of POST /api/chat/tickets.
type ticketRequest struct {
	TripID string `json:"trip_id"`
}

func newMessageResponse(message domain.Message) messageResponse {
	return messageResponse{
		ID:       message.ID,
		TripID:   message.TripID,
		SenderID: message.SenderID,
		Body:     message.Body,
		// Forced to UTC on the way out. Postgres hands back timestamptz in the
		// session's time zone, so without this the same message could serialise
		// with a different offset depending on which replica read it —
		// identical instants that no client would compare as equal (CLAUDE.md:
		// UTC, RFC 3339).
		CreatedAt: message.CreatedAt.UTC(),
	}
}

func newRoomResponse(summary domain.RoomSummary) roomResponse {
	room := roomResponse{
		TripID:      summary.Room.TripID,
		Title:       summary.Room.Title,
		Status:      summary.Room.Status,
		IsOpen:      summary.Room.IsOpen(),
		UnreadCount: summary.UnreadCount,
		CreatedAt:   summary.Room.CreatedAt.UTC(),
	}
	if summary.LastMessage != nil {
		last := newMessageResponse(*summary.LastMessage)
		room.LastMessage = &last
	}
	return room
}

// decodeBody reads and decodes a JSON request body, refusing anything it does
// not recognise.
//
// DisallowUnknownFields is deliberate: a client sending `{"tripId": ...}`
// against an API that wants `trip_id` gets told so, rather than a 400 about a
// missing field it is sure it sent.
func decodeBody(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return errBadBody{reason: "The request body is not valid JSON for this endpoint."}
	}
	// A second value in the stream means the client sent two objects, which is
	// never intentional and would otherwise be silently ignored.
	if decoder.More() {
		return errBadBody{reason: "The request body must be a single JSON object."}
	}
	return nil
}

// pathUUID reads a uuid out of a URL segment.
func pathUUID(raw, field string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, &domain.ValidationError{Fields: []domain.FieldError{{
			Field:   field,
			Message: "must be a uuid",
		}}}
	}
	return parsed, nil
}

// messagesQuery is the parsed query string of the history endpoint.
type messagesQuery struct {
	beforeID int64
	limit    int
}

// parseMessagesQuery reads `before_id`, `cursor` and `limit`.
//
// Two names for one position, and both are honoured on purpose. `before_id` is
// the readable form — a message id a client already has, which is what makes
// "load the fifty before this one" obvious from the URL. `cursor` is the opaque
// form CLAUDE.md fixes for every list endpoint, and it is what `next_cursor`
// hands back, so a client that only ever echoes the cursor never has to know
// what is inside it.
//
// Sending both is an error rather than a precedence rule. They mean the same
// thing, so a request carrying two different values is a client bug, and
// picking a winner would hide it.
func parseMessagesQuery(r *http.Request) (messagesQuery, error) {
	query := r.URL.Query()
	parsed := messagesQuery{}

	rawBefore := strings.TrimSpace(query.Get("before_id"))
	rawCursor := strings.TrimSpace(query.Get("cursor"))

	switch {
	case rawBefore != "" && rawCursor != "":
		return parsed, &domain.ValidationError{Fields: []domain.FieldError{{
			Field:   "before_id",
			Message: "cannot be combined with cursor; they are two spellings of the same position",
		}}}

	case rawBefore != "":
		beforeID, err := strconv.ParseInt(rawBefore, 10, 64)
		if err != nil || beforeID < 0 {
			return parsed, &domain.ValidationError{Fields: []domain.FieldError{{
				Field:   "before_id",
				Message: "must be a non-negative message id",
			}}}
		}
		parsed.beforeID = beforeID

	case rawCursor != "":
		beforeID, err := decodeCursor(rawCursor)
		if err != nil {
			return parsed, err
		}
		parsed.beforeID = beforeID
	}

	limit := 0
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return parsed, &domain.ValidationError{Fields: []domain.FieldError{{
				Field:   "limit",
				Message: "must be an integer",
			}}}
		}
		limit = value
	}

	cleaned, err := domain.CleanLimit(limit)
	if err != nil {
		return parsed, err
	}
	parsed.limit = cleaned
	return parsed, nil
}

// The message cursor is a base64url'd message id, and nothing more.
//
// Base64 is not encryption and is not meant to be. It says "this is our string,
// not yours", which keeps clients from building cursors by hand and then
// depending on the format — the same reason the trip service encodes its own.
func encodeCursor(beforeID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(beforeID, 10)))
}

func decodeCursor(raw string) (int64, error) {
	invalid := func() error {
		return &domain.ValidationError{Fields: []domain.FieldError{{
			Field:   "cursor",
			Message: "is not a valid cursor; omit it to start from the newest message",
		}}}
	}

	// Bounded before anything decodes it: a real cursor is a dozen characters,
	// and a client sending a megabyte should be rejected by a length check
	// rather than by an allocator.
	if len(raw) > 64 {
		return 0, invalid()
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, invalid()
	}
	beforeID, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil || beforeID < 0 {
		return 0, invalid()
	}
	return beforeID, nil
}

// roomsCursor is the opaque form of store.RoomsCursor.
//
// Two halves, separated by a character neither can contain: RFC 3339 has no
// '|', and neither does a uuid. RFC3339Nano rather than RFC3339 because
// timestamptz keeps microseconds, and a cursor truncated to the second would
// re-read every room that shares that second with the last row of the page.
const roomsCursorSeparator = "|"

func encodeRoomsCursor(cursor store.RoomsCursor) string {
	raw := cursor.LastActivity.UTC().Format(time.RFC3339Nano) + roomsCursorSeparator + cursor.TripID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeRoomsCursor(raw string) (*store.RoomsCursor, error) {
	invalid := func(reason string) error {
		return &domain.ValidationError{Fields: []domain.FieldError{{
			Field:   "cursor",
			Message: fmt.Sprintf("is not a valid cursor (%s); omit it to start from the first page", reason),
		}}}
	}

	if raw == "" {
		return nil, nil
	}
	if len(raw) > 256 {
		return nil, invalid("too long")
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, invalid("not base64")
	}

	activity, tripID, found := strings.Cut(string(decoded), roomsCursorSeparator)
	if !found {
		return nil, invalid("wrong shape")
	}
	parsedActivity, err := time.Parse(time.RFC3339Nano, activity)
	if err != nil {
		return nil, invalid("bad timestamp")
	}
	parsedTripID, err := uuid.Parse(tripID)
	if err != nil {
		return nil, invalid("bad id")
	}
	return &store.RoomsCursor{LastActivity: parsedActivity.UTC(), TripID: parsedTripID}, nil
}
