// Package events is this service's half of the bus contract: the envelope it
// wraps outbox rows in, the payloads it writes into them, and the relay that
// carries them to RabbitMQ.
//
// The envelope is duplicated here rather than shared with the Python services.
// That is CLAUDE.md rule 7 and it is deliberate: thirty lines of struct
// definition repeated in two languages costs a great deal less than a package
// that couples the deploy cycle of identity to the deploy cycle of trip. The
// thing both halves answer to is contracts/events.md, not each other.
package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Exchange is the one exchange every event in the system is published to.
// Routing happens on the routing key, which is always the event type verbatim.
const Exchange = "togethergo.events"

// The event types this service publishes. Each is also its routing key.
const (
	// TypeTripCreated is emitted on the draft -> recruiting edge, not on the
	// INSERT. contracts/events.md calls it "a trip has been published", and a
	// draft is not published: it is visible to nobody but its organizer, and
	// provisioning a chat room for one would create rooms for trips that are
	// never announced.
	TypeTripCreated = "trip.created"

	// TypeTripCancelled is emitted on every edge into `cancelled`, alongside
	// the trip.status_changed for the same edge. Both exist because their
	// consumers differ: this one carries the title and the affected users, and
	// is what closes the chat room and tells everybody involved.
	TypeTripCancelled = "trip.cancelled"

	TypeJoinRequestCreated  = "join_request.created"
	TypeJoinRequestApproved = "join_request.approved"
	TypeJoinRequestRejected = "join_request.rejected"
	TypeParticipantRemoved  = "participant.removed"
	TypeTripInviteSent      = "trip.invite_sent"
	TypeTripStatusChanged   = "trip.status_changed"
	TypeTripCompleted       = "trip.completed"
)

// The event types this service *consumes*. They are not in `versions` and can
// never be published from here: identity owns them, and a routing key this
// service both publishes and consumes would be a loop waiting to happen.
const (
	TypeUserProfileUpdated = "user.profile_updated"
	TypeUserRatingUpdated  = "user.rating_updated"
)

// versions is the `event_version` stamped on each type.
//
// Held in code and not in the outbox row, because a version is a property of
// the publishing build rather than of the fact that was recorded: bumping one
// is a deploy, not a data migration of rows already written. A type missing
// from this map cannot be published at all — see Build — which is what stops a
// typo'd event_type from reaching the broker as a message no consumer is bound
// to and nobody notices.
var versions = map[string]int{
	TypeTripCreated:         1,
	TypeTripCancelled:       1,
	TypeJoinRequestCreated:  1,
	TypeJoinRequestApproved: 1,
	TypeJoinRequestRejected: 1,
	TypeParticipantRemoved:  1,
	TypeTripInviteSent:      1,
	TypeTripStatusChanged:   1,
	TypeTripCompleted:       1,
}

// KnownType reports whether eventType is one this build knows how to publish.
func KnownType(eventType string) bool {
	_, known := versions[eventType]
	return known
}

// Envelope is the exact five-field wrapper contracts/events.md fixes, plus the
// payload. Field order here is the field order on the wire, which matters to
// nothing but is one less difference to explain when reading a message body out
// of the management UI.
type Envelope struct {
	EventID      uuid.UUID       `json:"event_id"`
	EventType    string          `json:"event_type"`
	EventVersion int             `json:"event_version"`
	OccurredAt   string          `json:"occurred_at"`
	AggregateID  uuid.UUID       `json:"aggregate_id"`
	Payload      json.RawMessage `json:"payload"`
}

// Build wraps a stored payload in its envelope.
//
// occurredAt is the outbox row's created_at, never time.Now(): the domain fact
// happened when the transaction wrote the row, not when the relay got round to
// it. eventID is the outbox row's id for the same reason — a batch republished
// after a relay crash carries the same event_id, and that identity is the whole
// basis of consumer-side idempotency.
func Build(eventID uuid.UUID, eventType string, aggregateID uuid.UUID, occurredAt time.Time, payload json.RawMessage) (Envelope, error) {
	version, known := versions[eventType]
	if !known {
		return Envelope{}, fmt.Errorf("no event_version registered for %q", eventType)
	}
	return Envelope{
		EventID:      eventID,
		EventType:    eventType,
		EventVersion: version,
		OccurredAt:   RFC3339(occurredAt),
		AggregateID:  aggregateID,
		Payload:      payload,
	}, nil
}

// RFC3339 renders a timestamp the way every field on the bus spells one: UTC,
// second precision, `Z` rather than `+00:00`.
//
// Second precision on purpose. These are human-scale facts — an approval, an
// invitation — and the microseconds Postgres keeps would be noise in a payload
// that a person reads out of a DLQ. The outbox row keeps the full precision;
// only the rendering rounds.
func RFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
