// Command migrate applies or rolls back the chat service's schema.
//
// A separate binary, run as a separate step — never from the server's startup
// path (CLAUDE.md). It carries the migrations embedded, so the runtime image
// needs no copy of the .sql files and the command works identically from a
// container, from the repository root and from a test.
//
// Usage:
//
//	migrate up            apply every pending migration
//	migrate up-by-one     apply the next pending migration
//	migrate down          roll back the most recent migration
//	migrate reset         roll back everything
//	migrate status        list migrations and when they were applied
//	migrate version       print the current schema version
//
// The DSN comes from CHAT_DATABASE_URL, the same variable the server reads.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/togethergo/chat/migrations"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "migrate: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: migrate <up|up-by-one|down|reset|status|version>")
	}
	command, args := os.Args[1], os.Args[2:]

	dsn := os.Getenv("CHAT_DATABASE_URL")
	if dsn == "" {
		return errors.New("CHAT_DATABASE_URL is required")
	}

	// goose speaks database/sql; pgx's stdlib driver is the same connection
	// code the service itself uses, just behind that interface.
	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		_ = db.Close()
	}()

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := goose.RunContext(ctx, command, db, ".", args...); err != nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}

// Keeps the pgx stdlib driver registered under the name goose opens it with.
var _ = stdlib.GetDefaultDriver
