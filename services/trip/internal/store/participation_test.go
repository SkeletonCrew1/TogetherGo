package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/events"
	"github.com/togethergo/trip/internal/store"
)

// --- fixtures ---------------------------------------------------------------

// recruitingTrip creates a trip and publishes it, which is the only state in
// which anybody may ask to join one.
func recruitingTrip(t *testing.T, s *store.Store, organizer uuid.UUID, capacity int) uuid.UUID {
	t.Helper()

	in := validInput()
	in.Capacity = capacity
	detail, err := s.CreateTrip(context.Background(), organizer, in)
	require.NoError(t, err)

	_, err = s.ChangeStatus(context.Background(), detail.Trip.ID, organizer, domain.StatusRecruiting)
	require.NoError(t, err)
	return detail.Trip.ID
}

// outboxEvent is one row of the outbox as the tests read it. The relay is not
// involved: what is asserted here is that the domain write and the promise to
// publish landed in the same transaction, which is a fact about the table.
type outboxEvent struct {
	EventType   string
	AggregateID uuid.UUID
	Payload     map[string]any
}

func outboxEvents(t *testing.T, pool *pgxpool.Pool) []outboxEvent {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT event_type, aggregate_id, payload FROM outbox ORDER BY created_at, id`)
	require.NoError(t, err)
	defer rows.Close()

	var out []outboxEvent
	for rows.Next() {
		var e outboxEvent
		var raw []byte
		require.NoError(t, rows.Scan(&e.EventType, &e.AggregateID, &raw))
		require.NoError(t, json.Unmarshal(raw, &e.Payload))
		out = append(out, e)
	}
	require.NoError(t, rows.Err())
	return out
}

func onlyEvent(t *testing.T, pool *pgxpool.Pool, eventType string) outboxEvent {
	t.Helper()

	var found []outboxEvent
	for _, e := range outboxEvents(t, pool) {
		if e.EventType == eventType {
			found = append(found, e)
		}
	}
	require.Len(t, found, 1, "expected exactly one %s in the outbox", eventType)
	return found[0]
}

func approvedCount(t *testing.T, pool *pgxpool.Pool, tripID uuid.UUID) int {
	t.Helper()

	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT approved_count FROM trips WHERE id = $1`, tripID).Scan(&count))
	return count
}

// --- creating a request -----------------------------------------------------

func TestCreateJoinRequest(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer, requester := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	request, err := s.CreateJoinRequest(ctx, tripID, requester, ptr("I have done this ridge before."))
	require.NoError(t, err)

	require.Equal(t, tripID, request.TripID)
	require.Equal(t, requester, request.UserID)
	require.Equal(t, domain.JoinPending, request.Status)
	require.Equal(t, "I have done this ridge before.", *request.Message)
	require.Nil(t, request.DecidedAt)
	require.Nil(t, request.DecidedBy)

	// A pending request occupies no seat. The counter moves at approval, which
	// is what makes "capacity" mean "people who are coming".
	require.Equal(t, 1, approvedCount(t, pool, tripID))

	event := onlyEvent(t, pool, events.TypeJoinRequestCreated)
	require.Equal(t, tripID, event.AggregateID)
	require.Equal(t, request.ID.String(), event.Payload["join_request_id"])
	require.Equal(t, organizer.String(), event.Payload["organizer_id"])
	require.Equal(t, requester.String(), event.Payload["requester_id"])
	require.Equal(t, "Carpathians in autumn", event.Payload["title"])
	require.Equal(t, "I have done this ridge before.", event.Payload["message"])
}

