package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/events"
)

// The user projection: the store half of the consumer, and the local-first read
// the discovery and participation pages go through.

func profileEvent(userID uuid.UUID, name string, at time.Time) events.UserProfileUpdated {
	return events.UserProfileUpdated{
		UserID:      userID,
		DisplayName: name,
		AvatarURL:   ptr("https://cdn.example.test/" + userID.String() + ".jpg"),
		Bio:         ptr("Weekend hiker."),
		UpdatedAt:   at,
	}
}

func ratingEvent(userID uuid.UUID, average float64, count int, at time.Time) events.UserRatingUpdated {
	return events.UserRatingUpdated{
		UserID:        userID,
		RatingAverage: average,
		RatingCount:   count,
		UpdatedAt:     at,
	}
}

func processedCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM processed_events`).Scan(&count))
	return count
}

// TestTheSameProfileEventTwiceUpdatesTheProjectionOnce is the third acceptance
// criterion.
//
// The second delivery is reported as not-first and writes nothing. It is the
// same event_id, which is what at-least-once delivery means in practice: the
// relay republishes after a crash between publish and mark, and the broker
// redelivers after a crash between commit and ack.
func TestTheSameProfileEventTwiceUpdatesTheProjectionOnce(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	userID := uuid.New()
	eventID := uuid.New()

	first, err := s.ApplyProfileUpdate(ctx, eventID, profileEvent(userID, "Alex", now))
	require.NoError(t, err)
	require.True(t, first)

	// The same event again, but carrying a different name — a payload the
	// projection must *not* apply, which is how this test can tell "processed
	// once" apart from "applied twice with the same value".
	second, err := s.ApplyProfileUpdate(ctx, eventID, profileEvent(userID, "Someone Else", now.Add(time.Hour)))
	require.NoError(t, err)
	require.False(t, second, "a redelivered event is not applied again")

	refs, err := s.UserRefs(ctx, []uuid.UUID{userID})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "Alex", refs[userID].FullName)

	require.Equal(t, 1, processedCount(t, pool), "one event, one ledger row")
}

// TestTheSameRatingEventTwiceUpdatesTheProjectionOnce: the same guarantee on
// the other event type, since each has its own upsert.
func TestTheSameRatingEventTwiceUpdatesTheProjectionOnce(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	userID := uuid.New()
	eventID := uuid.New()
	_, err := s.ApplyProfileUpdate(ctx, uuid.New(), profileEvent(userID, "Alex", now))
	require.NoError(t, err)

	first, err := s.ApplyRatingUpdate(ctx, eventID, ratingEvent(userID, 4.75, 12, now))
	require.NoError(t, err)
	require.True(t, first)

	second, err := s.ApplyRatingUpdate(ctx, eventID, ratingEvent(userID, 1.00, 999, now.Add(time.Hour)))
	require.NoError(t, err)
	require.False(t, second)

	refs, err := s.UserRefs(ctx, []uuid.UUID{userID})
	require.NoError(t, err)
	require.InDelta(t, 4.75, *refs[userID].RatingAvg, 0.001)
	require.Equal(t, 12, refs[userID].RatingCount)
}

// TestAnOlderProfileUpdateIsDiscardedButMarkedProcessed is the out-of-order
// rule from contracts/events.md. There is no ordering guarantee across events,
// so a consumer that applied whatever arrived last would let a stale update win
// a race.
func TestAnOlderProfileUpdateIsDiscardedButMarkedProcessed(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	userID := uuid.New()

	_, err := s.ApplyProfileUpdate(ctx, uuid.New(), profileEvent(userID, "Alex K.", now))
	require.NoError(t, err)

	stale, err := s.ApplyProfileUpdate(ctx, uuid.New(), profileEvent(userID, "Alex", now.Add(-time.Hour)))
	require.NoError(t, err)
	require.True(t, stale, "a distinct event_id is still a first delivery")

	refs, err := s.UserRefs(ctx, []uuid.UUID{userID})
	require.NoError(t, err)
	require.Equal(t, "Alex K.", refs[userID].FullName, "the newer name survives")

	// Discarded is not the same as unhandled: a redelivery of the stale event
	// must not be reconsidered.
	require.Equal(t, 2, processedCount(t, pool))
}

// TestProfileAndRatingWatermarksAreIndependent is why user_ref carries two of
// them rather than the one the table would otherwise need.
//
// A rating recorded after a profile edit but delivered before it must not make
// the profile edit look stale. With a single watermark it would, and the name
// would stay wrong until the user edited it again.
func TestProfileAndRatingWatermarksAreIndependent(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	userID := uuid.New()

	// A rating from 12:05 arrives first.
	_, err := s.ApplyRatingUpdate(ctx, uuid.New(), ratingEvent(userID, 4.5, 8, now.Add(5*time.Minute)))
	require.NoError(t, err)

	// A profile edit made at 12:03 — older than the rating, newer than any
	// profile event — arrives second.
	_, err = s.ApplyProfileUpdate(ctx, uuid.New(), profileEvent(userID, "Alex K.", now.Add(3*time.Minute)))
	require.NoError(t, err)

	refs, err := s.UserRefs(ctx, []uuid.UUID{userID})
	require.NoError(t, err)
	require.Equal(t, "Alex K.", refs[userID].FullName)
	require.InDelta(t, 4.5, *refs[userID].RatingAvg, 0.001)
}

// TestARatingOnlyRowIsNotAProjectionHit. A rating for a user no profile event
// has covered leaves a row with no name, and a row with no name is nothing to
// render. It is excluded from UserRefs so the id falls through to identity and
// comes back filled in — better than dropping the rating, and better than
// showing a blank name forever.
func TestARatingOnlyRowIsNotAProjectionHit(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	userID := uuid.New()

	_, err := s.ApplyRatingUpdate(ctx, uuid.New(), ratingEvent(userID, 4.0, 3, now))
	require.NoError(t, err)

	refs, err := s.UserRefs(ctx, []uuid.UUID{userID})
	require.NoError(t, err)
	require.Empty(t, refs, "no name means no hit")

	// The rating was still stored, and the row is still there for the profile
	// event that will eventually arrive.
	var rows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM user_ref WHERE user_id = $1`, userID).Scan(&rows))
	require.Equal(t, 1, rows)

	_, err = s.ApplyProfileUpdate(ctx, uuid.New(), profileEvent(userID, "Alex", now))
	require.NoError(t, err)

	refs, err = s.UserRefs(ctx, []uuid.UUID{userID})
	require.NoError(t, err)
	require.Equal(t, "Alex", refs[userID].FullName)
	require.Equal(t, 3, refs[userID].RatingCount, "the rating that arrived first is still there")
}

