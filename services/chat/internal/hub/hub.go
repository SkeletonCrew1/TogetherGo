package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/togethergo/chat/internal/domain"
)

// maxFrameBytes is the read limit on every socket.
//
// A message body is capped at 2000 characters, which is 8000 bytes at the worst
// UTF-8 expansion, plus a little JSON around it. Sixteen kibibytes is
// comfortably above that and far below anything that would matter as an
// allocation. Without a limit, coder/websocket buffers whatever arrives, and one
// client could hand this process a gigabyte.
const maxFrameBytes = 16 << 10

// Store is the hub's half of the data layer. An interface rather than
// *store.Store so that this package stays testable without a database and so
// the dependency reads as "the two writes a socket can cause".
type Store interface {
	InsertMessage(ctx context.Context, tripID, senderID uuid.UUID, body string) (domain.Message, error)
	MarkRead(ctx context.Context, tripID, userID uuid.UUID, lastMessageID int64) error
}

// Bus is the cross-replica fan-out. See internal/realtime.
type Bus interface {
	Publish(ctx context.Context, tripID uuid.UUID, payload []byte) error
	Subscribe(ctx context.Context, tripID uuid.UUID) error
	Unsubscribe(ctx context.Context, tripID uuid.UUID) error
	Run(ctx context.Context, deliver func(tripID uuid.UUID, payload []byte))
}

// Presence is who is in a room, across replicas. See internal/realtime.
type Presence interface {
	Touch(ctx context.Context, tripID, userID uuid.UUID) (bool, error)
	Forget(ctx context.Context, tripID, userID uuid.UUID) (bool, error)
	Sweep(ctx context.Context, tripID uuid.UUID) ([]uuid.UUID, error)
	Online(ctx context.Context, tripID uuid.UUID) ([]uuid.UUID, error)
}

// Limiter caps how fast one user may write to one room. See internal/realtime.
type Limiter interface {
	Allow(ctx context.Context, tripID, userID uuid.UUID) (bool, error)
}

// Options is everything the hub needs. Passed in rather than constructed here
// so that main owns every lifetime.
type Options struct {
	Store    Store
	Bus      Bus
	Presence Presence
	Limiter  Limiter
	Logger   *slog.Logger

	SendBuffer     int
	PingInterval   time.Duration
	PongTimeout    time.Duration
	AllowedOrigins []string
}

// Hub is this instance's view of the world: the sockets it holds, grouped by
// room.
//
// It is one of many. Every replica has its own, holding only the connections
// that happened to land on it, and they are joined by the Redis fan-out — which
// is why nothing in this struct is a source of truth about anything. Who is in
// a room lives in Postgres; who is *online* lives in Redis; this map is only
// "which file descriptors do I have to write to".
type Hub struct {
	store    Store
	bus      Bus
	presence Presence
	limiter  Limiter
	logger   *slog.Logger

	sendBuffer     int
	pingInterval   time.Duration
	pongTimeout    time.Duration
	allowedOrigins []string

	// mu guards rooms and is taken on the delivery path, so nothing slow
	// happens under it: deliver copies the connection set and releases.
	mu    sync.RWMutex
	rooms map[uuid.UUID]map[*Conn]struct{}

	// subMu guards subscribed, and unlike mu it *is* held across a network
	// call — the SUBSCRIBE or UNSUBSCRIBE to Redis.
	//
	// Two locks rather than one because the two have opposite requirements.
	// Subscription changes must be serialised with the refcount that decides
	// them, or a socket closing and another opening in the same room can
	// interleave into "no subscription, one live connection", which is a room
	// that silently stops delivering. Deliveries must never wait for a network
	// call. Neither lock is ever taken while the other is held.
	subMu      sync.Mutex
	subscribed map[uuid.UUID]int
}

// New builds a hub.
func New(opts Options) *Hub {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		store:          opts.Store,
		bus:            opts.Bus,
		presence:       opts.Presence,
		limiter:        opts.Limiter,
		logger:         logger,
		sendBuffer:     opts.SendBuffer,
		pingInterval:   opts.PingInterval,
		pongTimeout:    opts.PongTimeout,
		allowedOrigins: opts.AllowedOrigins,
		rooms:          map[uuid.UUID]map[*Conn]struct{}{},
		subscribed:     map[uuid.UUID]int{},
	}
}

