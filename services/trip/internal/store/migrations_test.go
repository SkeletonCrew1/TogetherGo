package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/testsupport"
)

// TestMigrationsAreReversible is the "goose up/down are both clean" acceptance
// criterion.
//
// It runs against the same throwaway database the other tests use and leaves it
// migrated up, so ordering between tests does not matter. `goose reset` rolls
// every migration back; if a Down block were missing a DROP, or dropped things
// in an order the foreign keys forbid, this fails here rather than the first
// time somebody tries to roll back a deployment.
func TestMigrationsAreReversible(t *testing.T) {
	dsn := testsupport.DSN(t)
	db := testsupport.Goose(t, dsn)
	ctx := context.Background()

	before, err := goose.GetDBVersion(db)
	require.NoError(t, err)
	require.Positive(t, before, "the suite starts from a migrated database")

	require.NoError(t, goose.ResetContext(ctx, db, "."), "goose down must be clean")

	version, err := goose.GetDBVersion(db)
	require.NoError(t, err)
	require.Zero(t, version)

	for _, table := range []string{"trips", "trip_points", "participants", "outbox"} {
		require.False(t, tableExists(t, db, table), "%s survived the rollback", table)
	}

	require.NoError(t, goose.UpContext(ctx, db, "."), "goose up must be clean")

	after, err := goose.GetDBVersion(db)
	require.NoError(t, err)
	require.Equal(t, before, after)

	for _, table := range []string{"trips", "trip_points", "participants", "outbox"} {
		require.True(t, tableExists(t, db, table), "%s was not recreated", table)
	}
}

// TestSchemaHasTheDeclaredIndexes: the GIST indexes are what make the radius
// filter a plain indexed predicate, which is the entire reason the departure and
// destination columns are denormalised. An index quietly dropped from a
// migration would leave the schema correct and the design pointless.
func TestSchemaHasTheDeclaredIndexes(t *testing.T) {
	pool := testsupport.Pool(t)

	for _, tc := range []struct {
		index  string
		method string
	}{
		{"trips_departure_gist", "gist"},
		{"trips_destination_gist", "gist"},
		{"trips_status_start_idx", "btree"},
		{"trips_organizer_status_idx", "btree"},
		{"outbox_unpublished_idx", "btree"},
	} {
		t.Run(tc.index, func(t *testing.T) {
			var method string
			err := pool.QueryRow(context.Background(), `
				SELECT am.amname
				FROM pg_class i
				JOIN pg_am am ON am.oid = i.relam
				WHERE i.relname = $1`, tc.index).Scan(&method)
			require.NoError(t, err, "index %s is missing", tc.index)
			require.Equal(t, tc.method, method)
		})
	}
}

// TestGeographyColumnsAreSRID4326 checks the thing a `geography(Point,4326)`
// declaration is for: a point written with a different SRID must be rejected by
// the column, not silently stored and silently mismeasured.
func TestGeographyColumnsAreSRID4326(t *testing.T) {
	pool := testsupport.Pool(t)

	rows, err := pool.Query(context.Background(), `
		SELECT f_table_name, f_geography_column, srid, type
		FROM geography_columns
		WHERE f_table_name IN ('trips', 'trip_points')
		ORDER BY f_table_name, f_geography_column`)
	require.NoError(t, err)
	defer rows.Close()

	found := map[string]bool{}
	for rows.Next() {
		var table, column, geomType string
		var srid int
		require.NoError(t, rows.Scan(&table, &column, &srid, &geomType))

		require.Equal(t, 4326, srid, "%s.%s is not SRID 4326", table, column)
		require.Equal(t, "Point", geomType)
		found[table+"."+column] = true
	}
	require.NoError(t, rows.Err())

	require.Equal(t, map[string]bool{
		"trips.departure_location":   true,
		"trips.destination_location": true,
		"trip_points.location":       true,
	}, found)
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		)`, name).Scan(&exists)
	require.NoError(t, err)
	return exists
}
