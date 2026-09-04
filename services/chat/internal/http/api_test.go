package httpapi_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/testsupport"
)

func TestHealthEndpoints(t *testing.T) {
	h := newHarness(t)

	t.Run("healthz checks nothing and always answers", func(t *testing.T) {
		resp := h.primary.do(http.MethodGet, "/healthz", uuid.Nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("readyz reports each dependency", func(t *testing.T) {
		resp := h.primary.do(http.MethodGet, "/readyz", uuid.Nil, nil)

		var body struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		decode(t, resp, &body)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "ready", body.Status)
		require.Equal(t, "ok", body.Checks["database"])
		require.Equal(t, "ok", body.Checks["jwks"])
		// Redis and the broker are not wired into the test router: an instance
		// without one reports that check as disabled, which must not make it
		// unready.
		require.Equal(t, "disabled", body.Checks["redis"])
		require.Equal(t, "disabled", body.Checks["broker"])
	})
}

func TestEveryEndpointRequiresAToken(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{
		"/api/chat/rooms",
		"/api/chat/rooms/" + uuid.NewString() + "/messages",
	} {
		resp := h.primary.do(http.MethodGet, path, uuid.Nil, nil)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, path)
		require.Equal(t, "invalid_access_token", errorCode(t, resp))
		// RFC 6750: a 401 on a bearer-protected resource says which scheme it
		// wanted.
		require.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
	}
}

func TestListRooms(t *testing.T) {
	h := newHarness(t)

	member := uuid.New()
	other := uuid.New()

	quiet := uuid.New()
	busy := uuid.New()
	somebodyElses := uuid.New()

	h.seedRoom(quiet, "Nobody has spoken here", member)
	h.seedRoom(busy, "Carpathians ridge", member, other)
	h.seedRoom(somebodyElses, "Not your trip", other)

	testsupport.SeedMessage(t, h.pool, busy, other, "first")
	lastID := testsupport.SeedMessage(t, h.pool, busy, other, "second")

	type roomRow struct {
		TripID      uuid.UUID `json:"trip_id"`
		Title       string    `json:"title"`
		Status      string    `json:"status"`
		IsOpen      bool      `json:"is_open"`
		UnreadCount int       `json:"unread_count"`
		LastMessage *struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		} `json:"last_message"`
	}
	var body struct {
		Items      []roomRow `json:"items"`
		NextCursor *string   `json:"next_cursor"`
	}

	resp := h.primary.do(http.MethodGet, "/api/chat/rooms", member, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	decode(t, resp, &body)

	// Only the caller's rooms, and the one with the newest message first.
	require.Len(t, body.Items, 2)
	require.Nil(t, body.NextCursor)
	require.Equal(t, busy, body.Items[0].TripID)
	require.Equal(t, quiet, body.Items[1].TripID)

	require.NotNil(t, body.Items[0].LastMessage)
	require.Equal(t, "second", body.Items[0].LastMessage.Body)
	require.Equal(t, lastID, body.Items[0].LastMessage.ID)
	require.Equal(t, 2, body.Items[0].UnreadCount)
	require.True(t, body.Items[0].IsOpen)

	// A room nobody has spoken in has a null last message rather than a
	// missing key, so a client can render it without a guard.
	require.Nil(t, body.Items[1].LastMessage)
	require.Equal(t, 0, body.Items[1].UnreadCount)
}

// The unread count is what the `read` frame moves, and it excludes the caller's
// own messages — a room is not unread because you were the last to speak in it.
func TestUnreadCountFollowsTheReadWatermark(t *testing.T) {
	h := newHarness(t)

	tripID := uuid.New()
	reader, writer := uuid.New(), uuid.New()
	h.seedRoom(tripID, "Unread", reader, writer)

	testsupport.SeedMessage(t, h.pool, tripID, writer, "one")
	second := testsupport.SeedMessage(t, h.pool, tripID, writer, "two")
	testsupport.SeedMessage(t, h.pool, tripID, writer, "three")
	testsupport.SeedMessage(t, h.pool, tripID, reader, "mine")

	require.Equal(t, 3, h.unreadCount(reader, tripID), "own message must not count as unread")

	socket := h.primary.dial(reader, tripID)
	socket.send(map[string]any{"type": "read", "last_message_id": second})

	require.Eventually(t, func() bool {
		return h.unreadCount(reader, tripID) == 1
	}, 5*time.Second, 50*time.Millisecond)

	// The watermark only advances. A second tab scrolled further back must not
	// bring the badge back.
	socket.send(map[string]any{"type": "read", "last_message_id": int64(1)})
	socket.sendMessage("round trip", "client-1")
	socket.awaitMessage("round trip")
	require.Equal(t, 1, h.unreadCount(reader, tripID))
}

func TestListMessages(t *testing.T) {
	h := newHarness(t)

	tripID := uuid.New()
	member, stranger := uuid.New(), uuid.New()
	h.seedRoom(tripID, "History", member)

	var ids []int64
	for i := 1; i <= 5; i++ {
		ids = append(ids, testsupport.SeedMessage(t, h.pool, tripID, member, "message "+strconv.Itoa(i)))
	}

	type messageRow struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	type pageBody struct {
		Items      []messageRow `json:"items"`
		NextCursor *string      `json:"next_cursor"`
	}

	t.Run("newest first, with a cursor when there is more", func(t *testing.T) {
		var body pageBody
		resp := h.primary.do(http.MethodGet, "/api/chat/rooms/"+tripID.String()+"/messages?limit=2", member, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		decode(t, resp, &body)

		require.Len(t, body.Items, 2)
		require.Equal(t, "message 5", body.Items[0].Body)
		require.Equal(t, "message 4", body.Items[1].Body)
		require.NotNil(t, body.NextCursor)

		// The cursor pages backwards, and the pages do not overlap.
		var next pageBody
		resp = h.primary.do(http.MethodGet,
			"/api/chat/rooms/"+tripID.String()+"/messages?limit=2&cursor="+*body.NextCursor, member, nil)
		decode(t, resp, &next)
		require.Equal(t, "message 3", next.Items[0].Body)
		require.Equal(t, "message 2", next.Items[1].Body)
	})

	t.Run("before_id is the readable spelling of the same position", func(t *testing.T) {
		var body pageBody
		resp := h.primary.do(http.MethodGet,
			"/api/chat/rooms/"+tripID.String()+"/messages?before_id="+strconv.FormatInt(ids[2], 10), member, nil)
		decode(t, resp, &body)

		require.Len(t, body.Items, 2)
		require.Equal(t, "message 2", body.Items[0].Body)
		require.Equal(t, "message 1", body.Items[1].Body)
		// The last page has no cursor rather than a cursor onto nothing.
		require.Nil(t, body.NextCursor)
	})

	t.Run("before_id and cursor together are a client bug, not a precedence rule", func(t *testing.T) {
		resp := h.primary.do(http.MethodGet,
			"/api/chat/rooms/"+tripID.String()+"/messages?before_id=3&cursor=Mw", member, nil)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Equal(t, "validation_error", errorCode(t, resp))
	})

	t.Run("a non-member gets 403", func(t *testing.T) {
		resp := h.primary.do(http.MethodGet, "/api/chat/rooms/"+tripID.String()+"/messages", stranger, nil)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "not_member", errorCode(t, resp))
	})

	t.Run("a room the projection has not seen is 404", func(t *testing.T) {
		resp := h.primary.do(http.MethodGet, "/api/chat/rooms/"+uuid.NewString()+"/messages", member, nil)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
		require.Equal(t, "room_not_found", errorCode(t, resp))
	})
}

// A cancelled trip's room is readable and not writable, and that split is the
// whole of what `trip.cancelled` does here.
func TestACancelledRoomIsReadOnly(t *testing.T) {
	h := newHarness(t)

	tripID := uuid.New()
	member := uuid.New()
	h.seedRoom(tripID, "Called off", member)
	testsupport.SeedMessage(t, h.pool, tripID, member, "see you on saturday")
	testsupport.SeedRoom(t, h.pool, tripID, "Called off", "cancelled")

	// History still reads.
	resp := h.primary.do(http.MethodGet, "/api/chat/rooms/"+tripID.String()+"/messages", member, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// And a ticket is still issued, because the socket is still useful for
	// reading.
	socket := h.primary.dial(member, tripID)

	// But nothing new can be said.
	socket.sendMessage("one more thing", "client-1")
	refusal := socket.await(10*time.Second, "a room_closed error frame", func(f frame) bool {
		return f.Type == "error"
	})
	require.Equal(t, "room_closed", refusal.Code)
}