// Run drives the two background loops — the fan-out reader and the presence
// heartbeat — until ctx is cancelled.
func (h *Hub) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.bus.Run(ctx, h.deliver)
	}()

	h.presenceLoop(ctx)
	wg.Wait()
}

// Shutdown closes every socket with 1001 Going Away.
//
// 1001 rather than 1000: "the server is going away" is exactly what has
// happened, and browsers and client libraries treat it as a reconnect-worthy
// close, where a 1000 reads as "we are done here, do not come back". A rolling
// deploy depends on that difference.
func (h *Hub) Shutdown() {
	h.mu.RLock()
	var conns []*Conn
	for _, room := range h.rooms {
		for conn := range room {
			conns = append(conns, conn)
		}
	}
	h.mu.RUnlock()

	for _, conn := range conns {
		conn.closeWith(websocket.StatusGoingAway, "server shutting down")
	}
	if len(conns) > 0 {
		h.logger.Info("closed open sockets for shutdown", slog.Int("count", len(conns)))
	}
}

// Reject completes the handshake and immediately closes with a code.
//
// A refusal before the upgrade would be an HTTP status, and a browser cannot
// read one: `WebSocket.onerror` fires, `onclose` reports 1006, and the page has
// no way to tell an expired ticket from a dropped network. Accepting and
// closing costs one round trip and gives the client a number it can act on —
// 4401 means "fetch another ticket", 4403 means "stop trying". Nothing is
// registered with the hub and nothing is read from the socket, so a rejected
// connection touches no state at all.
func (h *Hub) Reject(w http.ResponseWriter, r *http.Request, code websocket.StatusCode, reason string) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.allowedOrigins})
	if err != nil {
		// The upgrade itself failed — a non-websocket request, or an origin
		// that is not allowed. coder/websocket has already written a status.
		h.logger.Debug("websocket upgrade refused", slog.String("error", err.Error()))
		return
	}
	_ = ws.Close(code, reason)
}

// Serve takes over an authenticated connection and blocks until it closes.
//
// The caller has already redeemed the ticket and re-checked membership against
// the database; this function trusts the pair it is given and nothing else.
func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, tripID, userID uuid.UUID) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.allowedOrigins})
	if err != nil {
		h.logger.Debug("websocket upgrade failed", slog.String("error", err.Error()))
		return
	}
	ws.SetReadLimit(maxFrameBytes)

	// Detached from the request context on purpose. net/http cancels that
	// context when the handler returns, and for a hijacked connection some
	// server configurations cancel it at the upgrade; either way the socket
	// outlives the request, and its lifetime is the one below.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	conn := &Conn{
		ws:     ws,
		TripID: tripID,
		UserID: userID,
		send:   make(chan []byte, h.sendBuffer),
		closed: make(chan struct{}),
		logger: h.logger,
	}

	if err := h.register(ctx, conn); err != nil {
		// Registration only fails when Redis will not take the subscription,
		// and a socket that is not subscribed would show the user their own
		// messages and nobody else's — worse than no socket at all.
		h.logger.Error("could not subscribe to a room, refusing the socket",
			slog.String("trip_id", tripID.String()),
			slog.String("error", err.Error()))
		_ = ws.Close(websocket.StatusInternalError, "fan-out unavailable")
		return
	}
	defer h.unregister(context.WithoutCancel(ctx), conn)

	h.logger.Info("socket opened",
		slog.String("trip_id", tripID.String()),
		slog.String("user_id", userID.String()))

	h.announceArrival(ctx, conn)

	go conn.writeLoop(ctx, h.pingInterval, h.pongTimeout)
	h.readLoop(ctx, conn)

	conn.closeWith(websocket.StatusNormalClosure, "")
	h.logger.Info("socket closed",
		slog.String("trip_id", tripID.String()),
		slog.String("user_id", userID.String()))
}

// register subscribes to the room if this is the first local socket for it,
// then adds the connection.
//
// Subscribe first, add second. The other order leaves a window in which the
// connection is in the map but the instance is not receiving the room's traffic,
// and a message published in that window would be missed with nothing to
// indicate it. This order's window is the harmless one: the instance receives
// traffic for a connection that is not registered yet, and the delivery finds an
// empty set.
func (h *Hub) register(ctx context.Context, conn *Conn) error {
	h.subMu.Lock()
	count := h.subscribed[conn.TripID]
	if count == 0 {
		if err := h.bus.Subscribe(ctx, conn.TripID); err != nil {
			h.subMu.Unlock()
			return err
		}
	}
	h.subscribed[conn.TripID] = count + 1
	h.subMu.Unlock()

	h.mu.Lock()
	room, ok := h.rooms[conn.TripID]
	if !ok {
		room = map[*Conn]struct{}{}
		h.rooms[conn.TripID] = room
	}
	room[conn] = struct{}{}
	h.mu.Unlock()
	return nil
}

