package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Close codes this service uses on top of the standard ones.
//
// 4000–4999 is the range RFC 6455 reserves for applications, and a browser can
// read the code out of the `close` event — which is the reason to use them at
// all. A socket that simply drops gives the client 1006 and no way to tell "your
// ticket expired, ask for another" from "the wifi went".
const (
	// CloseTicketInvalid is the ticket being missing, expired, already spent or
	// issued for a different trip. The client's move is to fetch a new ticket
	// and reconnect; if that fails too, its access token is the problem.
	CloseTicketInvalid = 4401

	// CloseNotMember is a caller who is not on the trip, or who was removed
	// while connected. Reconnecting will not help and the client should stop.
	CloseNotMember = 4403

	// CloseRoomNotReady is the room projection not having caught up. This one
	// *is* worth retrying, and shortly: the event is on its way.
	CloseRoomNotReady = 4404

	// CloseBadRequest is a malformed handshake — a trip id that is not a uuid.
	CloseBadRequest = 4400
)

// writeTimeout bounds one write to one socket.
//
// It is the second half of the slow-client defence. The buffered send channel
// catches a client that is not reading fast enough; this catches one whose TCP
// window has closed entirely, where a single Write would otherwise block the
// writer goroutine — and therefore that connection's whole outbound path —
// indefinitely.
const writeTimeout = 10 * time.Second

// Conn is one open websocket, and the two goroutines that own it.
//
// The division is strict and it is the reason the hub can be lock-light: the
// *writer* goroutine is the only thing that ever writes to the socket, and
// everything else — the reader, the fan-out, the shutdown — reaches it through
// `send`. Nothing outside this file writes to the connection, so there is no
// interleaved-frame bug to have.
type Conn struct {
	ws     *websocket.Conn
	TripID uuid.UUID
	UserID uuid.UUID

	// send is the writer's inbox. Buffered: a burst in a busy room must not
	// make the fan-out wait on the slowest socket in it.
	send chan []byte

	// closed is closed once, by close(), to stop both goroutines. A channel
	// rather than a flag because both goroutines select on it.
	closed    chan struct{}
	closeOnce sync.Once

	logger *slog.Logger
}

// enqueue hands a pre-encoded frame to the writer, or gives up on the client.
//
// This is the "a slow client must not stall the room" rule, and it is a
// non-blocking send for exactly that reason. If the buffer is full the client
// has failed to read `SendBuffer` frames while everyone else kept up, and the
// choice is between blocking the fan-out goroutine — which serves every room on
// this instance — and dropping one connection. Dropping the connection is not
// even lossy from the user's point of view: their client reconnects and
// refetches history, which is the same thing it does after any network blip.
func (c *Conn) enqueue(payload []byte) {
	select {
	case c.send <- payload:
	case <-c.closed:
	default:
		c.logger.Info("closing a socket that cannot keep up",
			slog.String("trip_id", c.TripID.String()),
			slog.String("user_id", c.UserID.String()),
			slog.Int("buffered", len(c.send)),
		)
		c.closeWith(websocket.StatusPolicyViolation, "client is not reading fast enough")
	}
}

// send1 encodes a frame and enqueues it. Used for the frames that go to one
// socket rather than to a room: errors, and the presence snapshot a connecting
// client gets.
func (c *Conn) send1(frame any) {
	payload, err := json.Marshal(frame)
	if err != nil {
		c.logger.Error("encoding an outbound frame failed", slog.String("error", err.Error()))
		return
	}
	c.enqueue(payload)
}

// closeWith closes the socket once, with a code the client can read.
//
// The Close is best-effort and its error is discarded on purpose: every caller
// is already on a path that ends with this connection gone, and a failure here
// only means the peer will see a dropped TCP connection instead of a close
// frame.
func (c *Conn) closeWith(status websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.ws.Close(status, reason)
	})
}

// writeLoop is the writer goroutine: the only thing that writes to the socket.
//
// It also owns the keepalive, which is why the ping is here rather than in a
// third goroutine. Ping blocks until the pong comes back, so on a dead
// connection it blocks for the whole pong timeout — which is harmless, because
// the connection is about to be closed anyway — and on a live one it returns in
// under a millisecond. What it must not do is run concurrently with a Write,
// and keeping it in this select is what guarantees that.
func (c *Conn) writeLoop(ctx context.Context, pingInterval, pongTimeout time.Duration) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return

		case payload := <-c.send:
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(writeCtx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				// Not logged at error: a client closing its laptop produces
				// this, and it is the expected end of most connections.
				c.logger.Debug("write to socket failed, closing",
					slog.String("trip_id", c.TripID.String()),
					slog.String("error", err.Error()))
				c.closeWith(websocket.StatusInternalError, "write failed")
				return
			}

		case <-ping.C:
			// A pong is read by the *reader* goroutine's Read call, which
			// handles control frames underneath; this returns as soon as it
			// arrives, or when the deadline passes.
			pingCtx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err != nil {
				c.logger.Debug("no pong within the timeout, closing",
					slog.String("trip_id", c.TripID.String()),
					slog.String("user_id", c.UserID.String()))
				c.closeWith(websocket.StatusPolicyViolation, "keepalive timed out")
				return
			}
		}
	}
}
