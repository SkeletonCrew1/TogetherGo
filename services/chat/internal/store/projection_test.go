package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/domain"
	"github.com/togethergo/chat/internal/events"
	"github.com/togethergo/chat/internal/store"
	"github.com/togethergo/chat/internal/testsupport"
)

// The projection tests run against a real Postgres, because every claim they
// make is a claim about SQL: that an upsert is idempotent, that a foreign key
// does not fire, that a status is not rewritten. A mocked store would only
// restate the code.

func TestTripCreatedProvisionsARoomWithItsOrganizer(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, organizer := uuid.New(), uuid.New()

	applied, err := st.ApplyTripCreated(ctx, uuid.New(), events.TripCreated{
		TripID:      tripID,
		OrganizerID: organizer,
		Title:       "Carpathians ridge",
	})
	require.NoError(t, err)
	require.True(t, applied)

	access, err := st.Authorize(ctx, tripID, organizer)
	require.NoError(t, err)
	require.Equal(t, "Carpathians ridge", access.Room.Title)
	require.Equal(t, domain.StatusRecruiting, access.Room.Status)
	require.True(t, access.Room.IsOpen())
	// The room exists so the organizer can post before anybody has joined,
	// which is what contracts/events.md says this event is for.
	require.True(t, access.IsMember)
}

// The idempotency criterion: consuming the same approval twice adds one member.
//
// Both layers are proved at once. `processed_events` makes the second call a
// no-op — it returns false and writes nothing — and the upsert underneath would
// have been harmless even if it had run. Belt and braces, and the test asserts
// on the belt (the boolean) and on the outcome (the row count).
func TestConsumingAnApprovalTwiceAddsOneMember(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, organizer, participant := uuid.New(), uuid.New(), uuid.New()
	_, err := st.ApplyTripCreated(ctx, uuid.New(), events.TripCreated{
		TripID: tripID, OrganizerID: organizer, Title: "Carpathians ridge",
	})
	require.NoError(t, err)

	eventID := uuid.New()
	payload := events.JoinRequestApproved{
		TripID:        tripID,
		ParticipantID: participant,
		Title:         "Carpathians ridge",
	}

	first, err := st.ApplyJoinApproved(ctx, eventID, payload)
	require.NoError(t, err)
	require.True(t, first, "the first delivery applies the event")

	second, err := st.ApplyJoinApproved(ctx, eventID, payload)
	require.NoError(t, err)
	require.False(t, second, "a redelivery must be reported as already processed")

	require.Equal(t, 2, testsupport.MemberCount(t, pool, tripID), "organizer plus one participant")

	// A *different* event id for the same participant is a genuinely new
	// event — a re-approval after leaving — and must still not duplicate the
	// row. That is the upsert, not the ledger.
	applied, err := st.ApplyJoinApproved(ctx, uuid.New(), payload)
	require.NoError(t, err)
	require.True(t, applied)
	require.Equal(t, 2, testsupport.MemberCount(t, pool, tripID))
}

// Events are unordered, so an approval can arrive before the trip.created that
// ought to have preceded it. The handler provisions the room from the title the
// approval carries, and the late creation event then refreshes it rather than
// failing or duplicating.
func TestAnApprovalThatOvertakesItsTripCreatedStillWorks(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, organizer, participant := uuid.New(), uuid.New(), uuid.New()

	_, err := st.ApplyJoinApproved(ctx, uuid.New(), events.JoinRequestApproved{
		TripID: tripID, ParticipantID: participant, Title: "Carpathians ridge",
	})
	require.NoError(t, err)

	access, err := st.Authorize(ctx, tripID, participant)
	require.NoError(t, err)
	require.True(t, access.IsMember, "the participant is in a room nothing has created yet")

	_, err = st.ApplyTripCreated(ctx, uuid.New(), events.TripCreated{
		TripID: tripID, OrganizerID: organizer, Title: "Carpathians ridge, two days",
	})
	require.NoError(t, err)

	access, err = st.Authorize(ctx, tripID, organizer)
	require.NoError(t, err)
	require.Equal(t, "Carpathians ridge, two days", access.Room.Title, "the creation event owns the title")
	require.Equal(t, 2, testsupport.MemberCount(t, pool, tripID))
}