// unregister is register in reverse, plus the presence departure.
func (h *Hub) unregister(ctx context.Context, conn *Conn) {
	h.mu.Lock()
	if room, ok := h.rooms[conn.TripID]; ok {
		delete(room, conn)
		if len(room) == 0 {
			delete(h.rooms, conn.TripID)
		}
	}
	h.mu.Unlock()

	h.announceDeparture(ctx, conn)

	h.subMu.Lock()
	count := h.subscribed[conn.TripID] - 1
	if count <= 0 {
		delete(h.subscribed, conn.TripID)
		if err := h.bus.Unsubscribe(ctx, conn.TripID); err != nil {
			h.logger.Warn("could not unsubscribe from a room",
				slog.String("trip_id", conn.TripID.String()),
				slog.String("error", err.Error()))
		}
	} else {
		h.subscribed[conn.TripID] = count
	}
	h.subMu.Unlock()
}

// deliver is the fan-out's entry point, called on the bus's single goroutine.
//
// It must not block, which is why the lock is released before anything is
// written and why enqueue is a non-blocking send. One unresponsive socket in one
// room would otherwise stall every room on this instance.
func (h *Hub) deliver(tripID uuid.UUID, payload []byte) {
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		h.logger.Warn("dropping an unreadable fan-out message",
			slog.String("trip_id", tripID.String()),
			slog.String("error", err.Error()))
		return
	}

	h.mu.RLock()
	conns := make([]*Conn, 0, len(h.rooms[tripID]))
	for conn := range h.rooms[tripID] {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()

	for _, conn := range conns {
		switch {
		case env.Evict != nil:
			if conn.UserID == *env.Evict {
				conn.send1(newErrorFrame(CodeNotMember, "You are no longer a member of this trip."))
				conn.closeWith(CloseNotMember, "membership revoked")
			}
		case env.Except != nil && conn.UserID == *env.Except:
			// The author of a typing indicator does not need it back.
		default:
			conn.enqueue(env.Frame)
		}
	}
}

// publish encodes an envelope and puts it on the room's channel.
func (h *Hub) publish(ctx context.Context, tripID uuid.UUID, env envelope) {
	payload, err := json.Marshal(env)
	if err != nil {
		h.logger.Error("encoding a fan-out envelope failed", slog.String("error", err.Error()))
		return
	}
	if err := h.bus.Publish(ctx, tripID, payload); err != nil {
		h.logger.Error("publishing to the room fan-out failed",
			slog.String("trip_id", tripID.String()),
			slog.String("error", err.Error()))
	}
}

// broadcast publishes one frame to a whole room.
func (h *Hub) broadcast(ctx context.Context, tripID uuid.UUID, frame any, except *uuid.UUID) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		h.logger.Error("encoding an outbound frame failed", slog.String("error", err.Error()))
		return
	}
	h.publish(ctx, tripID, envelope{Frame: encoded, Except: except})
}

// Evict closes every socket this user holds in this room, on every replica.
//
// Called by the `participant.removed` handler after its transaction commits.
// The consumer runs on one instance and the user's socket may be on another, so
// the message goes through the same fan-out everything else does rather than
// through any kind of instance-to-instance addressing.
func (h *Hub) Evict(ctx context.Context, tripID, userID uuid.UUID) {
	h.publish(ctx, tripID, envelope{Evict: &userID})
}

