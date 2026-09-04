package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/auth"
	httpapi "github.com/togethergo/chat/internal/http"
	"github.com/togethergo/chat/internal/hub"
	"github.com/togethergo/chat/internal/realtime"
	"github.com/togethergo/chat/internal/store"
	"github.com/togethergo/chat/internal/testsupport"
)

// The tests in this package run the real router over a real Postgres, a real
// Redis and real websockets. Nothing between the request and the database is
// faked.
//
// That is not thoroughness for its own sake. The three things this service is
// actually made of — single-use tickets, cross-replica fan-out and a rate
// limiter — are all properties of Redis commands, and a mock would only prove
// that the mock agrees with the code written against it. The acceptance
// criteria are statements about what two browser tabs see, so the tests are
// statements about what two sockets see.

// replica is one instance of the chat service: its own hub, its own HTTP
// server, sharing the database and the Redis with every other.
//
// Two of them is how `docker compose up --scale chat=2` is tested without
// Docker: the fan-out either works between two hubs on one Redis or it does
// not, and the number of processes involved is not what makes that true.
type replica struct {
	t        *testing.T
	server   *httptest.Server
	identity *testsupport.Identity
	pool     *pgxpool.Pool
	redis    *goredis.Client
}

// keepalive is the ping/pong pair a harness builds its hubs with.
//
// Configurable because the two things worth testing about it want opposite
// values. Every other test wants them short, so that a socket which *should*
// die does so before the test's own deadline; the idle-connection test wants
// the production pair, because "does a healthy socket survive its pong timeout"
// is only a real question at the durations the service actually ships with.
type keepalive struct {
	pingInterval time.Duration
	pongTimeout  time.Duration
	presenceTTL  time.Duration
}

func defaultKeepalive() keepalive {
	return keepalive{
		pingInterval: time.Second,
		pongTimeout:  5 * time.Second,
		presenceTTL:  time.Minute,
	}
}

// harness owns the shared backing services and the replicas over them.
type harness struct {
	t        *testing.T
	pool     *pgxpool.Pool
	redis    *goredis.Client
	identity *testsupport.Identity

	keepalive keepalive

	primary *replica
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, defaultKeepalive())
}

func newHarnessWith(t *testing.T, ka keepalive) *harness {
	t.Helper()

	h := &harness{
		t:         t,
		pool:      testsupport.Pool(t),
		redis:     testsupport.Redis(t),
		identity:  testsupport.NewIdentity(t),
		keepalive: ka,
	}
	h.primary = h.addReplica()
	return h
}

// addReplica starts another instance against the same database and Redis.
func (h *harness) addReplica() *replica {
	h.t.Helper()

	logger := httpapi.NewLogger("error")
	st := store.New(h.pool)

	bus := realtime.NewBus(h.redis, logger)
	rooms := hub.New(hub.Options{
		Store:    st,
		Bus:      bus,
		Presence: realtime.NewPresence(h.redis, h.keepalive.presenceTTL),
		Limiter:  realtime.NewLimiter(h.redis, 20, 10*time.Second),
		Logger:   logger,
		// Deliberately small. A test that wants to prove a slow client is
		// disconnected should not have to send sixty-four messages to do it.
		SendBuffer:   8,
		PingInterval: h.keepalive.pingInterval,
		PongTimeout:  h.keepalive.pongTimeout,
		// The websocket client below sends no Origin header, so this is only
		// here to say that the check is not what a failing test is about.
		AllowedOrigins: []string{"*"},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rooms.Run(ctx)
	}()

	server := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Store:    st,
		Verifier: auth.NewVerifier(auth.NewKeySet(h.identity.JWKSURL, time.Minute, nil)),
		Tickets:  realtime.NewTickets(h.redis, 30*time.Second),
		Hub:      rooms,
		Logger:   logger,
	}))

	h.t.Cleanup(func() {
		rooms.Shutdown()
		server.Close()
		cancel()
		<-done
	})

	return &replica{
		t:        h.t,
		server:   server,
		identity: h.identity,
		pool:     h.pool,
		redis:    h.redis,
	}
}

