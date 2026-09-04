package domain

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel domain errors. Every one of these is mapped onto an HTTP status and
// an API error code in exactly one place — internal/http/errors.go. Handlers
// return them as-is; nothing outside that mapper decides what a domain failure
// looks like on the wire.
var (
	// ErrTripNotFound covers both "no such id" and "this id exists but you are
	// not allowed to know that" — see Trip.IsVisibleTo.
	ErrTripNotFound = errors.New("trip not found")

	// ErrNotOrganizer is a genuine 403: the caller can see the trip, but only
	// its organizer may change it.
	ErrNotOrganizer = errors.New("caller is not the trip organizer")

	// ErrInvalidTransition is the state machine refusing an edge.
	ErrInvalidTransition = errors.New("invalid status transition")

	// ErrNotEditable is a trip whose fields are frozen because it is already
	// under way, finished or cancelled.
	ErrNotEditable = errors.New("trip is not editable in its current status")

	// ErrNotDraft guards the hard delete. Once a trip has been published other
	// people have seen it, so it is cancelled, never erased.
	ErrNotDraft = errors.New("trip is not a draft")

	// ErrCapacityBelowApproved is an edit that would shrink a trip below the
	// number of people already on it.
	ErrCapacityBelowApproved = errors.New("capacity is below the number of approved participants")

	// The participation flow. Every refusal below is a distinct error code on
	// the wire, because "you cannot join this trip" is four different problems
	// with four different things the client should do about them: one is a bug
	// in its own UI, one is a stale trip, one is a duplicate submit and one is
	// a race it lost.

	// ErrOwnTrip is an organizer asking to join their own trip. They are
	// already on the roster; the button should not have been there.
	ErrOwnTrip = errors.New("the organizer is already on this trip")

	// ErrTripNotRecruiting is an application to a trip that is not taking any:
	// still a draft, already under way, finished or called off.
	ErrTripNotRecruiting = errors.New("trip is not recruiting")

	// ErrAlreadyParticipant is an application from somebody already on the
	// roster — usually a stale client that has not seen its own approval yet.
	ErrAlreadyParticipant = errors.New("user is already a participant")

	// ErrRequestPending is a second application from a user whose first is
	// still open. Not an error the user can fix by trying harder: the answer is
	// to wait, or to cancel the one they have.
	ErrRequestPending = errors.New("user already has a pending request on this trip")

	// ErrTripFull is the capacity invariant refusing an approval. It is the one
	// failure in this flow that is nobody's mistake: two organizers, or one
	// organizer double-clicking, can both pass every check and only one of them
	// can have the last seat. The row lock decides which.
	ErrTripFull = errors.New("trip is full")

	// ErrRequestNotFound is a request id that does not exist, or belongs to a
	// different trip. The two are one error for the same reason a draft is a
	// 404: the id space should not be probeable.
	ErrRequestNotFound = errors.New("join request not found")

	// ErrRequestNotPending is a decision on a request that has already been
	// decided — the second of two approvals, or an approval racing a cancel.
	ErrRequestNotPending = errors.New("join request is no longer pending")

	// ErrNotParticipant is a removal or a departure aimed at somebody who is
	// not on the roster.
	ErrNotParticipant = errors.New("user is not a participant on this trip")

	// ErrCannotRemoveOrganizer covers both halves of the same rule: the
	// organizer may not remove themselves, and may not be removed. A trip
	// without an organizer has nobody who can approve, cancel or complete it.
	ErrCannotRemoveOrganizer = errors.New("the organizer cannot leave or be removed from their own trip")

	// ErrTripEnded is a roster change on a completed or cancelled trip. The
	// roster of a finished trip is history — trip.completed has already been
	// published carrying it, and identity has opened a rating window against
	// exactly those people.
	ErrTripEnded = errors.New("trip has already ended")

	// ErrAlreadyInvited is an invitation to somebody who has one.
	ErrAlreadyInvited = errors.New("user has already been invited to this trip")

	// ErrCannotInviteSelf is the organizer naming themselves in an invite. A
	// separate sentinel from ErrOwnTrip even though the underlying fact is the
	// same one, because the two produce different advice: "you are already on
	// this trip" is the answer to a join, and "remove yourself from the list"
	// is the answer to an invitation.
	ErrCannotInviteSelf = errors.New("the organizer cannot invite themselves")
)

// TransitionError carries the two ends of a refused transition so the response
// can say what was attempted and what would have been allowed.
type TransitionError struct {
	From Status
	To   Status
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("cannot move trip from %s to %s", e.From, e.To)
}

// Unwrap makes errors.Is(err, ErrInvalidTransition) true for the rich error, so
// the HTTP mapper matches on the sentinel and reads the detail off the concrete
// type.
func (e *TransitionError) Unwrap() error { return ErrInvalidTransition }

// Allowed lists the transitions that would have been legal from e.From.
func (e *TransitionError) Allowed() []Status { return AllowedFrom(e.From) }

// FieldError is one rejected field, identified by a path into the request body
// (`title`, `points[3].lat`) rather than by a bare name, so a client can point
// at the offending input without guessing.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError is every rejected field from one request, collected rather
// than reported one at a time: a form that gets a single error per round trip
// takes as many round trips as it has mistakes.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Message)
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

// ErrValidation is what errors.Is matches a *ValidationError against.
var ErrValidation = errors.New("validation error")

func (e *ValidationError) Unwrap() error { return ErrValidation }

// add appends a field error. Returns the receiver so builders can chain.
func (e *ValidationError) add(field, message string) {
	e.Fields = append(e.Fields, FieldError{Field: field, Message: message})
}

// orNil collapses an empty collection into a nil error, so callers can write
// `if err := in.Validate(now); err != nil`.
func (e *ValidationError) orNil() error {
	if len(e.Fields) == 0 {
		return nil
	}
	return e
}