// Each refusal is its own error, because each one means a different thing has
// gone wrong and a different thing should happen next.
func TestCreateJoinRequestRefusals(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	t.Run("their own trip", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		_, err := s.CreateJoinRequest(ctx, tripID, organizer, nil)
		require.ErrorIs(t, err, domain.ErrOwnTrip)
	})

	t.Run("a trip that is not recruiting", func(t *testing.T) {
		in := validInput()
		detail, err := s.CreateTrip(ctx, organizer, in)
		require.NoError(t, err)
		// A draft is invisible to everybody but its organizer, so a stranger
		// gets 404 rather than "this trip is not recruiting" — which would
		// confirm the id names a real trip.
		_, err = s.CreateJoinRequest(ctx, detail.Trip.ID, uuid.New(), nil)
		require.ErrorIs(t, err, domain.ErrTripNotFound)

		_, err = s.ChangeStatus(ctx, detail.Trip.ID, organizer, domain.StatusRecruiting)
		require.NoError(t, err)
		_, err = s.ChangeStatus(ctx, detail.Trip.ID, organizer, domain.StatusInProgress)
		require.NoError(t, err)

		_, err = s.CreateJoinRequest(ctx, detail.Trip.ID, uuid.New(), nil)
		require.ErrorIs(t, err, domain.ErrTripNotRecruiting)
	})

	t.Run("a trip they are already on", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		joiner := uuid.New()

		request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
		require.NoError(t, err)
		_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
		require.NoError(t, err)

		_, err = s.CreateJoinRequest(ctx, tripID, joiner, nil)
		require.ErrorIs(t, err, domain.ErrAlreadyParticipant)
	})

	t.Run("a second open request", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		joiner := uuid.New()

		_, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
		require.NoError(t, err)
		_, err = s.CreateJoinRequest(ctx, tripID, joiner, nil)
		require.ErrorIs(t, err, domain.ErrRequestPending)
	})

	t.Run("a message longer than the contract allows", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		long := make([]rune, domain.JoinMessageMaxLen+1)
		for i := range long {
			long[i] = 'я'
		}
		_, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), ptr(string(long)))
		require.ErrorIs(t, err, domain.ErrValidation)
	})
}

// The partial unique index is on `status = 'pending'` only, so a rejection is
// not a life sentence: the organizer who says no in March can be asked again in
// June, and the history of both is kept.
func TestReapplyingAfterARejection(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer, joiner := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	first, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)
	_, err = s.RejectJoinRequest(ctx, tripID, first.ID, organizer, ptr("Group is already full of navigators."))
	require.NoError(t, err)

	second, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)

	page, err := s.JoinRequests(ctx, tripID, organizer, domain.JoinRequestQuery{})
	require.NoError(t, err)
	require.Len(t, page.Items, 2, "the rejected request is history and stays")
}

// --- cancelling -------------------------------------------------------------

func TestCancelOwnJoinRequest(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer, joiner := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	_, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)

	require.NoError(t, s.CancelJoinRequest(ctx, tripID, joiner))

	// Cancelling never touches approved_count: the request never held a seat.
	require.Equal(t, 1, approvedCount(t, pool, tripID))

	// And it publishes nothing of its own. Nobody outside this service acted on
	// a pending request, so there is nothing to undo — an event no consumer is
	// bound to is a queue that fills up.
	//
	// The trip's own lifecycle events are excluded rather than asserted on:
	// recruitingTrip publishes the draft, which writes a trip.status_changed
	// and a trip.created that have nothing to do with this request.
	for _, e := range outboxEvents(t, pool) {
		if e.EventType == events.TypeTripStatusChanged || e.EventType == events.TypeTripCreated {
			continue
		}
		require.Equal(t, events.TypeJoinRequestCreated, e.EventType)
	}

	// Cancelling twice is a 404, not a silent success: there is no open request.
	require.ErrorIs(t, s.CancelJoinRequest(ctx, tripID, joiner), domain.ErrRequestNotFound)

	// The cancelled row is still there, and having cancelled frees the user to
	// ask again.
	_, err = s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)
}

// --- approving --------------------------------------------------------------

