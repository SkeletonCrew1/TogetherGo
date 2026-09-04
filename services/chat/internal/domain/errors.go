// Package domain holds the chat service's types and its rules: what a message
// body may be, what a room's status means, and the sentinel errors every layer
// above maps onto a status code or a close code.
//
// Nothing here talks to Postgres, Redis or a socket. The store decides *when*
// to ask these questions; this package is the answer.
package domain

import "errors"

// Sentinel domain errors. Every one of these is mapped onto an HTTP status and
// an API error code in exactly one place — internal/http/errors.go — and onto a
// websocket close code in one other, internal/http/ws.go. Handlers return them
// as-is; nothing outside those two mappers decides what a failure looks like on
// the wire.
var (
	// ErrRoomNotFound is the projection not having caught up yet, and it is a
	// normal condition rather than an error in the system.
	//
	// This service never calls trip to find out whether a room ought to exist
	// (CLAUDE.md rule 1, and the prompt for this stage is explicit about it).
	// If `trip.created` has not arrived, the room does not exist here, the
	// connection is refused, and the client retries — by which time the event
	// will almost certainly have landed, because the two are separated by one
	// outbox poll and one broker hop.
	ErrRoomNotFound = errors.New("room not found")

	// ErrNotMember is a user who is not on the trip, or is no longer on it.
	// Both answer the same question — may this caller read and write this room
	// — and both have the same remedy, which is none.
	ErrNotMember = errors.New("caller is not a member of this room")

	// ErrRoomClosed is a cancelled trip. History stays readable; nothing new
	// may be written.
	ErrRoomClosed = errors.New("room is closed to new messages")

	// ErrRateLimited is the per-user, per-room message limit. It never closes a
	// connection: a client that types too fast is a client, not an attacker,
	// and dropping its socket would cost it the whole conversation.
	ErrRateLimited = errors.New("message rate limit exceeded")

	// ErrTicketInvalid covers missing, expired, already-spent and
	// wrong-trip tickets — one error for all four, for the same reason
	// auth.ErrInvalidToken covers five token failures: the client's next move
	// is identical in every case (ask for a new ticket), and distinguishing
	// them out loud only helps someone probing the endpoint.
	ErrTicketInvalid = errors.New("websocket ticket is missing, expired or does not match this trip")
)

// ValidationError is a request or a frame this service will not act on, with
// the offending fields named. Mapped to 400 / validation_error over HTTP and to
// an `error` frame with code `invalid_frame` on a socket.
type ValidationError struct {
	Fields []FieldError
}

// FieldError names one rejected field and says why, in a sentence that starts
// with a verb so the client can render "body " + message.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "the request is invalid"
	}
	return e.Fields[0].Field + " " + e.Fields[0].Message
}

// invalid builds a single-field validation error, which is what almost every
// call site needs.
func invalid(field, message string) *ValidationError {
	return &ValidationError{Fields: []FieldError{{Field: field, Message: message}}}
}
