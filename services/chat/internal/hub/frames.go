// Package hub owns the websocket protocol: the frames, one goroutine pair per
// connection, and the map from rooms to the sockets this instance holds.
//
// It knows nothing about how a connection was authenticated. By the time Serve
// is called the caller has already redeemed a ticket and re-checked membership
// against the database; this package's job starts at "a verified user is on the
// other end of this socket".
package hub

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Frame types, client to server.
const (
	frameMessage = "message"
	frameTyping  = "typing"
	frameRead    = "read"
)

// inboundFrame is every client frame, decoded in one pass.
//
// A flat struct with the union of all three shapes rather than a two-stage
// decode into json.RawMessage: there are three frames, they share no fields
// that mean different things, and one Unmarshal is both faster and easier to
// read than a switch that unmarshals twice.
type inboundFrame struct {
	Type string `json:"type"`

	// message
	ClientMsgID string `json:"client_msg_id"`
	Body        string `json:"body"`

	// read
	LastMessageID int64 `json:"last_message_id"`
}

// Server-to-client frames. Each is a struct rather than a map so that the field
// names are checked at compile time and appear here, in one place, exactly as
// they go on the wire.

// MessageFrame is a persisted message, delivered to everyone in the room.
//
// ClientMsgID is echoed so the sender can reconcile the optimistic row it
// rendered before the round trip instead of showing the message twice. It is
// sent to every recipient, not only the sender: it is a uuid the client
// generated, it identifies nothing, and one pre-encoded frame written to every
// socket is what keeps the fan-out to a single marshal per message.
type MessageFrame struct {
	Type        string    `json:"type"`
	ID          int64     `json:"id"`
	ClientMsgID string    `json:"client_msg_id"`
	TripID      uuid.UUID `json:"trip_id"`
	SenderID    uuid.UUID `json:"sender_id"`
	Body        string    `json:"body"`
	CreatedAt   string    `json:"created_at"`
}

// PresenceFrame announces an arrival or a departure.
type PresenceFrame struct {
	Type   string    `json:"type"`
	UserID uuid.UUID `json:"user_id"`
	Status string    `json:"status"`
}

// Presence statuses.
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// TypingFrame says somebody is composing. Not persisted, not acknowledged, and
// not worth a retry if it is lost.
type TypingFrame struct {
	Type   string    `json:"type"`
	UserID uuid.UUID `json:"user_id"`
}

// ErrorFrame is a refusal that does not close the connection.
//
// It carries the same `code` vocabulary the REST endpoints use, for the same
// reason: the client switches on the code and never on the message text.
type ErrorFrame struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error frame codes.
const (
	CodeInvalidFrame     = "invalid_frame"
	CodeUnsupportedFrame = "unsupported_frame"
	CodeValidationError  = "validation_error"
	CodeRateLimited      = "rate_limited"
	CodeRoomClosed       = "room_closed"
	CodeNotMember        = "not_member"
	CodeInternalError    = "internal_error"
)

func newMessageFrame(id int64, clientMsgID string, tripID, senderID uuid.UUID, body string, createdAt time.Time) MessageFrame {
	return MessageFrame{
		Type:        frameMessage,
		ID:          id,
		ClientMsgID: clientMsgID,
		TripID:      tripID,
		SenderID:    senderID,
		Body:        body,
		// RFC 3339 in UTC, like every timestamp this system puts on a wire
		// (CLAUDE.md). Nanosecond precision, unlike the event bus's: two
		// messages a millisecond apart are ordinary in a chat room, and a
		// client sorting by timestamp needs to be able to tell them apart.
		CreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
	}
}

func newErrorFrame(code, message string) ErrorFrame {
	return ErrorFrame{Type: "error", Code: code, Message: message}
}

// envelope is what travels between replicas on Redis. It is not a client frame
// and never reaches a socket as-is.
//
// Two fields beyond the payload, and both exist because the fan-out is a
// broadcast that occasionally needs to not be:
//
//   - Except omits one user. A typing indicator echoed back to its own author
//     is noise on every keystroke; nothing else uses it.
//   - Evict names a user whose sockets in this room must be closed, which is
//     how `participant.removed` reaches the replica holding that user's
//     connection. The consumer that handled the event has no idea which
//     instance that is, and asking would mean a service discovery mechanism
//     for something Redis already does.
//
// Exactly one of Frame and Evict is set.
type envelope struct {
	Frame  json.RawMessage `json:"frame,omitempty"`
	Except *uuid.UUID      `json:"except,omitempty"`
	Evict  *uuid.UUID      `json:"evict,omitempty"`
}