// Removal revokes access and keeps history. The row stays, so the messages the
// participant wrote stay attributed and everybody else's conversation has no
// holes in it.
func TestParticipantRemovedRevokesAccessWithoutDeletingAnything(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, organizer, participant := uuid.New(), uuid.New(), uuid.New()
	_, err := st.ApplyTripCreated(ctx, uuid.New(), events.TripCreated{
		TripID: tripID, OrganizerID: organizer, Title: "Carpathians ridge",
	})
	require.NoError(t, err)
	_, err = st.ApplyJoinApproved(ctx, uuid.New(), events.JoinRequestApproved{
		TripID: tripID, ParticipantID: participant, Title: "Carpathians ridge",
	})
	require.NoError(t, err)

	testsupport.SeedMessage(t, pool, tripID, participant, "I will bring the stove")

	_, err = st.ApplyParticipantRemoved(ctx, uuid.New(), events.ParticipantRemoved{
		TripID: tripID, ParticipantID: participant,
	})
	require.NoError(t, err)

	access, err := st.Authorize(ctx, tripID, participant)
	require.NoError(t, err)
	require.False(t, access.IsMember)

	// Their message is still there, and still theirs.
	messages, _, err := st.ListMessages(ctx, tripID, 0, 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, participant, messages[0].SenderID)

	// And they can be let back in without losing anything.
	_, err = st.ApplyJoinApproved(ctx, uuid.New(), events.JoinRequestApproved{
		TripID: tripID, ParticipantID: participant, Title: "Carpathians ridge",
	})
	require.NoError(t, err)
	require.True(t, mustAuthorize(t, st, tripID, participant).IsMember)
}

// A removal for a room this service has never heard of is a no-op, not a
// failure. There is no access to revoke, and dead-lettering it would put a
// message in a queue nobody can act on.
func TestRemovingSomebodyFromAnUnknownRoomIsHarmless(t *testing.T) {
	st := store.New(testsupport.Pool(t))

	applied, err := st.ApplyParticipantRemoved(context.Background(), uuid.New(), events.ParticipantRemoved{
		TripID: uuid.New(), ParticipantID: uuid.New(),
	})
	require.NoError(t, err)
	require.True(t, applied)
}

// Cancellation closes the room, and nothing reopens it — not a late
// trip.created, not a late approval. The status has one writer and it moves one
// way.
func TestCancellationIsNotUndoneByALateCreationEvent(t *testing.T) {
	pool := testsupport.Pool(t)
	st := store.New(pool)
	ctx := context.Background()

	tripID, organizer := uuid.New(), uuid.New()

	_, err := st.ApplyTripCancelled(ctx, uuid.New(), events.TripCancelled{
		TripID: tripID, Title: "Called off",
	})
	require.NoError(t, err)

	_, err = st.ApplyTripCreated(ctx, uuid.New(), events.TripCreated{
		TripID: tripID, OrganizerID: organizer, Title: "Carpathians ridge",
	})
	require.NoError(t, err)

	access := mustAuthorize(t, st, tripID, organizer)
	require.Equal(t, domain.StatusCancelled, access.Room.Status, "a late creation must not reopen a cancelled room")
	require.Equal(t, "Carpathians ridge", access.Room.Title, "but it does still supply the title")
	require.True(t, access.IsMember)
}

// An event type this build does not know is logged, marked processed and
// acked — contracts/events.md is explicit that it is not an error.
func TestAnUnknownEventIsRecordedAsHandled(t *testing.T) {
	st := store.New(testsupport.Pool(t))
	ctx := context.Background()

	eventID := uuid.New()

	first, err := st.MarkProcessed(ctx, eventID)
	require.NoError(t, err)
	require.True(t, first)

	second, err := st.MarkProcessed(ctx, eventID)
	require.NoError(t, err)
	require.False(t, second)
}

func mustAuthorize(t *testing.T, st *store.Store, tripID, userID uuid.UUID) store.Access {
	t.Helper()
	access, err := st.Authorize(context.Background(), tripID, userID)
	require.NoError(t, err)
	return access
}
