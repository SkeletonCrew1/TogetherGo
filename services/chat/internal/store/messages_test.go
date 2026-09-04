package store_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/domain"
	"github.com/togethergo/chat/internal/store"
	"github.com/togethergo/chat/internal/testsupport"
)

// The membership and openness checks live inside the INSERT, so these are the
// tests that prove there is no window between deciding and doing. A handler
// that read first and inserted second would pass every one of them except by
// accident of timing — which is why the check is in the statement.

func TestInsertMessageRefusesNonMembers(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, member, stranger := uuid.New(), uuid.New(), uuid.New()
	testsupport.SeedRoom(t, pool, tripID, "Members only", domain.StatusRecruiting)
	testsupport.SeedMember(t, pool, tripID, member)

	_, err := st.InsertMessage(ctx, tripID, stranger, "let me in")
	require.ErrorIs(t, err, domain.ErrNotMember)

	messages, _, err := st.ListMessages(ctx, tripID, 0, 10)
	require.NoError(t, err)
	require.Empty(t, messages, "a refused message must not be written")
}

func TestInsertMessageRefusesARemovedMember(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, member := uuid.New(), uuid.New()
	testsupport.SeedRoom(t, pool, tripID, "Removed", domain.StatusRecruiting)
	testsupport.SeedMember(t, pool, tripID, member)

	_, err := st.InsertMessage(ctx, tripID, member, "still here")
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `UPDATE room_members SET left_at = now() WHERE trip_id = $1 AND user_id = $2`, tripID, member)
	require.NoError(t, err)

	_, err = st.InsertMessage(ctx, tripID, member, "and now")
	require.ErrorIs(t, err, domain.ErrNotMember)
}

func TestInsertMessageRefusesACancelledRoom(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, member := uuid.New(), uuid.New()
	testsupport.SeedRoom(t, pool, tripID, "Called off", domain.StatusCancelled)
	testsupport.SeedMember(t, pool, tripID, member)

	_, err := st.InsertMessage(ctx, tripID, member, "one more thing")
	require.ErrorIs(t, err, domain.ErrRoomClosed)
}

func TestInsertMessageRefusesARoomThatDoesNotExist(t *testing.T) {
	st := store.New(testsupport.Pool(t))

	_, err := st.InsertMessage(context.Background(), uuid.New(), uuid.New(), "hello?")
	require.ErrorIs(t, err, domain.ErrRoomNotFound)
}

// History pages backwards, newest first, and the pages do not overlap or skip.
func TestListMessagesPagesBackwards(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, member := uuid.New(), uuid.New()
	testsupport.SeedRoom(t, pool, tripID, "History", domain.StatusRecruiting)
	testsupport.SeedMember(t, pool, tripID, member)

	for i := 1; i <= 7; i++ {
		testsupport.SeedMessage(t, pool, tripID, member, "message "+strconv.Itoa(i))
	}

	var seen []string
	beforeID := int64(0)
	for page := 0; page < 5; page++ {
		messages, hasMore, err := st.ListMessages(ctx, tripID, beforeID, 3)
		require.NoError(t, err)
		for _, message := range messages {
			seen = append(seen, message.Body)
		}
		if !hasMore {
			break
		}
		require.NotEmpty(t, messages)
		beforeID = messages[len(messages)-1].ID
	}

	require.Equal(t, []string{
		"message 7", "message 6", "message 5",
		"message 4", "message 3", "message 2",
		"message 1",
	}, seen)
}

// The room list is one query, and this is what it has to get right: the
// caller's rooms only, most recently active first, with the newest message and
// an unread count that ignores the caller's own words.
func TestListRoomsOrdersByActivity(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	member, other := uuid.New(), uuid.New()
	silent, chatty, foreign := uuid.New(), uuid.New(), uuid.New()

	testsupport.SeedRoom(t, pool, silent, "Silent", domain.StatusRecruiting)
	testsupport.SeedRoom(t, pool, chatty, "Chatty", domain.StatusRecruiting)
	testsupport.SeedRoom(t, pool, foreign, "Not yours", domain.StatusRecruiting)
	testsupport.SeedMember(t, pool, silent, member)
	testsupport.SeedMember(t, pool, chatty, member)
	testsupport.SeedMember(t, pool, chatty, other)
	testsupport.SeedMember(t, pool, foreign, other)

	testsupport.SeedMessage(t, pool, chatty, other, "are we still on")
	testsupport.SeedMessage(t, pool, chatty, member, "yes")
	testsupport.SeedMessage(t, pool, foreign, other, "nothing to do with you")

	summaries, err := st.ListRooms(ctx, member, nil, 10)
	require.NoError(t, err)
	require.Len(t, summaries, 2)

	require.Equal(t, chatty, summaries[0].Room.TripID)
	require.NotNil(t, summaries[0].LastMessage)
	require.Equal(t, "yes", summaries[0].LastMessage.Body)
	require.Equal(t, 1, summaries[0].UnreadCount, "the caller's own message is not unread")

	require.Equal(t, silent, summaries[1].Room.TripID)
	require.Nil(t, summaries[1].LastMessage)
	require.Equal(t, 0, summaries[1].UnreadCount)
}

// Keyset pagination on the room list, which is the shape CLAUDE.md fixes for
// every list endpoint. The cursor has to carry the tiebreaker, or two rooms
// whose last activity is identical would repeat or vanish.
func TestListRoomsPagesWithACursor(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	member := uuid.New()
	for i := 0; i < 5; i++ {
		tripID := uuid.New()
		testsupport.SeedRoom(t, pool, tripID, "Room "+strconv.Itoa(i), domain.StatusRecruiting)
		testsupport.SeedMember(t, pool, tripID, member)
	}

	seen := map[uuid.UUID]bool{}
	var cursor *store.RoomsCursor
	for page := 0; page < 5; page++ {
		summaries, err := st.ListRooms(ctx, member, cursor, 2)
		require.NoError(t, err)
		if len(summaries) == 0 {
			break
		}
		for _, summary := range summaries {
			require.False(t, seen[summary.Room.TripID], "a room appeared on two pages")
			seen[summary.Room.TripID] = true
		}
		last := summaries[len(summaries)-1]
		cursor = &store.RoomsCursor{LastActivity: last.LastActivity, TripID: last.Room.TripID}
	}
	require.Len(t, seen, 5, "every room appeared exactly once")
}

func TestMarkReadOnlyMovesForward(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, reader, writer := uuid.New(), uuid.New(), uuid.New()
	testsupport.SeedRoom(t, pool, tripID, "Watermark", domain.StatusRecruiting)
	testsupport.SeedMember(t, pool, tripID, reader)
	testsupport.SeedMember(t, pool, tripID, writer)

	testsupport.SeedMessage(t, pool, tripID, writer, "one")
	second := testsupport.SeedMessage(t, pool, tripID, writer, "two")
	testsupport.SeedMessage(t, pool, tripID, writer, "three")

	require.NoError(t, st.MarkRead(ctx, tripID, reader, second))
	summaries, err := st.ListRooms(ctx, reader, nil, 10)
	require.NoError(t, err)
	require.Equal(t, 1, summaries[0].UnreadCount)

	// A second tab, scrolled further back, must not bring the badge back.
	require.NoError(t, st.MarkRead(ctx, tripID, reader, 1))
	summaries, err = st.ListRooms(ctx, reader, nil, 10)
	require.NoError(t, err)
	require.Equal(t, 1, summaries[0].UnreadCount)
}