// TestBackfillFillsGapsWithoutOverwritingEvents. Identity's REST snapshot and
// its event stream are the same data from the same owner, but only the stream
// carries a timestamp this service can order against — so the stream wins by
// default and the snapshot fills what it has not covered.
func TestBackfillFillsGapsWithoutOverwritingEvents(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	fromEvent := uuid.New()
	coldStart := uuid.New()

	_, err := s.ApplyProfileUpdate(ctx, uuid.New(), profileEvent(fromEvent, "Alex K.", now))
	require.NoError(t, err)

	require.NoError(t, s.BackfillUserRefs(ctx, []domain.UserRef{
		// This one the projection already knows about, from an event. The
		// backfill must leave its name alone.
		{UserID: fromEvent, FullName: "Stale Snapshot", RatingAvg: ptr(4.25), RatingCount: 4},
		// This one it has never heard of.
		{UserID: coldStart, FullName: "Bohdan", PhotoURL: ptr("https://cdn.example.test/b.jpg"), RatingAvg: ptr(4.9), RatingCount: 20},
	}))

	refs, err := s.UserRefs(ctx, []uuid.UUID{fromEvent, coldStart})
	require.NoError(t, err)
	require.Len(t, refs, 2)

	require.Equal(t, "Alex K.", refs[fromEvent].FullName, "an event's value is authoritative")
	// The rating, though, no event has covered — so the snapshot fills it.
	require.InDelta(t, 4.25, *refs[fromEvent].RatingAvg, 0.001)

	require.Equal(t, "Bohdan", refs[coldStart].FullName)
	require.Equal(t, 20, refs[coldStart].RatingCount)

	// And an event arriving after the backfill still wins, because the
	// backfill left the watermark NULL.
	_, err = s.ApplyProfileUpdate(ctx, uuid.New(), profileEvent(coldStart, "Bohdan M.", now.Add(-24*time.Hour)))
	require.NoError(t, err)
	refs, err = s.UserRefs(ctx, []uuid.UUID{coldStart})
	require.NoError(t, err)
	require.Equal(t, "Bohdan M.", refs[coldStart].FullName)
}

// TestUserRefsMissesAreAbsentNotErrors. The caller's next move is to ask
// identity about whatever is missing, and it can only do that if a miss is a
// missing key rather than a failure.
func TestUserRefsMissesAreAbsentNotErrors(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	known := uuid.New()
	_, err := s.ApplyProfileUpdate(ctx, uuid.New(), profileEvent(known, "Alex", now))
	require.NoError(t, err)

	refs, err := s.UserRefs(ctx, []uuid.UUID{known, uuid.New(), uuid.New()})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Contains(t, refs, known)

	empty, err := s.UserRefs(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// TestMarkProcessedRecordsAnUnknownEvent. contracts/events.md says an event
// type a build does not recognise is logged, marked processed and acked — not
// dead-lettered. Marking it is what stops a redelivery re-logging it forever.
func TestMarkProcessedRecordsAnUnknownEvent(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	eventID := uuid.New()

	first, err := s.MarkProcessed(ctx, eventID)
	require.NoError(t, err)
	require.True(t, first)

	second, err := s.MarkProcessed(ctx, eventID)
	require.NoError(t, err)
	require.False(t, second)
}
