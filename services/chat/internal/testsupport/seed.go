package testsupport

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// The projection helpers below write rooms and members directly, which is the
// one thing production code never does — every row in those two tables gets
// there through an event.
//
// That is the point. A store test about pagination should not have to publish
// four events and wait for a consumer to catch up; the tests that care about
// the *events* drive the consumer end to end, and the rest start from the state
// those events would have produced. When the two disagree it is the consumer
// tests that are right.

// SeedRoom inserts a room in the given status.
func SeedRoom(t *testing.T, pool *pgxpool.Pool, tripID uuid.UUID, title, status string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO rooms (trip_id, title, status) VALUES ($1, $2, $3)
		 ON CONFLICT (trip_id) DO UPDATE SET title = EXCLUDED.title, status = EXCLUDED.status`,
		tripID, title, status)
	require.NoError(t, err)
}

// SeedMember adds an active member.
func SeedMember(t *testing.T, pool *pgxpool.Pool, tripID, userID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO room_members (trip_id, user_id) VALUES ($1, $2)
		 ON CONFLICT (trip_id, user_id) DO UPDATE SET left_at = NULL`,
		tripID, userID)
	require.NoError(t, err)
}

// SeedMessage inserts a message and returns its id, for tests that need history
// without going through a socket.
func SeedMessage(t *testing.T, pool *pgxpool.Pool, tripID, senderID uuid.UUID, body string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, pool.QueryRow(context.Background(),
		`INSERT INTO messages (trip_id, sender_id, body) VALUES ($1, $2, $3) RETURNING id`,
		tripID, senderID, body).Scan(&id))
	return id
}

// MemberCount counts the active members of a room. Used by the idempotency
// tests, where the whole assertion is "one row, not two".
func MemberCount(t *testing.T, pool *pgxpool.Pool, tripID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM room_members WHERE trip_id = $1 AND left_at IS NULL`,
		tripID).Scan(&count))
	return count
}
