package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/togethergo/chat/internal/domain"
	"github.com/togethergo/chat/internal/hub"
	"github.com/togethergo/chat/internal/realtime"
	"github.com/togethergo/chat/internal/store"
)

type wsHandlers struct {
	store   *store.Store
	tickets *realtime.Tickets
	hub     *hub.Hub
}

// connect is GET /ws/chat/{tripID}?ticket=…
//
// The handshake has three gates and they are in this order for a reason:
//
//  1. **The trip id parses.** Cheapest, and a malformed one cannot be a real
//     ticket's trip either.
//  2. **The ticket redeems, atomically, and names this trip.** GETDEL, so a
//     ticket presented twice succeeds exactly once — the second connection
//     finds nothing, whichever of the two it is. The trip is compared against
//     the URL so a member of one room cannot use their legitimate ticket to
//     open another.
//  3. **Membership is re-read from the database.** Not from the ticket: the
//     ticket says who the caller is, and the database says what they may do.
//     Between issuing a ticket and spending it, an organizer can remove the
//     participant — thirty seconds is short but it is not zero — and a
//     handshake that trusted the ticket's word would let them in anyway.
//
// Every refusal completes the upgrade and then closes with a code, rather than
// answering the HTTP request with a status. A browser cannot read the status of
// a failed websocket handshake: `onclose` reports 1006 and the page has no way
// to tell an expired ticket from a dropped connection. Accepting and closing
// costs one round trip and gives the client a number it can branch on — which
// is the whole reason RFC 6455 reserves 4000–4999 for applications.
func (h *wsHandlers) connect(w http.ResponseWriter, r *http.Request) {
	logger := loggerFrom(r.Context())

	tripID, err := pathUUID(chi.URLParam(r, "tripID"), "trip_id")
	if err != nil {
		h.hub.Reject(w, r, hub.CloseBadRequest, "trip id is not a uuid")
		return
	}

	userID, err := h.tickets.Redeem(r.Context(), r.URL.Query().Get("ticket"), tripID)
	if err != nil {
		if errors.Is(err, domain.ErrTicketInvalid) {
			logger.Debug("refused a websocket handshake with an invalid ticket",
				slog.String("trip_id", tripID.String()))
			h.hub.Reject(w, r, hub.CloseTicketInvalid, "ticket is missing, expired or not for this trip")
			return
		}
		// Redis is unreachable. The client cannot fix this and should not
		// hammer; a policy close rather than the ticket code, so a client that
		// distinguishes them does not go and fetch a ticket it cannot store.
		logger.Error("redeeming a websocket ticket failed", slog.String("error", err.Error()))
		h.hub.Reject(w, r, websocket.StatusInternalError, "ticket store unavailable")
		return
	}

	access, err := h.store.Authorize(r.Context(), tripID, userID)
	switch {
	case errors.Is(err, domain.ErrRoomNotFound):
		// The projection has not caught up. This service does not call the trip
		// service to find out whether the room ought to exist — that would be
		// the synchronous dependency the whole event-driven arrangement exists
		// to avoid — so the connection is refused with a code that means "try
		// again shortly", and the event arrives while the client is retrying.
		logger.Info("refused a websocket handshake for a room the projection has not seen yet",
			slog.String("trip_id", tripID.String()))
		h.hub.Reject(w, r, hub.CloseRoomNotReady, "the room has not been provisioned yet")
		return
	case err != nil:
		logger.Error("authorizing a websocket handshake failed", slog.String("error", err.Error()))
		h.hub.Reject(w, r, websocket.StatusInternalError, "could not verify membership")
		return
	case !access.IsMember:
		h.hub.Reject(w, r, hub.CloseNotMember, "not a member of this trip")
		return
	}

	// A cancelled room is connectable. History stays readable and presence
	// keeps working; what a cancelled room refuses is the INSERT, which is
	// enforced in the one place it belongs — see store.InsertMessage.

	h.hub.Serve(w, r, tripID, userID)
}