func TestApproveJoinRequest(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer, joiner := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)

	approved, err := s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
	require.NoError(t, err)

	require.Equal(t, domain.JoinApproved, approved.Status)
	require.NotNil(t, approved.DecidedAt)
	require.Equal(t, organizer, *approved.DecidedBy)

	require.Equal(t, 2, approvedCount(t, pool, tripID))

	detail, err := s.TripDetail(ctx, tripID)
	require.NoError(t, err)
	require.Len(t, detail.Participants, 2)
	require.Equal(t, 2, detail.Trip.ApprovedCount)

	var roles []string
	for _, p := range detail.Participants {
		if p.UserID == joiner {
			roles = append(roles, p.Role)
		}
	}
	require.Equal(t, []string{domain.RoleParticipant}, roles)

	// The payload carries everything chat and notification need to render their
	// side without calling back into this service.
	event := onlyEvent(t, pool, events.TypeJoinRequestApproved)
	require.Equal(t, tripID, event.AggregateID)
	require.Equal(t, tripID.String(), event.Payload["trip_id"])
	require.Equal(t, "Carpathians in autumn", event.Payload["title"])
	require.Equal(t, joiner.String(), event.Payload["participant_id"])
	require.Equal(t, organizer.String(), event.Payload["organizer_id"])
	require.Equal(t, float64(2), event.Payload["participant_count"])
	require.Equal(t, float64(8), event.Payload["max_participants"])
}

func TestApproveJoinRequestRefusals(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	t.Run("by somebody who is not the organizer", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		request, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)

		_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, uuid.New())
		require.ErrorIs(t, err, domain.ErrNotOrganizer)
	})

	t.Run("a request that is already decided", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		request, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)

		_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
		require.NoError(t, err)
		_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
		require.ErrorIs(t, err, domain.ErrRequestNotPending)
	})

	t.Run("a request belonging to another trip", func(t *testing.T) {
		mine := recruitingTrip(t, s, organizer, 8)
		theirs := recruitingTrip(t, s, organizer, 8)
		request, err := s.CreateJoinRequest(ctx, theirs, uuid.New(), nil)
		require.NoError(t, err)

		_, err = s.ApproveJoinRequest(ctx, mine, request.ID, organizer)
		require.ErrorIs(t, err, domain.ErrRequestNotFound)
	})

	t.Run("a trip that has stopped recruiting", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		request, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)

		_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusInProgress)
		require.NoError(t, err)

		_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
		require.ErrorIs(t, err, domain.ErrTripNotRecruiting)
	})

	t.Run("a full trip", func(t *testing.T) {
		// Capacity 2: the organizer and one other.
		tripID := recruitingTrip(t, s, organizer, 2)

		first, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)
		second, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)

		_, err = s.ApproveJoinRequest(ctx, tripID, first.ID, organizer)
		require.NoError(t, err)

		_, err = s.ApproveJoinRequest(ctx, tripID, second.ID, organizer)
		require.ErrorIs(t, err, domain.ErrTripFull)
	})
}

// --- rejecting --------------------------------------------------------------

func TestRejectJoinRequest(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer, joiner := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)

	rejected, err := s.RejectJoinRequest(ctx, tripID, request.ID, organizer, ptr("Group is full of navigators already."))
	require.NoError(t, err)

	require.Equal(t, domain.JoinRejected, rejected.Status)
	require.Equal(t, "Group is full of navigators already.", *rejected.DecisionReason)
	require.Equal(t, organizer, *rejected.DecidedBy)

	// Rejecting never touches approved_count.
	require.Equal(t, 1, approvedCount(t, pool, tripID))

	event := onlyEvent(t, pool, events.TypeJoinRequestRejected)
	require.Equal(t, joiner.String(), event.Payload["requester_id"])
	// The reason on the bus is a code a template can act on. The organizer's
	// sentence stays in this service (contracts/events.md).
	require.Equal(t, events.RejectedByOrganizer, event.Payload["reason"])
	require.NotContains(t, event.Payload, "decision_reason")
	for _, value := range event.Payload {
		require.NotEqual(t, "Group is full of navigators already.", value)
	}

	// The requester can read the sentence, and only through this service.
	mine, err := s.MyJoinRequest(ctx, tripID, joiner)
	require.NoError(t, err)
	require.Equal(t, "Group is full of navigators already.", *mine.DecisionReason)
}

