package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/togethergo/trip/internal/events"
)

// insertOutbox records an event in the same transaction as the domain change
// that caused it.
//
// This is the transactional outbox and it is the only way this service ever
// emits anything (CLAUDE.md rule 4). Publishing inline from a handler would
// mean two writes that can disagree: a broker acknowledgement followed by a
// failed commit invents an event for a thing that did not happen, and a commit
// followed by a failed publish loses one for a thing that did. Writing the
// event as a row makes the domain change and the promise to publish it a single
// atomic fact, and internal/events.Relay turns the promise into a message.
//
// The row's id becomes the wire's event_id, which is what makes the whole chain
// idempotent: a batch republished after a relay crash carries the same id and
// consumers deduplicate on it.
func insertOutbox(ctx context.Context, tx pgx.Tx, aggregateID uuid.UUID, eventType string, payload any) error {
	// Caught here rather than at publish time. An event_type with no registered
	// version is a bug in this build, and the row would otherwise sit
	// unpublishable in the outbox while the relay logged it every half second.
	if !events.KnownType(eventType) {
		return fmt.Errorf("refusing to write outbox row: unknown event type %q", eventType)
	}

	// The payload only, never the whole envelope — the relay assembles that
	// around the row so that occurred_at is the row's created_at and the
	// version is the publishing build's.
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", eventType, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO outbox (id, aggregate_id, event_type, payload)
		VALUES ($1, $2, $3, $4)`,
		uuid.New(), aggregateID, eventType, body,
	); err != nil {
		return fmt.Errorf("insert %s outbox row: %w", eventType, err)
	}
	return nil
}
