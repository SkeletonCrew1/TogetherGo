package events

import (
	"time"

	"github.com/google/uuid"
)

// The payload of every event this service publishes, one struct per type.
//
// Field names and nullability are fixed by contracts/events.md. `omitempty`
// appears nowhere: a consumer reading `message` expects the key to be there and
// `null` when there is no message, and a field that vanishes when it is empty
// is a field every consumer has to guard.
//
// These are the payload only. The relay assembles the envelope around whatever
// the outbox row holds; nothing here knows about event_id or occurred_at.

// Reason codes. Closed vocabularies from contracts/events.md, not free text.
//
// The organizer's own words on a rejection are deliberately not among them:
// they stay in join_requests.decision_reason and are served to the requester
// through this service's API. What goes on the bus is the class of decision,
// which is all a notification template can act on anyway.
const (
	RejectedByOrganizer = "declined_by_organizer"
	RejectedTripFull    = "trip_full"
	RejectedExpired     = "expired"

	RemovedByOrganizer = "removed_by_organizer"
	RemovedVoluntarily = "left_voluntarily"

	// CancelledByOrganizer is the only cancellation reason this build emits.
	// The catalogue also allows `insufficient_participants` and `expired`, and
	// neither has a code path: nothing in this service cancels a trip on its
	// own — the scheduler starts and completes them, and an organizer is the
	// only actor who calls one off.
	CancelledByOrganizer = "organizer_cancelled"
)

// TripCreated announces a published trip. Chat provisions the group
// conversation from it, up front, so the organizer has somewhere to post before
// anybody has joined.
//
// The route geometry is deliberately absent. It is large, only this service
// needs it, and it is available over REST — contracts/events.md is explicit
// about that, and it is the reason this payload is eight scalars rather than a
// trip.
type TripCreated struct {
	TripID      uuid.UUID `json:"trip_id"`
	OrganizerID uuid.UUID `json:"organizer_id"`
	Title       string    `json:"title"`
	Category    string    `json:"category"`
	StartsAt    string    `json:"starts_at"`
	EndsAt      string    `json:"ends_at"`
	// MaxParticipants is the trip's capacity, organizer included.
	MaxParticipants int `json:"max_participants"`
	// Status is always the literal "open" on this event, which is what the
	// catalogue fixes. It is not this service's `recruiting`: the two describe
	// the same state, and the event's vocabulary is the contract's rather than
	// the publisher's.
	Status string `json:"status"`
}

// TripCancelled tells everyone the trip is off. Chat closes the room to new
// messages and notification writes to the people affected.
type TripCancelled struct {
	TripID      uuid.UUID `json:"trip_id"`
	Title       string    `json:"title"`
	OrganizerID uuid.UUID `json:"organizer_id"`
	Reason      string    `json:"reason"`
	// Comment is the organizer's free text. Always null in this build: the
	// cancel endpoint takes no body, and inventing a sentence would be worse
	// than saying there was none.
	Comment     *string `json:"comment"`
	CancelledAt string  `json:"cancelled_at"`
	// AffectedUserIDs is everyone with a stake in the trip except its
	// organizer: the approved participants, and the people whose join request
	// was still open when it was called off. The second group matters — they
	// are waiting for an answer they are now never going to get.
	AffectedUserIDs []uuid.UUID `json:"affected_user_ids"`
}

// JoinRequestCreated tells the organizer somebody has asked to come.
type JoinRequestCreated struct {
	JoinRequestID uuid.UUID `json:"join_request_id"`
	TripID        uuid.UUID `json:"trip_id"`
	Title         string    `json:"title"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	RequesterID   uuid.UUID `json:"requester_id"`
	Message       *string   `json:"message"`
	RequestedAt   string    `json:"requested_at"`
}

// JoinRequestApproved is the event that grants chat access.
//
// It is the fattest of the five on purpose. Chat has to create a room
// membership and notification has to write "you are in, here is the trip", and
// neither may call back into this service to do it: a callback would make the
// side effect depend on trip being up at that moment, which is precisely what
// putting the fact on a durable bus was meant to avoid. So the payload carries
// the trip's id and title, the participant, the organizer, and the counts both
// consumers would otherwise have to ask for.
type JoinRequestApproved struct {
	JoinRequestID uuid.UUID `json:"join_request_id"`
	TripID        uuid.UUID `json:"trip_id"`
	Title         string    `json:"title"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	// ParticipantID is the newly approved user — the `user_id` of the request.
	ParticipantID    uuid.UUID `json:"participant_id"`
	ApprovedAt       string    `json:"approved_at"`
	ParticipantCount int       `json:"participant_count"`
	MaxParticipants  int       `json:"max_participants"`
}