// An organizer whose trip has filled up must still be able to clear the queue.
func TestRejectingIsAllowedOnAFullTrip(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()
	tripID := recruitingTrip(t, s, organizer, 2)

	taken, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)
	spare, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)

	_, err = s.ApproveJoinRequest(ctx, tripID, taken.ID, organizer)
	require.NoError(t, err)

	_, err = s.RejectJoinRequest(ctx, tripID, spare.ID, organizer, nil)
	require.NoError(t, err)
}

// --- the organizer's queue --------------------------------------------------

func TestJoinRequestsAreOrderedPendingFirst(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()
	tripID := recruitingTrip(t, s, organizer, 20)

	// Two decided, then two still open. Newest first within each group, so the
	// expected order is the reverse of the creation order inside each half.
	decidedFirst, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)
	decidedSecond, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)
	pendingFirst, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)
	pendingSecond, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)

	_, err = s.ApproveJoinRequest(ctx, tripID, decidedFirst.ID, organizer)
	require.NoError(t, err)
	_, err = s.RejectJoinRequest(ctx, tripID, decidedSecond.ID, organizer, nil)
	require.NoError(t, err)

	page, err := s.JoinRequests(ctx, tripID, organizer, domain.JoinRequestQuery{})
	require.NoError(t, err)
	require.Nil(t, page.NextCursor)

	got := make([]uuid.UUID, 0, len(page.Items))
	for _, item := range page.Items {
		got = append(got, item.ID)
	}
	require.Equal(t,
		[]uuid.UUID{pendingSecond.ID, pendingFirst.ID, decidedSecond.ID, decidedFirst.ID},
		got,
		"pending first, newest first within each group")
}

func TestJoinRequestsPaginateAcrossTheGroupBoundary(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()
	tripID := recruitingTrip(t, s, organizer, 20)

	// Five requests, the two oldest rejected, so the pending/decided boundary
	// falls inside a page at limit 2 — which is the case a cursor that carried
	// only a timestamp would get wrong.
	created := make([]uuid.UUID, 0, 5)
	for range 5 {
		request, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
		require.NoError(t, err)
		created = append(created, request.ID)
	}
	for _, id := range created[:2] {
		_, err := s.RejectJoinRequest(ctx, tripID, id, organizer, nil)
		require.NoError(t, err)
	}

	var walked []uuid.UUID
	query := domain.JoinRequestQuery{Limit: 2}
	for page := 0; ; page++ {
		require.Less(t, page, 10, "pagination did not terminate")

		result, err := s.JoinRequests(ctx, tripID, organizer, query)
		require.NoError(t, err)
		for _, item := range result.Items {
			walked = append(walked, item.ID)
		}
		if result.NextCursor == nil {
			break
		}
		query.Cursor = result.NextCursor
	}

	require.Len(t, walked, 5)
	require.ElementsMatch(t, created, walked, "every request, exactly once")

	single, err := s.JoinRequests(ctx, tripID, organizer, domain.JoinRequestQuery{Limit: 50})
	require.NoError(t, err)
	expected := make([]uuid.UUID, 0, 5)
	for _, item := range single.Items {
		expected = append(expected, item.ID)
	}
	require.Equal(t, expected, walked, "walking the pages gives the same order as one big page")
}

func TestJoinRequestsIsOrganizerOnly(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer, joiner := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	_, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)

	// Even the requester cannot read the queue: who else applied, and what they
	// wrote, is not their business.
	_, err = s.JoinRequests(ctx, tripID, joiner, domain.JoinRequestQuery{})
	require.ErrorIs(t, err, domain.ErrNotOrganizer)
}

// --- leaving and being removed ---------------------------------------------

func TestRemoveParticipant(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer, joiner := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)
	_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
	require.NoError(t, err)
	require.Equal(t, 2, approvedCount(t, pool, tripID))

	detail, err := s.RemoveParticipant(ctx, tripID, joiner, organizer)
	require.NoError(t, err)

	require.Equal(t, 1, detail.Trip.ApprovedCount)
	require.Equal(t, 1, approvedCount(t, pool, tripID))
	require.Len(t, detail.Participants, 1)

	event := onlyEvent(t, pool, events.TypeParticipantRemoved)
	require.Equal(t, joiner.String(), event.Payload["participant_id"])
	require.Equal(t, organizer.String(), event.Payload["removed_by"])
	require.Equal(t, events.RemovedByOrganizer, event.Payload["reason"])
	require.Equal(t, float64(1), event.Payload["participant_count"])

	// Removing them again is a 404: they are not on the trip. This is the guard
	// that keeps approved_count from being decremented twice.
	_, err = s.RemoveParticipant(ctx, tripID, joiner, organizer)
	require.ErrorIs(t, err, domain.ErrNotParticipant)
	require.Equal(t, 1, approvedCount(t, pool, tripID))
}

