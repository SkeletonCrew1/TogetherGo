// Package store is the chat service's data access layer: hand-written SQL over
// pgx, returning domain types. No ORM.
//
// It is also where the guards live, because a check that protects a write has
// to happen under the same lock as the write itself. The clearest example is
// InsertMessage: "is the caller still a member, and is the room still open"
// is not a query the handler runs and then acts on — it is a WHERE EXISTS on
// the INSERT, so there is no window between deciding and doing.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns the connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New builds a store over an existing pool. The pool's lifetime is the
// caller's business; the store never closes it.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ping is the readiness check's database probe.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
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