// JoinRequestRejected tells the requester no. Chat does not consume it: no
// access was granted, so there is none to revoke.
type JoinRequestRejected struct {
	JoinRequestID uuid.UUID `json:"join_request_id"`
	TripID        uuid.UUID `json:"trip_id"`
	Title         string    `json:"title"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	RequesterID   uuid.UUID `json:"requester_id"`
	Reason        string    `json:"reason"`
	RejectedAt    string    `json:"rejected_at"`
}

// ParticipantRemoved revokes chat access, whether the participant left or was
// removed. `removed_by` is what distinguishes the two, together with `reason`.
type ParticipantRemoved struct {
	TripID           uuid.UUID `json:"trip_id"`
	ParticipantID    uuid.UUID `json:"participant_id"`
	RemovedBy        uuid.UUID `json:"removed_by"`
	Reason           string    `json:"reason"`
	RemovedAt        string    `json:"removed_at"`
	ParticipantCount int       `json:"participant_count"`
}

// TripInviteSent asks notification to mail the invitee. The email address is
// not here: notification resolves it from its own user projection, because an
// address on the bus is an address in every DLQ and every log that ever holds
// this message.
type TripInviteSent struct {
	TripID    uuid.UUID `json:"trip_id"`
	Title     string    `json:"title"`
	InviteID  uuid.UUID `json:"invite_id"`
	InviterID uuid.UUID `json:"inviter_id"`
	InviteeID uuid.UUID `json:"invitee_id"`
	ExpiresAt string    `json:"expires_at"`
	SentAt    string    `json:"sent_at"`
}

// TripStatusChanged records a move along the lifecycle.
//
// Written by Store.ChangeStatus, which is the only writer of trips.status, so
// there is no transition — organizer-driven or scheduled — that does not
// produce one of these.
//
// A note on the contract. contracts/events.md says `completed` and `cancelled`
// never appear in `new_status`, on the grounds that the terminal transitions
// have their own events. This build emits a status_changed for *every* edge,
// terminal ones included, because the guarantee that is actually worth having
// is "one row per transition, from the one function that performs them" — a
// rule with an exception in it is a rule somebody will get wrong. Consumers
// that only care about the non-terminal edges filter on new_status;
// `trip.completed` is still emitted alongside, carrying the roster, and remains
// the event the rating window is opened from.
type TripStatusChanged struct {
	TripID      uuid.UUID `json:"trip_id"`
	OrganizerID uuid.UUID `json:"organizer_id"`
	OldStatus   string    `json:"old_status"`
	NewStatus   string    `json:"new_status"`
	ChangedAt   string    `json:"changed_at"`
}

// TripCompletedParticipant is one traveller on a finished trip's roster.
type TripCompletedParticipant struct {
	UserID   uuid.UUID `json:"user_id"`
	Role     string    `json:"role"`
	JoinedAt string    `json:"joined_at"`
}

// TripCompleted is the fat one, and the only event in this service whose
// payload carries a list.
//
// It opens the rating window: identity has to decide who may rate whom, and
// notification has to write "rate your co-travellers", and neither may call
// back into this service for the roster. A callback would make the rating
// window depend on trip being up at that moment, which is what putting the
// fact on a durable bus was meant to avoid.
//
// Emitted **exactly once per trip**, guarded by trips.completed_event_emitted
// rather than by the status check alone — see store.emitTripCompleted. A trip
// that finishes with only its organizer on the roster emits none at all: there
// is nobody to rate, and an empty rating window is a mail nobody can act on.
type TripCompleted struct {
	TripID      uuid.UUID `json:"trip_id"`
	Title       string    `json:"title"`
	OrganizerID uuid.UUID `json:"organizer_id"`
	StartedAt   string    `json:"started_at"`
	CompletedAt string    `json:"completed_at"`
	// RatingWindowClosesAt is computed here rather than in identity so that
	// every consumer of this message agrees on the deadline without having to
	// share a constant across two languages.
	RatingWindowClosesAt string                     `json:"rating_window_closes_at"`
	Participants         []TripCompletedParticipant `json:"participants"`
}

// The inbound half. These two are identity's payloads, consumed from
// trip.user-events and projected into user_ref; this service never publishes
// them. They are decoded rather than assembled, so the timestamps are
// time.Time — the projection compares them against the stored watermark, and a
// string comparison would work only by accident of RFC 3339's ordering.

// UserProfileUpdated is identity's whole current public profile, not a delta.
type UserProfileUpdated struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	AvatarURL   *string   `json:"avatar_url"`
	Bio         *string   `json:"bio"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// UserRatingUpdated is a user's aggregate rating after a new co-traveller
// rating was recorded. `bio` has no counterpart here and neither does the name:
// the two events feed different columns of the same row.
type UserRatingUpdated struct {
	UserID        uuid.UUID `json:"user_id"`
	RatingAverage float64   `json:"rating_average"`
	RatingCount   int       `json:"rating_count"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Timestamp is RFC3339 under a name that reads correctly at the call sites in
// the store, where what is being converted is a domain time rather than "an
// envelope field".
func Timestamp(t time.Time) string { return RFC3339(t) }
