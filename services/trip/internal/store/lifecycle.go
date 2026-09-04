package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/togethergo/trip/internal/domain"
)

// The scheduler's two claim queries.
//
// `FOR UPDATE SKIP LOCKED` is what makes running more than one replica safe
// without a leader election or an advisory lock: a second scheduler issuing the
// identical query at the identical instant does not block on this one's rows
// and does not fail on them — it skips them and takes the next batch. The pass
// is idempotent in the weaker sense too, because a trip whose status has
// already moved no longer matches the WHERE clause.
//
// The lock is held for the whole transaction, which is why the transition runs
// inside it rather than in a second one. Claiming ids, committing, and then
// transitioning them would open exactly the window SKIP LOCKED was chosen to
// close.
//
// `<=` rather than `<` on the time comparison: a trip whose start_at is exactly
// now has started.
const dueToStartSQL = `SELECT` + tripColumns + `
	FROM trips t
	WHERE t.status = 'recruiting' AND t.start_at <= $1
	ORDER BY t.start_at
	LIMIT $2
	FOR UPDATE SKIP LOCKED`

const dueToCompleteSQL = `SELECT` + tripColumns + `
	FROM trips t
	WHERE t.status = 'in_progress' AND t.end_at <= $1
	ORDER BY t.end_at
	LIMIT $2
	FOR UPDATE SKIP LOCKED`

// LifecycleTick advances every trip whose clock has run out, in two passes of
// at most `batch` trips each.
//
// The order of the passes matters and is the reason they are not one query. A
// trip seeded — or left by an outage — with both its start and its end in the
// past is caught up completely by a single tick: the first pass moves it
// recruiting -> in_progress, the second moves it in_progress -> completed. The
// other order would take two ticks, which for a sixty-second interval is
// invisible in production and maddening in a test.
//
// Each pass is its own transaction. Two short transactions rather than one long
// one because the passes share no invariant, and a batch of a hundred
// completions holding locks while a hundred starts are processed is a
// contention profile with nothing to recommend it.
//
// Errors are not swallowed: a failed pass rolls back, its trips stay claimable,
// and the next tick picks them up. The caller logs. Nothing here retries,
// because "the next tick" already is the retry.
func (s *Store) LifecycleTick(ctx context.Context, batch int) (domain.LifecycleResult, error) {
	if batch <= 0 {
		return domain.LifecycleResult{}, fmt.Errorf("lifecycle batch size must be positive, got %d", batch)
	}
	now := s.now()

	var result domain.LifecycleResult

	started, _, err := s.advanceDue(ctx, dueToStartSQL, domain.StatusInProgress, now, batch)
	result.Started = started
	if err != nil {
		return result, fmt.Errorf("start due trips: %w", err)
	}

	completed, emitted, err := s.advanceDue(ctx, dueToCompleteSQL, domain.StatusCompleted, now, batch)
	result.Completed = completed
	result.CompletedEvents = emitted
	if err != nil {
		return result, fmt.Errorf("complete due trips: %w", err)
	}

	return result, nil
}

// advanceDue claims one batch and walks every trip in it to `to`.
//
// The rows are read into a slice before anything is written. pgx runs one
// statement at a time on a connection, so issuing the UPDATE while the SELECT's
// rows are still open would fail — and the batch is bounded, so holding it in
// memory costs nothing.
func (s *Store) advanceDue(ctx context.Context, claimSQL string, to domain.Status, now time.Time, batch int) (int, int, error) {
	var advanced, emitted int

	err := s.inTx(ctx, func(tx pgx.Tx) error {
		claimed, err := claimDueTrips(ctx, tx, claimSQL, now, batch)
		if err != nil {
			return err
		}

		for _, trip := range claimed {
			// The same core the HTTP handlers reach through ChangeStatus: it
			// consults domain.Can, writes the status, and writes the
			// trip.status_changed outbox row — plus trip.completed on the edge
			// into `completed`. The scheduler has no private path to that
			// column and no private idea of which edges are legal.
			_, didEmit, err := changeStatus(ctx, tx, trip, to)
			if err != nil {
				return fmt.Errorf("advance trip %s to %s: %w", trip.ID, to, err)
			}
			advanced++
			if didEmit {
				emitted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return advanced, emitted, nil
}

func claimDueTrips(ctx context.Context, tx pgx.Tx, claimSQL string, now time.Time, batch int) ([]domain.Trip, error) {
	rows, err := tx.Query(ctx, claimSQL, now, batch)
	if err != nil {
		return nil, fmt.Errorf("claim due trips: %w", err)
	}
	defer rows.Close()

	claimed := make([]domain.Trip, 0, batch)
	for rows.Next() {
		var trip domain.Trip
		if err := rows.Scan(tripScanTargets(&trip)...); err != nil {
			return nil, fmt.Errorf("scan due trip: %w", err)
		}
		claimed = append(claimed, trip)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due trips: %w", err)
	}
	return claimed, nil
}
