package events

import "github.com/google/uuid"

// The payloads this service consumes, one struct per event type. Field names
// and nullability are fixed by contracts/events.md.
//
// Each struct is the *subset this service acts on*, not the whole payload.
// `trip.created` carries a category, a start date and a capacity; a chat room
// has no use for any of them, and decoding a field into a struct member nothing
// reads is how a service ends up looking like it depends on data it does not.
// Unknown JSON keys are ignored by encoding/json, which is what makes this
// narrowing safe as the catalogue grows.

// TripCreated provisions the room.
type TripCreated struct {
	TripID uuid.UUID `json:"trip_id"`
	// OrganizerID becomes the room's first member. They can post before anyone
	// has joined, which is the reason the room is created up front at all
	// rather than on the first approval.
	OrganizerID uuid.UUID `json:"organizer_id"`
	Title       string    `json:"title"`
}

// TripCancelled closes the room. History is untouched.
type TripCancelled struct {
	TripID uuid.UUID `json:"trip_id"`
	// Title, for the same reason JoinRequestApproved carries it: this event can
	// be the first one this service sees for a trip, and a room has to have a
	// name even when it was born closed.
	Title string `json:"title"`
}

// JoinRequestApproved grants room access. This is the only event that ever
// does: an unapproved user cannot open the socket, because there is no other
// writer of room_members.
type JoinRequestApproved struct {
	TripID uuid.UUID `json:"trip_id"`
	// ParticipantID is the newly approved user.
	ParticipantID uuid.UUID `json:"participant_id"`
	// Title is carried so the handler can provision a room the projection has
	// not seen a `trip.created` for yet — see store.ApplyJoinApproved.
	Title string `json:"title"`
}

// ParticipantRemoved revokes room access, whether the participant left or was
// removed by the organizer. The distinction does not reach this service:
// either way the answer is "no longer a member from now on, and everything
// they wrote stays".
type ParticipantRemoved struct {
	TripID        uuid.UUID `json:"trip_id"`
	ParticipantID uuid.UUID `json:"participant_id"`
}
