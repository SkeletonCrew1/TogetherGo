package httpapi_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The acceptance criterion, stated as a test: two approved participants, one
// room, and a message sent by one appears at the other.
//
// It goes the whole way — ticket, handshake, membership re-check, INSERT, Redis
// publish, subscription, socket write — because every one of those is a place
// the message could be lost and none of them is exercised by testing the layer
// underneath it.
func TestTwoClientsInOneRoomExchangeMessages(t *testing.T) {
	h := newHarness(t)

	tripID := uuid.New()
	organizer, participant := uuid.New(), uuid.New()
	h.seedRoom(tripID, "Carpathians ridge", organizer, participant)

	first := h.primary.dial(organizer, tripID)
	second := h.primary.dial(participant, tripID)

	first.sendMessage("we leave at six", "client-1")

	// The recipient sees it.
	received := second.awaitMessage("we leave at six")
	require.Equal(t, tripID, received.TripID)
	require.Equal(t, organizer, received.SenderID)
	require.NotZero(t, received.ID)
	require.NotEmpty(t, received.CreatedAt)

	// And so does the sender, with the client_msg_id echoed back — which is
	// what lets the composer replace its optimistic row instead of rendering
	// the message twice.
	echoed := first.awaitMessage("we leave at six")
	require.Equal(t, "client-1", echoed.ClientMsgID)
	require.Equal(t, received.ID, echoed.ID)

	// And the conversation goes both ways.
	second.sendMessage("bringing the map", "client-2")
	reply := first.awaitMessage("bringing the map")
	require.Equal(t, participant, reply.SenderID)
	require.Greater(t, reply.ID, received.ID)
}

// The acceptance criterion for `docker compose up --scale chat=2`.
//
// Two hubs, two HTTP servers, one Redis, one database: exactly the shape two
// replicas have. Without the fan-out each instance would see only its own
// sockets and this test would hang — which is the failure it exists to catch,
// because with one replica everything works and the bug is invisible.
func TestMessagesReachSocketsOnAnotherReplica(t *testing.T) {
	h := newHarness(t)
	other := h.addReplica()

	tripID := uuid.New()
	alice, bob := uuid.New(), uuid.New()
	h.seedRoom(tripID, "Two replicas", alice, bob)

	onFirst := h.primary.dial(alice, tripID)
	onSecond := other.dial(bob, tripID)

	onFirst.sendMessage("can you hear me on the other instance", "client-1")

	received := onSecond.awaitMessage("can you hear me on the other instance")
	require.Equal(t, alice, received.SenderID)

	// Both directions, because a subscription that only works one way is a
	// plausible bug and an invisible one in a single-replica test.
	onSecond.sendMessage("clearly", "client-2")
	back := onFirst.awaitMessage("clearly")
	require.Equal(t, bob, back.SenderID)
}

// A non-member cannot get a ticket, and a ticket that was never issued cannot
// open a socket.
//
// The two halves are the two layers of the same rule, and both are asserted
// because either one alone would leave a hole: a service that only checked at
// the ticket endpoint could be opened with a forged ticket, and one that only
// checked at the handshake would hand out tickets to anybody who asked.
func TestNonMembersAreRefused(t *testing.T) {
	h := newHarness(t)

	tripID := uuid.New()
	member, stranger := uuid.New(), uuid.New()
	h.seedRoom(tripID, "Members only", member)

	t.Run("a non-member's ticket request is 403", func(t *testing.T) {
		resp := h.primary.do(http.MethodPost, "/api/chat/tickets", stranger,
			map[string]string{"trip_id": tripID.String()})

		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "not_member", errorCode(t, resp))
	})

	t.Run("a forged ticket closes with 4401", func(t *testing.T) {
		// Well-formed and entirely invented: the right length, the right
		// alphabet, and no corresponding key in Redis.
		forged := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

		socket := h.primary.dialWithTicket(tripID, forged)
		require.Equal(t, websocket.StatusCode(4401), socket.awaitClose(10*time.Second))
	})

	t.Run("a member's ticket does not open another trip's room", func(t *testing.T) {
		// The ticket is real. It is simply for a different room, which is the
		// check that keeps membership of one trip from being membership of all
		// of them.
		otherTrip := uuid.New()
		h.seedRoom(otherTrip, "Somewhere else", member)

		ticket := h.primary.ticket(member, tripID)
		socket := h.primary.dialWithTicket(otherTrip, ticket)
		require.Equal(t, websocket.StatusCode(4401), socket.awaitClose(10*time.Second))
	})
}

