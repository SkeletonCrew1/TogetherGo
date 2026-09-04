// Package store is the trip service's data access layer: hand-written SQL over
// pgx, returning domain types. No ORM.
//
// It is also where the write paths run, because the checks that guard a write
// have to happen under the same lock as the write itself. A handler that read a
// trip, decided the caller was its organizer and then issued an UPDATE would be
// deciding on a snapshot; every mutating method here does `SELECT ... FOR
// UPDATE` and its checks inside one transaction instead.
//
// The rules themselves are not defined here. Whether a transition is legal,
// whether a title is long enough, who may see a draft — all of that lives in
// internal/domain and is called from here. This package decides *when* to ask,
// never *what* the answer is.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns the connection pool.
type Store struct {
	pool *pgxpool.Pool

	// now is the clock validation is checked against — "start_at must be in the
	// future" needs one. Injectable so tests do not have to sleep.
	now func() time.Time
}

// New builds a store over an existing pool. The pool's lifetime is the
// caller's business; the store never closes it.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, now: time.Now}
}

// WithClock replaces the clock used for validation. Tests only.
func (s *Store) WithClock(now func() time.Time) *Store {
	clone := *s
	clone.now = now
	return &clone
}

// Ping is the readiness check's database probe.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// querier is the subset of pgx that both the pool and a transaction satisfy, so
// read helpers can be shared between "read on its own" and "read inside the
// transaction that is about to write".
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// inTx runs fn in a transaction, committing on success and rolling back on any
// error or panic.
//
// The deferred Rollback after a successful Commit is not a mistake: pgx makes
// rolling back an already-committed transaction a no-op returning
// pgx.ErrTxClosed, and having it there unconditionally is what makes an early
// return from fn — or a panic in it — safe.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// checkViolation reports the constraint name when err is a Postgres CHECK
// violation.
//
// These are backstops, not the primary guard: every one of them is also checked
// in Go before the write. Recognising them here means that if a check ever does
// fire the log says which invariant was broken, instead of "SQLSTATE 23514".
func checkViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23514" {
		return pgErr.ConstraintName, true
	}
	return "", false
}