// do issues an authenticated request, or an unauthenticated one when user is
// uuid.Nil.
func (r *replica) do(method, path string, user uuid.UUID, body any) *http.Response {
	r.t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		require.NoError(r.t, err)
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, r.server.URL+path, reader)
	require.NoError(r.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user != uuid.Nil {
		req.Header.Set("Authorization", "Bearer "+r.identity.Token(r.t, user))
	}

	resp, err := r.server.Client().Do(req)
	require.NoError(r.t, err)
	r.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// decode reads a JSON response body into target.
func decode(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, target), "response body was %s", body)
}

// errorCode reads the `error.code` out of a failure response, so a test asserts
// on the code rather than on the message text — which is exactly what a client
// is required to do.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	decode(t, resp, &envelope)
	return envelope.Error.Code
}

// ticket asks for a websocket ticket the way the SPA does.
func (r *replica) ticket(user, tripID uuid.UUID) string {
	r.t.Helper()

	resp := r.do(http.MethodPost, "/api/chat/tickets", user, map[string]string{"trip_id": tripID.String()})
	require.Equal(r.t, http.StatusCreated, resp.StatusCode)

	var body struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
	}
	decode(r.t, resp, &body)
	require.NotEmpty(r.t, body.Ticket)
	require.Equal(r.t, 30, body.ExpiresIn)
	return body.Ticket
}

// socket is one connected client.
//
// Frames are pumped off the connection by a goroutine into a buffered channel
// rather than read on demand, and that is not a stylistic choice.
// coder/websocket closes the whole connection when a Read's context expires —
// the frame stream is in an unknown state at that point, so there is nothing
// else it could do — which means a test that waits for a frame with a deadline
// and does not get one has destroyed the socket it was about to assert on. With
// a pump, "nothing arrived within 500ms" is a timer on a channel and the
// connection is untouched.
type socket struct {
	t    *testing.T
	ws   *websocket.Conn
	trip uuid.UUID

	frames chan frame

	// closed is closed by the pump when the connection ends; readErr is
	// written before that, so reading it after <-closed is safe.
	closed  chan struct{}
	readErr error
}

// pump reads the connection until it closes.
func (s *socket) pump() {
	defer close(s.closed)
	for {
		_, data, err := s.ws.Read(context.Background())
		if err != nil {
			s.readErr = err
			return
		}
		var f frame
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		s.frames <- f
	}
}

// wsURL turns the test server's http URL into a websocket one.
func (r *replica) wsURL(tripID uuid.UUID, ticket string) string {
	return strings.Replace(r.server.URL, "http://", "ws://", 1) +
		"/ws/chat/" + tripID.String() + "?ticket=" + ticket
}

// dial opens a socket with a freshly issued ticket.
func (r *replica) dial(user, tripID uuid.UUID) *socket {
	r.t.Helper()
	return r.dialWithTicket(tripID, r.ticket(user, tripID))
}

// dialWithTicket opens a socket with whatever ticket it is given — including
// one that has already been spent, or one that was never issued.
func (r *replica) dialWithTicket(tripID uuid.UUID, ticket string) *socket {
	r.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, r.wsURL(tripID, ticket), nil)
	require.NoError(r.t, err, "websocket handshake failed")

	s := &socket{
		t:      r.t,
		ws:     ws,
		trip:   tripID,
		frames: make(chan frame, 256),
		closed: make(chan struct{}),
	}
	go s.pump()

	r.t.Cleanup(func() { _ = ws.CloseNow() })
	return s
}

// frame is every server-to-client frame, decoded in one pass. The union is
// small enough that one struct is clearer than a type switch.
type frame struct {
	Type        string    `json:"type"`
	ID          int64     `json:"id"`
	ClientMsgID string    `json:"client_msg_id"`
	TripID      uuid.UUID `json:"trip_id"`
	SenderID    uuid.UUID `json:"sender_id"`
	Body        string    `json:"body"`
	CreatedAt   string    `json:"created_at"`
	UserID      uuid.UUID `json:"user_id"`
	Status      string    `json:"status"`
	Code        string    `json:"code"`
	Message     string    `json:"message"`
}

// send writes a client frame.
func (s *socket) send(v any) {
	s.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	encoded, err := json.Marshal(v)
	require.NoError(s.t, err)
	require.NoError(s.t, s.ws.Write(ctx, websocket.MessageText, encoded))
}