// A ticket is spent by the connection that redeems it.
//
// This is the property GETDEL buys, and the reason the redemption is one Redis
// command rather than a GET and a DEL: two handshakes racing on the same ticket
// must not both succeed, and a captured ticket must be worthless the moment its
// owner uses it.
func TestATicketCannotBeReused(t *testing.T) {
	h := newHarness(t)

	tripID := uuid.New()
	member := uuid.New()
	h.seedRoom(tripID, "Single use", member)

	ticket := h.primary.ticket(member, tripID)

	// The first use works, and the socket stays open.
	first := h.primary.dialWithTicket(tripID, ticket)
	first.sendMessage("still connected", "client-1")
	first.awaitMessage("still connected")

	// The second finds nothing in Redis and is closed with the same code an
	// expired ticket gets, because from the client's side they are the same
	// problem with the same remedy.
	second := h.primary.dialWithTicket(tripID, ticket)
	require.Equal(t, websocket.StatusCode(4401), second.awaitClose(10*time.Second))
}

// Over the rate limit the message is dropped and the sender is told — and the
// connection stays open.
//
// The last clause is the one worth testing. Closing the socket would be the
// easy implementation and the wrong one: a user typing fast is a user, and
// dropping their connection costs them the conversation over something that
// corrects itself in ten seconds.
func TestTheTwentyFirstMessageIsRejectedWithoutDisconnecting(t *testing.T) {
	h := newHarness(t)

	tripID := uuid.New()
	sender, watcher := uuid.New(), uuid.New()
	h.seedRoom(tripID, "Rate limited", sender, watcher)

	socket := h.primary.dial(sender, tripID)
	observer := h.primary.dial(watcher, tripID)

	// The limit is twenty per ten seconds, and the window is anchored to the
	// first message — so twenty-one sent back to back are twenty-one in one
	// window, with no bucket boundary for the twenty-first to slip through.
	for i := 1; i <= 20; i++ {
		socket.sendMessage("message "+strconv.Itoa(i), "client-"+strconv.Itoa(i))
	}
	// Waiting for the twentieth to come back before sending the next is what
	// makes the assertion below about the limiter rather than about a race
	// between the socket and the database.
	observer.awaitMessage("message 20")

	socket.sendMessage("message 21", "client-21")

	refusal := socket.await(10*time.Second, "a rate_limited error frame", func(f frame) bool {
		return f.Type == "error"
	})
	require.Equal(t, "rate_limited", refusal.Code)

	// The message was dropped, not queued: the observer never sees it.
	observer.expectNoFrame(500*time.Millisecond, func(f frame) bool {
		return f.Type == "message" && f.Body == "message 21"
	})

	// And the connection is still usable once the window has passed.
	require.NoError(t, h.redis.Del(context.Background(), rateLimitKey(tripID, sender)).Err())
	socket.sendMessage("message 22", "client-22")
	observer.awaitMessage("message 22")
}

// rateLimitKey mirrors the key internal/realtime builds. Deleting it is how
// this test skips ten seconds of waiting without making the window
// configurable for the sake of a test.
func rateLimitKey(tripID, userID uuid.UUID) string {
	return "chat:rate:" + tripID.String() + ":" + userID.String()
}

// An idle socket outlives the pong timeout, because the keepalive refreshes it.
//
// This is the regression test for a socket that closed at 59.4 seconds with
// nothing on either end having asked it to. Sixty seconds is the pong timeout,
// and a connection dying just under it is the signature of a deadline that is
// set once and never extended.
//
// The service's own keepalive is not that: internal/hub's writer goroutine
// pings every CHAT_PING_INTERVAL, the reader goroutine's Read is what takes the
// pong off the wire, and coder/websocket's Ping resolves on it — so the sixty
// seconds is a deadline on *one* ping's answer, restarted every thirty. This
// test pins that down at the durations the service ships with (30s/60s rather
// than the harness's fast pair), because at one-second pings the property is
// true for reasons that would not survive the real configuration.
//
// Ninety seconds is three ping intervals and one and a half pong timeouts: long
// enough that a deadline nothing refreshed would have fired, twice.
func TestAnIdleSocketOutlivesThePongTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("holds a connection open for 90 seconds")
	}

	h := newHarnessWith(t, keepalive{
		// The production defaults, from config.Load.
		pingInterval: 30 * time.Second,
		pongTimeout:  60 * time.Second,
		// Comfortably longer than the run, so a swept presence entry cannot
		// broadcast anything into a test that is about silence.
		presenceTTL: 10 * time.Minute,
	})

	tripID := uuid.New()
	member := uuid.New()
	h.seedRoom(tripID, "Nobody is saying anything", member)

	socket := h.primary.dial(member, tripID)

	// No writes from the client for the whole window. The only traffic on the
	// connection is the server's ping and the client library's automatic pong.
	socket.expectStillOpen(90 * time.Second)

	// And it is not merely unclosed — it still works.
	socket.sendMessage("still here", "client-1")
	received := socket.awaitMessage("still here")
	require.Equal(t, member, received.SenderID)
}