func TestLeaveTrip(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer, joiner := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)
	_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
	require.NoError(t, err)

	detail, err := s.LeaveTrip(ctx, tripID, joiner)
	require.NoError(t, err)
	require.Equal(t, 1, detail.Trip.ApprovedCount)

	// The same event as a removal — chat has to revoke access either way — and
	// `removed_by` plus `reason` are what tell the two apart.
	event := onlyEvent(t, pool, events.TypeParticipantRemoved)
	require.Equal(t, joiner.String(), event.Payload["participant_id"])
	require.Equal(t, joiner.String(), event.Payload["removed_by"])
	require.Equal(t, events.RemovedVoluntarily, event.Payload["reason"])
}

func TestTheOrganizerCannotLeaveTheirOwnTrip(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	_, err := s.LeaveTrip(ctx, tripID, organizer)
	require.ErrorIs(t, err, domain.ErrCannotRemoveOrganizer)

	// Nor can they remove themselves by the other door.
	_, err = s.RemoveParticipant(ctx, tripID, organizer, organizer)
	require.ErrorIs(t, err, domain.ErrCannotRemoveOrganizer)
}

// A finished trip's roster has already gone out on trip.completed, and identity
// has opened a rating window against exactly those people.
func TestTheRosterOfAnEndedTripIsFrozen(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer, joiner := uuid.New(), uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
	require.NoError(t, err)
	_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
	require.NoError(t, err)

	_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusInProgress)
	require.NoError(t, err)
	_, err = s.ChangeStatus(ctx, tripID, organizer, domain.StatusCompleted)
	require.NoError(t, err)

	_, err = s.RemoveParticipant(ctx, tripID, joiner, organizer)
	require.ErrorIs(t, err, domain.ErrTripEnded)
	_, err = s.LeaveTrip(ctx, tripID, joiner)
	require.ErrorIs(t, err, domain.ErrTripEnded)
}

// --- invitations ------------------------------------------------------------

func TestInviteUsers(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	first, second := uuid.New(), uuid.New()
	invites, err := s.InviteUsers(ctx, tripID, organizer, []uuid.UUID{first, second, first})
	require.NoError(t, err)
	// The repeat is dropped, not rejected: naming the same person twice in one
	// request has an obvious intent.
	require.Len(t, invites, 2)

	var invited []uuid.UUID
	var sent []outboxEvent
	for _, invite := range invites {
		invited = append(invited, invite.InvitedUserID)
		require.Equal(t, invite.CreatedAt.Add(domain.InviteTTL), invite.ExpiresAt())
	}
	for _, e := range outboxEvents(t, pool) {
		if e.EventType == events.TypeTripInviteSent {
			sent = append(sent, e)
		}
	}
	require.ElementsMatch(t, []uuid.UUID{first, second}, invited)
	require.Len(t, sent, 2, "one event per invitation")

	// Matched by invite id rather than by position. Both rows are written in one
	// transaction and therefore share a created_at, so the order the outbox
	// hands them back is not defined — which is the contract (no global ordering
	// guarantee) and not something a test should pretend otherwise about.
	byInvite := make(map[string]map[string]any, len(sent))
	for _, e := range sent {
		require.Equal(t, tripID, e.AggregateID)
		byInvite[e.Payload["invite_id"].(string)] = e.Payload
	}

	for _, invite := range invites {
		payload, found := byInvite[invite.ID.String()]
		require.Truef(t, found, "no trip.invite_sent for invite %s", invite.ID)

		require.Equal(t, tripID.String(), payload["trip_id"])
		require.Equal(t, "Carpathians in autumn", payload["title"])
		require.Equal(t, organizer.String(), payload["inviter_id"])
		require.Equal(t, invite.InvitedUserID.String(), payload["invitee_id"])
		// No email address on the bus: notification resolves it from its own
		// projection, so an address is not left in every log and DLQ this
		// message passes through.
		require.NotContains(t, payload, "email")
	}
}