// sendMessage is the frame a composer sends, with a client_msg_id the caller
// can match the echo against.
func (s *socket) sendMessage(body, clientMsgID string) {
	s.t.Helper()
	s.send(map[string]any{"type": "message", "client_msg_id": clientMsgID, "body": body})
}

// await reads frames until one satisfies want, or fails after timeout.
//
// Filtering rather than taking the next frame is what keeps these tests from
// being order-dependent: presence frames arrive on their own schedule, and a
// test about messages should not break because a heartbeat landed first.
func (s *socket) await(timeout time.Duration, what string, want func(frame) bool) frame {
	s.t.Helper()

	deadline := time.After(timeout)
	var seen []string
	for {
		select {
		case f := <-s.frames:
			seen = append(seen, f.Type)
			if want(f) {
				return f
			}
		case <-s.closed:
			s.t.Fatalf("socket closed while waiting for %s: %v (frames seen: %v)", what, s.readErr, seen)
		case <-deadline:
			s.t.Fatalf("timed out waiting for %s (frames seen: %v)", what, seen)
		}
	}
}

// awaitMessage waits for a `message` frame with the given body.
func (s *socket) awaitMessage(body string) frame {
	s.t.Helper()
	return s.await(10*time.Second, "message "+body, func(f frame) bool {
		return f.Type == "message" && f.Body == body
	})
}

// awaitClose waits for the connection to close and returns the close code, so a
// test can assert on 4401 rather than on "it did not work".
func (s *socket) awaitClose(timeout time.Duration) websocket.StatusCode {
	s.t.Helper()

	select {
	case <-s.closed:
		return websocket.CloseStatus(s.readErr)
	case <-time.After(timeout):
		s.t.Fatal("timed out waiting for the socket to close")
		return 0
	}
}

// expectNoFrame asserts that no matching frame arrives within the window, and
// leaves the connection open.
//
// Used where the assertion is an absence — a rate-limited message that must not
// reach the room, a typing indicator that must not be echoed to its author. The
// window passing is the pass condition, which is why this is a timer on the
// pump's channel and not a Read with a deadline.
func (s *socket) expectNoFrame(window time.Duration, matches func(frame) bool) {
	s.t.Helper()

	deadline := time.After(window)
	for {
		select {
		case f := <-s.frames:
			if matches(f) {
				s.t.Fatalf("did not expect a %s frame, but got one: %+v", f.Type, f)
			}
		case <-s.closed:
			s.t.Fatalf("socket closed unexpectedly: %v", s.readErr)
		case <-deadline:
			return
		}
	}
}

// unreadCount reads the unread badge the way the room list computes it, so a
// test can assert on it without paging through an endpoint.
func (h *harness) unreadCount(userID, tripID uuid.UUID) int {
	h.t.Helper()

	var count int
	require.NoError(h.t, h.pool.QueryRow(context.Background(), `
		SELECT count(*)
		FROM messages m
		JOIN room_members rm ON rm.trip_id = m.trip_id AND rm.user_id = $1
		WHERE m.trip_id = $2 AND m.id > rm.last_read_message_id AND m.sender_id <> rm.user_id`,
		userID, tripID).Scan(&count))
	return count
}

// seedRoom provisions a room with its members, standing in for the events that
// would have created it. The consumer tests cover that path; these cover what
// happens once it has run.
func (h *harness) seedRoom(tripID uuid.UUID, title string, members ...uuid.UUID) {
	h.t.Helper()
	testsupport.SeedRoom(h.t, h.pool, tripID, title, "recruiting")
	for _, member := range members {
		testsupport.SeedMember(h.t, h.pool, tripID, member)
	}
}

// expectStillOpen holds the connection for d and fails if it ends first.
//
// Frames arriving are not a failure — a presence heartbeat rides the same
// interval as the keepalive — so they are drained and ignored. The assertion is
// only that nothing closed the socket.
func (s *socket) expectStillOpen(d time.Duration) {
	s.t.Helper()

	deadline := time.After(d)
	for {
		select {
		case <-s.frames:
		case <-s.closed:
			s.t.Fatalf("the socket closed inside %s with no client action: %v", d, s.readErr)
		case <-deadline:
			return
		}
	}
}
