package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/events"
)

// The two events this service publishes about a trip's existence rather than
// about its status: `trip.created` on publication and `trip.cancelled` when it
// is called off.
//
// Both go to chat, which builds its rooms out of them and nothing else — it
// never calls this service — so a missing event here is a trip with no
// conversation and a cancelled trip whose room stays open. That is not
// something the chat service can detect or work around, which is why these are
// pinned on this side.

// Publication is the event, not creation: a draft is visible to nobody but its
// organizer, and provisioning a chat room for one would create rooms for trips
// that are never announced.
func TestPublishingATripAnnouncesIt(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	in := validInput()
	in.Capacity = 6
	detail, err := s.CreateTrip(ctx, organizer, in)
	require.NoError(t, err)
	tripID := detail.Trip.ID

	// A draft announces nothing.
	require.Empty(t, outboxPayloads(t, pool, tripID, events.TypeTripCreated))

	_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusRecruiting)
	require.NoError(t, err)

	created := outboxPayloads(t, pool, tripID, events.TypeTripCreated)
	require.Len(t, created, 1, "exactly one trip.created per trip")

	var payload events.TripCreated
	require.NoError(t, json.Unmarshal(created[0], &payload))
	require.Equal(t, tripID, payload.TripID)
	require.Equal(t, organizer, payload.OrganizerID)
	require.Equal(t, detail.Trip.Title, payload.Title)
	require.Equal(t, detail.Trip.Category, payload.Category)
	require.Equal(t, 6, payload.MaxParticipants)
	require.NotEmpty(t, payload.StartsAt)
	require.NotEmpty(t, payload.EndsAt)
	// The catalogue fixes this literal, and it is not this service's
	// `recruiting`: the event's vocabulary is the contract's.
	require.Equal(t, "open", payload.Status)

	// And the status change is on the bus alongside it, from the same function.
	require.Len(t, outboxPayloads(t, pool, tripID, events.TypeTripStatusChanged), 1)
}

// Cancellation carries everyone with a stake in the trip — including the people
// whose join request was still open, who are otherwise waiting for an answer
// that is never coming.
func TestCancellingATripAnnouncesItToEveryoneAffected(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()

	organizer := uuid.New()
	approved := uuid.New()
	pending := uuid.New()

	tripID := recruitingTrip(t, s, organizer, 5)

	request, err := s.CreateJoinRequest(ctx, tripID, approved, nil)
	require.NoError(t, err)
	_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
	require.NoError(t, err)

	_, err = s.CreateJoinRequest(ctx, tripID, pending, nil)
	require.NoError(t, err)

	_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusCancelled)
	require.NoError(t, err)

	cancelled := outboxPayloads(t, pool, tripID, events.TypeTripCancelled)
	require.Len(t, cancelled, 1)

	var payload events.TripCancelled
	require.NoError(t, json.Unmarshal(cancelled[0], &payload))
	require.Equal(t, tripID, payload.TripID)
	require.Equal(t, organizer, payload.OrganizerID)
	require.NotEmpty(t, payload.Title)
	require.Equal(t, events.CancelledByOrganizer, payload.Reason)
	require.Nil(t, payload.Comment)
	require.NotEmpty(t, payload.CancelledAt)

	require.ElementsMatch(t, []uuid.UUID{approved, pending}, payload.AffectedUserIDs,
		"approved participants and open requesters, and not the organizer")
}

// A cancelled draft has nobody on it but its organizer, and the field is still
// an array rather than null — the catalogue says it is required, and a nil
// slice would marshal to null for every consumer to guard.
func TestCancellingADraftAnnouncesAnEmptyAffectedList(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	detail, err := s.CreateTrip(ctx, organizer, validInput())
	require.NoError(t, err)

	_, err = s.ChangeStatus(ctx, detail.Trip.ID, organizer, domain.StatusCancelled)
	require.NoError(t, err)

	cancelled := outboxPayloads(t, pool, detail.Trip.ID, events.TypeTripCancelled)
	require.Len(t, cancelled, 1)

	// `[]`, not `null`. Decoding is what distinguishes them — a JSON null
	// unmarshals to a nil slice and an empty array does not — and the raw text
	// cannot be compared directly because jsonb re-renders it on the way out.
	var payload events.TripCancelled
	require.NoError(t, json.Unmarshal(cancelled[0], &payload))
	require.NotNil(t, payload.AffectedUserIDs)
	require.Empty(t, payload.AffectedUserIDs)

	// A cancelled draft was never published, so no trip.created was ever
	// emitted for it and chat has no room to close.
	require.Empty(t, outboxPayloads(t, pool, detail.Trip.ID, events.TypeTripCreated))
}
