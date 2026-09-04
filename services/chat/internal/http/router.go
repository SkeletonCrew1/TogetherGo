package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/togethergo/chat/internal/auth"
	"github.com/togethergo/chat/internal/hub"
	"github.com/togethergo/chat/internal/realtime"
	"github.com/togethergo/chat/internal/store"
)

// Deps is everything the router needs. Passed in rather than constructed here
// so that main owns every lifetime and the tests can substitute a real store
// over a throwaway database without touching this file.
type Deps struct {
	Store    *store.Store
	Verifier *auth.Verifier
	Tickets  *realtime.Tickets
	Hub      *hub.Hub
	Logger   *slog.Logger

	// Redis and Broker back /readyz. Optional and nil in tests that are not
	// about either: an instance without one reports that check as disabled
	// rather than as failed.
	Redis  Probe
	Broker Probe
}

// NewRouter builds the service's HTTP handler.
//
// The gateway strips no prefix, so the paths here are the paths clients use:
// Traefik routes /api/chat and /ws/chat to this service verbatim.
func NewRouter(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = NewLogger("info")
	}

	rooms := &roomHandlers{store: deps.Store}
	tickets := &ticketHandlers{store: deps.Store, tickets: deps.Tickets}
	sockets := &wsHandlers{store: deps.Store, tickets: deps.Tickets, hub: deps.Hub}
	health := &healthHandlers{
		store:    deps.Store,
		verifier: deps.Verifier,
		redis:    deps.Redis,
		broker:   deps.Broker,
	}

	r := chi.NewRouter()
	r.Use(requestContext(logger))
	r.Use(recoverer)

	// Health endpoints sit outside the authenticated group: a probe that has to
	// hold a token is a probe that reports the identity service's outages as
	// this service's.
	//
	// HEAD as well as GET: probes that only want the status line — Docker's
	// HEALTHCHECK with `wget --spider`, most uptime checkers — send HEAD, and
	// chi answers a GET-only route with 405, which reads as a failed probe.
	r.Get("/healthz", health.healthz)
	r.Head("/healthz", health.healthz)
	r.Get("/readyz", health.readyz)
	r.Head("/readyz", health.readyz)

	r.Route("/api/chat", func(r chi.Router) {
		// Bearer auth, verified against identity's JWKS by this service
		// (CLAUDE.md rule 6). The websocket route below is deliberately *not*
		// in this group: a browser cannot put a header on a handshake, which is
		// what the ticket endpoint inside this group exists to work around.
		r.Use(authenticate(deps.Verifier))

		r.Post("/tickets", tickets.createTicket)
		r.Get("/rooms", rooms.listRooms)
		r.Get("/rooms/{tripID}/messages", rooms.listMessages)
	})

	// The socket. Authenticated by a single-use ticket rather than by a bearer
	// token — see ticketHandlers.createTicket for why, and wsHandlers.connect
	// for the three gates it passes through.
	r.Get("/ws/chat/{tripID}", sockets.connect)

	// chi's own 404 and 405 write plain text; the error shape is the same on
	// every response this service produces, including the ones no handler saw.
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusNotFound, errorEnvelope{apiError{
			Code:    "not_found",
			Message: "No such endpoint.",
		}})
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusMethodNotAllowed, errorEnvelope{apiError{
			Code:    "method_not_allowed",
			Message: "That method is not allowed on this endpoint.",
		}})
	})

	return r
}