// readLoop is the reader goroutine: the only thing that reads from the socket.
//
// It runs on the HTTP handler's own goroutine rather than a new one, which is
// what makes "Serve blocks until the connection closes" true and keeps the
// deferred unregister in the right place.
func (h *Hub) readLoop(ctx context.Context, conn *Conn) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-conn.closed:
			return
		default:
		}

		_, data, err := conn.ws.Read(ctx)
		if err != nil {
			// Every connection ends here, and almost all of them end normally:
			// a closed tab, a navigation, a dropped network. Debug, not error.
			h.logger.Debug("socket read ended",
				slog.String("trip_id", conn.TripID.String()),
				slog.String("error", err.Error()))
			return
		}

		var frame inboundFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			// The connection stays open. A client that sends one malformed
			// frame is a client with a bug, not an attacker, and killing its
			// socket would cost the user the conversation.
			conn.send1(newErrorFrame(CodeInvalidFrame, "That frame is not valid JSON."))
			continue
		}

		switch frame.Type {
		case frameMessage:
			h.handleMessage(ctx, conn, frame)
		case frameTyping:
			h.broadcast(ctx, conn.TripID, TypingFrame{Type: frameTyping, UserID: conn.UserID}, &conn.UserID)
		case frameRead:
			h.handleRead(ctx, conn, frame)
		default:
			conn.send1(newErrorFrame(CodeUnsupportedFrame, "That frame type is not supported."))
		}
	}
}

// handleMessage validates, rate limits, persists and fans out one message, in
// that order.
//
// The order is the interesting part. Validation is free and local, so it comes
// first. The rate limit comes before the database so that a client hammering
// the socket costs one Redis INCR rather than one INSERT. Persistence comes
// before the fan-out, always, so that no client ever sees a message that is not
// in the database — a message rendered and then lost is worse than one that
// takes another millisecond to appear.
func (h *Hub) handleMessage(ctx context.Context, conn *Conn, frame inboundFrame) {
	body, err := domain.CleanBody(frame.Body)
	if err != nil {
		var validation *domain.ValidationError
		if errors.As(err, &validation) {
			conn.send1(newErrorFrame(CodeValidationError, validation.Error()))
			return
		}
		conn.send1(newErrorFrame(CodeValidationError, "That message cannot be sent."))
		return
	}

	allowed, err := h.limiter.Allow(ctx, conn.TripID, conn.UserID)
	if err != nil {
		// Allow fails open, so this is a log line and not a refusal.
		h.logger.Warn("rate limit check failed, allowing the message",
			slog.String("trip_id", conn.TripID.String()),
			slog.String("error", err.Error()))
	}
	if !allowed {
		// An error frame and a dropped message, never a closed connection: the
		// user is typing too fast, which is a thing users do.
		conn.send1(newErrorFrame(CodeRateLimited, "You are sending messages too quickly. Wait a moment and try again."))
		return
	}

	message, err := h.store.InsertMessage(ctx, conn.TripID, conn.UserID, body)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrRoomClosed):
			// Readable, not writable. The socket stays open so the user can
			// keep reading the history of the trip that was called off.
			conn.send1(newErrorFrame(CodeRoomClosed, "This trip has been cancelled; the room is read-only."))
		case errors.Is(err, domain.ErrNotMember), errors.Is(err, domain.ErrRoomNotFound):
			// The membership went away underneath an open socket and the
			// eviction has not reached this instance yet, or never will.
			conn.send1(newErrorFrame(CodeNotMember, "You are no longer a member of this trip."))
			conn.closeWith(CloseNotMember, "membership revoked")
		default:
			h.logger.Error("persisting a message failed",
				slog.String("trip_id", conn.TripID.String()),
				slog.String("error", err.Error()))
			conn.send1(newErrorFrame(CodeInternalError, "The message could not be sent. Try again."))
		}
		return
	}

	// The sender's own copy comes back through Redis like everybody else's.
	// One path, one ordering: writing it locally first would let the sender see
	// their message before a message that was persisted earlier, and would be a
	// second place for this frame to be assembled.
	h.broadcast(ctx, conn.TripID,
		newMessageFrame(message.ID, frame.ClientMsgID, conn.TripID, conn.UserID, message.Body, message.CreatedAt),
		nil)
}

// handleRead moves the caller's read watermark. Nothing is fanned out: read
// receipts are not part of this service's frame vocabulary, and the unread
// count they feed is only ever read back by the same user.
func (h *Hub) handleRead(ctx context.Context, conn *Conn, frame inboundFrame) {
	if frame.LastMessageID <= 0 {
		conn.send1(newErrorFrame(CodeValidationError, "last_message_id must be a positive message id."))
		return
	}
	if err := h.store.MarkRead(ctx, conn.TripID, conn.UserID, frame.LastMessageID); err != nil {
		h.logger.Warn("marking a room read failed",
			slog.String("trip_id", conn.TripID.String()),
			slog.String("error", err.Error()))
	}
}
