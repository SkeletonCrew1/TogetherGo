// Package testsupport brings up the throwaway backing services the chat tests
// need: a Postgres database, a Redis, a RabbitMQ, and a stand-in for identity.
//
// It is a non-test package so that every internal package can import it; Go
// will not let one package import another's _test files.
//
// The containers are the images Compose runs. That matters least for Postgres —
// this service's SQL is ordinary — and most for Redis, where GETDEL, the Lua
// rate limiter and pub/sub are the thing under test and a fake would only prove
// that the fake agrees with itself.
package testsupport

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/togethergo/chat/migrations"
)

// The image and role names mirror deploy/postgres/init/01-databases.sql, so a
// test failing on a privilege would fail the same way in Compose. The image is
// the PostGIS one even though nothing in this schema is geographic: it is the
// one Compose runs, and using a second base image would mean a second thing
// that can differ between the test and the deployment.
const (
	image    = "postgis/postgis:16-3.4"
	database = "chat_db"
	user     = "chat_user"
	password = "chat_pass"
)

// shared is built once per test binary. Starting a container per test would
// dominate the runtime; each test gets a clean slate from Truncate instead.
var shared struct {
	dsn string
	err error
}

// Pool returns a connection pool to a migrated, empty chat_db.
//
// The container is started on the first call and torn down when the test binary
// exits. The schema is built by running the service's own goose migrations, not
// by a hand-maintained fixture, so a migration that does not apply cleanly
// fails the store tests rather than being discovered in Compose.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := ensureContainer(t)

	pool, err := pgxpool.New(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	Truncate(t, pool)
	return pool
}

// Truncate empties every table, leaving the schema in place. Cheaper than
// recreating the database, and RESTART IDENTITY resets the messages sequence so
// that a test asserting on message ids does not depend on what ran before it.
func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE rooms, room_members, messages, processed_events RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
}

func ensureContainer(t *testing.T) string {
	t.Helper()
	containerOnce(t)
	if shared.err != nil {
		t.Fatalf("start postgres container: %v", shared.err)
	}
	return shared.dsn
}

var started bool

func containerOnce(t *testing.T) {
	t.Helper()
	if started {
		return
	}
	started = true

	// An escape hatch for running the suite against an already-running
	// database — `make up` leaves one on localhost:5432 — which turns a
	// twenty-second container start into nothing. The container is the default
	// so that CI needs no setup.
	if dsn := os.Getenv("CHAT_TEST_DATABASE_URL"); dsn != "" {
		shared.dsn = dsn
		shared.err = migrate(dsn)
		return
	}

	ctx := context.Background()
	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase(database),
		postgres.WithUsername(user),
		postgres.WithPassword(password),
		testcontainers.WithWaitStrategy(
			// Postgres in this image starts, runs the init scripts and
			// restarts, so the first "ready" line is not the one that counts.
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second),
		),
	)
	if err != nil {
		shared.err = err
		return
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		shared.err = err
		return
	}

	shared.dsn = dsn
	shared.err = migrate(dsn)
}

// migrate applies the service's goose migrations to a fresh database.
func migrate(dsn string) error {
	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	return goose.Up(db, ".")
}

var _ = stdlib.GetDefaultDriver

// DSN returns the connection string of the throwaway database, for tests that
// need to drive goose themselves rather than talk to an already-migrated one.
func DSN(t *testing.T) string {
	t.Helper()
	return ensureContainer(t)
}

// Goose opens a goose-ready handle on dsn with the embedded migrations already
// registered, so a test can drive `up` and `down` itself. The caller closes it.
func Goose(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	db, err := goose.OpenDBWithDriver("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	require.NoError(t, goose.SetDialect("postgres"))
	return db
}