func TestInviteUsersIsAllOrNothing(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()
	tripID := recruitingTrip(t, s, organizer, 8)

	already := uuid.New()
	_, err := s.InviteUsers(ctx, tripID, organizer, []uuid.UUID{already})
	require.NoError(t, err)

	fresh := uuid.New()
	_, err = s.InviteUsers(ctx, tripID, organizer, []uuid.UUID{fresh, already})

	var conflict *domain.UserConflictError
	require.ErrorAs(t, err, &conflict)
	require.ErrorIs(t, err, domain.ErrAlreadyInvited)
	require.Equal(t, []uuid.UUID{already}, conflict.UserIDs)

	// Nothing was written for the one that would have been fine.
	var count int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM trip_invites WHERE trip_id = $1`, tripID).Scan(&count))
	require.Equal(t, 1, count)
}

func TestInviteUsersRefusals(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	t.Run("themselves", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		_, err := s.InviteUsers(ctx, tripID, organizer, []uuid.UUID{organizer})
		require.ErrorIs(t, err, domain.ErrCannotInviteSelf)
	})

	t.Run("somebody already on the trip", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		joiner := uuid.New()
		request, err := s.CreateJoinRequest(ctx, tripID, joiner, nil)
		require.NoError(t, err)
		_, err = s.ApproveJoinRequest(ctx, tripID, request.ID, organizer)
		require.NoError(t, err)

		_, err = s.InviteUsers(ctx, tripID, organizer, []uuid.UUID{joiner})
		require.ErrorIs(t, err, domain.ErrAlreadyParticipant)
	})

	t.Run("by somebody who is not the organizer", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		_, err := s.InviteUsers(ctx, tripID, uuid.New(), []uuid.UUID{uuid.New()})
		require.ErrorIs(t, err, domain.ErrNotOrganizer)
	})

	t.Run("to a draft", func(t *testing.T) {
		detail, err := s.CreateTrip(ctx, organizer, validInput())
		require.NoError(t, err)
		// An invitation to a draft is a link to a 404: the invitee cannot see
		// the trip. Publish first.
		_, err = s.InviteUsers(ctx, detail.Trip.ID, organizer, []uuid.UUID{uuid.New()})
		require.ErrorIs(t, err, domain.ErrTripNotRecruiting)
	})

	t.Run("nobody at all", func(t *testing.T) {
		tripID := recruitingTrip(t, s, organizer, 8)
		_, err := s.InviteUsers(ctx, tripID, organizer, nil)
		require.ErrorIs(t, err, domain.ErrValidation)
	})
}

// Deleting a trip takes its requests and invitations with it — the ON DELETE
// CASCADE in the migration, asserted rather than assumed.
func TestDeletingATripCascadesToItsParticipationRows(t *testing.T) {
	s, pool := newStore(t)
	ctx := context.Background()
	organizer := uuid.New()

	tripID := recruitingTrip(t, s, organizer, 8)
	_, err := s.CreateJoinRequest(ctx, tripID, uuid.New(), nil)
	require.NoError(t, err)
	_, err = s.InviteUsers(ctx, tripID, organizer, []uuid.UUID{uuid.New()})
	require.NoError(t, err)

	// DeleteTrip is draft-only, so the row is taken out from under the store.
	_, err = pool.Exec(ctx, `DELETE FROM trips WHERE id = $1`, tripID)
	require.NoError(t, err)

	var requests, invites int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM join_requests WHERE trip_id = $1`, tripID).Scan(&requests))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM trip_invites WHERE trip_id = $1`, tripID).Scan(&invites))
	require.Zero(t, requests)
	require.Zero(t, invites)
}
