package hub

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// announceArrival records the user in the room's presence set, tells the room
// if that is news, and gives the new socket the current roster.
//
// The snapshot is the reason this is not just a ZADD. A client that has just
// connected knows nothing about who else is in the room, and the only other
// source of that would be an HTTP endpoint returning a list that is stale by
// the time it renders. Sending it as ordinary `presence` frames means the
// client has one code path for "who is here" instead of two.
func (h *Hub) announceArrival(ctx context.Context, conn *Conn) {
	online, err := h.presence.Online(ctx, conn.TripID)
	if err != nil {
		h.logger.Warn("reading room presence failed",
			slog.String("trip_id", conn.TripID.String()),
			slog.String("error", err.Error()))
	}
	for _, userID := range online {
		if userID == conn.UserID {
			continue
		}
		conn.send1(PresenceFrame{Type: "presence", UserID: userID, Status: StatusOnline})
	}

	h.touch(ctx, conn.TripID, conn.UserID)
}

// touch heartbeats one user in one room and announces them if they were not
// there a moment ago.
//
// The "if" comes from Redis rather than from anything this process remembers:
// ZADD reports whether it added the member or updated them. That is what makes
// the announcement correct with several replicas and several tabs — the second
// tab's arrival is an update and says nothing, and a user swept out by another
// instance while still connected here is re-added and re-announced on the next
// heartbeat.
func (h *Hub) touch(ctx context.Context, tripID, userID uuid.UUID) {
	arrived, err := h.presence.Touch(ctx, tripID, userID)
	if err != nil {
		h.logger.Warn("recording presence failed",
			slog.String("trip_id", tripID.String()),
			slog.String("error", err.Error()))
		return
	}
	if arrived {
		h.broadcast(ctx, tripID, PresenceFrame{Type: "presence", UserID: userID, Status: StatusOnline}, nil)
	}
}

// announceDeparture clears the user from the room's presence set, unless they
// still have another socket open on this instance.
//
// The local check is what keeps a user with two tabs from flickering offline
// when they close one. It cannot see the *other* replicas' sockets, so a user
// connected to two instances who disconnects from one is briefly announced
// offline — and the other instance's next heartbeat re-adds them, sees a fresh
// insert, and announces them online again. Presence is best-effort and this is
// the shape of the error it makes: a wrong answer that corrects itself within
// one heartbeat, rather than one that persists.
func (h *Hub) announceDeparture(ctx context.Context, conn *Conn) {
	if h.hasLocalConn(conn.TripID, conn.UserID) {
		return
	}

	departed, err := h.presence.Forget(ctx, conn.TripID, conn.UserID)
	if err != nil {
		h.logger.Warn("clearing presence failed",
			slog.String("trip_id", conn.TripID.String()),
			slog.String("error", err.Error()))
		return
	}
	if departed {
		h.broadcast(ctx, conn.TripID, PresenceFrame{Type: "presence", UserID: conn.UserID, Status: StatusOffline}, nil)
	}
}

// hasLocalConn reports whether this instance still holds a socket for a user in
// a room. Called after the closing connection has already been removed from the
// map, so it answers "is there another one".
func (h *Hub) hasLocalConn(tripID, userID uuid.UUID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for conn := range h.rooms[tripID] {
		if conn.UserID == userID {
			return true
		}
	}
	return false
}

// presenceLoop is the heartbeat, and the sweeper.
//
// It runs on the ping interval, which is not a coincidence: the two answer the
// same question from opposite ends. The ping decides whether *this* instance
// still has a live socket; the heartbeat tells the other instances that it does.
// A member whose heartbeat stops — because the instance holding them was killed,
// which is the case no amount of deferred cleanup can cover — is swept out by
// whichever instance notices first, one presence TTL later.
func (h *Hub) presenceLoop(ctx context.Context) {
	ticker := time.NewTicker(h.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.heartbeat(ctx)
		}
	}
}

// heartbeat refreshes every locally-held (room, user) pair and sweeps the rooms
// this instance has somebody in.
//
// Only those rooms: a room with no local sockets is somebody else's to sweep,
// and if nobody has a socket in it there is nobody to tell anyway. The stale
// entries in such a room are read out by Presence.Online's score filter when
// the next person connects, so nothing is left showing an ex-member as online.
func (h *Hub) heartbeat(ctx context.Context) {
	// Deduplicated, so a user with three tabs on this instance is one ZADD
	// rather than three.
	type member struct{ tripID, userID uuid.UUID }

	h.mu.RLock()
	members := make(map[member]struct{}, len(h.rooms))
	rooms := make([]uuid.UUID, 0, len(h.rooms))
	for tripID, conns := range h.rooms {
		rooms = append(rooms, tripID)
		for conn := range conns {
			members[member{tripID: tripID, userID: conn.UserID}] = struct{}{}
		}
	}
	h.mu.RUnlock()

	for m := range members {
		h.touch(ctx, m.tripID, m.userID)
	}

	for _, tripID := range rooms {
		swept, err := h.presence.Sweep(ctx, tripID)
		if err != nil {
			h.logger.Warn("sweeping stale presence failed",
				slog.String("trip_id", tripID.String()),
				slog.String("error", err.Error()))
			continue
		}
		for _, userID := range swept {
			h.broadcast(ctx, tripID, PresenceFrame{Type: "presence", UserID: userID, Status: StatusOffline}, nil)
		}
	}
}
