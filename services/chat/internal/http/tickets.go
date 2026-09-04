package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/togethergo/chat/internal/auth"
	"github.com/togethergo/chat/internal/domain"
	"github.com/togethergo/chat/internal/realtime"
	"github.com/togethergo/chat/internal/store"
)

type ticketHandlers struct {
	store   *store.Store
	tickets *realtime.Tickets
}

// createTicket issues a short-lived, single-use credential for one room's
// socket.
//
// This endpoint exists because of a limitation in the browser: the WebSocket
// constructor takes a URL and nothing else, so there is no way to put an
// Authorization header on the handshake. The two usual answers are a cookie —
// which this system does not use, and which would bring CSRF with it — and the
// access token in the query string, which writes a fifteen-minute credential
// for every service into Traefik's access log, the browser's history, and the
// Referer of anything the page loads next.
//
// A ticket is the third answer: it is fetched with a normal bearer token over
// HTTPS, it is worth thirty seconds, it works for one room, and it works once.
// Leaking one costs the attacker a socket on a room they would have to already
// be a member of.
//
// Membership is verified here *and* again at the handshake. This check is what
// stops a non-member getting a ticket at all; the one at the handshake is what
// stops a ticket issued a moment before an approval was revoked from still
// working.
func (h *ticketHandlers) createTicket(w http.ResponseWriter, r *http.Request) {
	var body ticketRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	tripID, err := pathUUID(body.TripID, "trip_id")
	if err != nil {
		writeError(w, r, err)
		return
	}

	userID := auth.UserIDFrom(r.Context())

	// Authorize, not IsMember-only: a room that does not exist yet has to be a
	// 404 rather than a 403, because the client's remedy is to wait and retry
	// rather than to give up.
	access, err := h.store.Authorize(r.Context(), tripID, userID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !access.IsMember {
		writeError(w, r, domain.ErrNotMember)
		return
	}
	// A cancelled room still gets a ticket. Its history stays readable and the
	// socket stays useful; what changes is that InsertMessage refuses, which is
	// the one place that rule belongs.

	ticket, err := h.tickets.Issue(r.Context(), userID, tripID)
	if err != nil {
		writeError(w, r, err)
		return
	}

	loggerFrom(r.Context()).Debug("issued a websocket ticket",
		slog.String("trip_id", tripID.String()))

	writeJSON(w, r, http.StatusCreated, ticketResponse{
		Ticket:    ticket.Value,
		ExpiresIn: int(ticket.TTL.Seconds()),
	})
}
