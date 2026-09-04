// Package events is this service's half of the bus contract: the envelope it
// unwraps, the payloads it consumes, and the consumer that reads
// `chat.trip-events`.
//
// The envelope is duplicated here rather than shared with the trip service.
// That is CLAUDE.md rule 7 and it is deliberate: thirty lines of struct
// definition repeated in two services costs a great deal less than a package
// that couples their deploy cycles. The thing both halves answer to is
// contracts/events.md, not each other.
//
// This service publishes nothing. It has no outbox and no relay, because it
// produces no domain facts anybody else acts on — a message in a room is read
// over this service's own API and over its own socket, by people who are
// already in the room.
package events

import (
	"encoding/json"

	"github.com/google/uuid"
)

// Queue is the chat service's inbox. One queue for the whole service rather
// than one per event type: contracts/events.md fixes that shape, and the
// dispatch on event_type happens in the consumer rather than in the broker.
const Queue = "chat.trip-events"

// The event types this service consumes, which are exactly the four routing
// keys bound to that queue in deploy/rabbitmq/definitions.json. Anything else
// arriving on it is logged, marked processed and acked — see Consumer.apply.
const (
	TypeTripCreated         = "trip.created"
	TypeTripCancelled       = "trip.cancelled"
	TypeJoinRequestApproved = "join_request.approved"
	TypeParticipantRemoved  = "participant.removed"
)

// Envelope is the exact five-field wrapper contracts/events.md fixes, plus the
// payload.
type Envelope struct {
	EventID      uuid.UUID       `json:"event_id"`
	EventType    string          `json:"event_type"`
	EventVersion int             `json:"event_version"`
	OccurredAt   string          `json:"occurred_at"`
	AggregateID  uuid.UUID       `json:"aggregate_id"`
	Payload      json.RawMessage `json:"payload"`
}
