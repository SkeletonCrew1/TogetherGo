// Package testsupport brings up a throwaway PostGIS database for tests.
//
// It is a non-test package so that both internal/store and internal/http can
// import it; Go will not let one package import another's _test files.
//
// The container is a real postgis/postgis:16-3.4 — the same image Compose runs
// — because this service's SQL is not portable and there would be no point
// testing it against something else. ST_DWithin, geography(Point,4326) and the
// GIST indexes either work against PostGIS or they are not being tested.
package testsupport

import (
	"context"
	"database/sql"
	"fmt"
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

	"github.com/togethergo/trip/migrations"
)

// The image and role names mirror deploy/postgres/init/01-databases.sql, so a
// test failing on a privilege or an extension would fail the same way in
// Compose.
const (
	image    = "postgis/postgis:16-3.4"
	database = "trip_db"
	user     = "trip_user"
	password = "trip_pass"
)

// sharedPool is built once per test binary. Starting a container per test would
// dominate the runtime; each test gets a clean slate from Truncate instead.
var shared struct {
	dsn string
	err error
}

// Pool returns a connection pool to a migrated, empty trip_db.
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
// recreating the database, and TRUNCATE ... CASCADE also resets the tables the
// foreign keys point at.
func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE trips, trip_points, participants, join_requests, trip_invites, outbox, processed_events, user_ref CASCADE`)
	require.NoError(t, err)
}

func ensureContainer(t *testing.T) string {
	t.Helper()
	containerOnce(t)
	if shared.err != nil {
		t.Fatalf("start postgis container: %v", shared.err)
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
	if dsn := os.Getenv("TRIP_TEST_DATABASE_URL"); dsn != "" {
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

	// PostGIS is installed by the superuser in Compose too; the container's
	// bootstrap role is a superuser, so this is the same shape.
	if err := createExtension(dsn); err != nil {
		shared.err = err
		return
	}

	shared.dsn = dsn
	shared.err = migrate(dsn)
}

func createExtension(dsn string) error {
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(context.Background(), `CREATE EXTENSION IF NOT EXISTS postgis`); err != nil {
		return fmt.Errorf("create postgis extension: %w", err)
	}
	return nil
}

// migrate applies the service's goose migrations to a fresh database.
func migrate(dsn string) error {
	db, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open database for migrations: %w", err)
	}
	defer func() { _ = db.Close() }()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
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
