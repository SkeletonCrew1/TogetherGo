package httpapi

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/togethergo/trip/internal/auth"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/users"
)

// Deps is everything the router needs. Passed in rather than constructed here
// so that main owns every lifetime and the tests can substitute a real store
// over a throwaway database without touching this file.
type Deps struct {
	Store    *store.Store
	Verifier *auth.Verifier
	Logger   *slog.Logger

	// Users resolves organizer ids for the discovery endpoints. Optional: a
	// service configured without an identity URL serves search with organizer
	// blocks that carry an id and nothing else.
	Users users.Resolver

	// Broker is the outbox relay, for /readyz. Optional and nil in tests that
	// are not about publishing: an instance without one reports the broker
	// check as disabled rather than as failed.
	Broker BrokerProbe

	// Scheduler backs POST /internal/scheduler/tick. Optional, and the route
	// does not exist without it: main leaves it nil in production, where the
	// only thing that should be advancing trips is the loop.
	Scheduler Scheduler

	// InternalToken is the shared secret the /internal group requires as
	// X-Internal-Token. An empty one disables the group entirely rather than
	// opening it — a misconfiguration must not be the thing that publishes an
	// unauthenticated write endpoint.
	InternalToken string
}

// BrokerProbe is what /readyz asks about RabbitMQ. An interface rather than
// *events.Relay so that internal/http keeps knowing nothing about AMQP, and so
// a test can report either answer without a broker.
type BrokerProbe interface {
	Ping(ctx context.Context) error
}

// NewRouter builds the service's HTTP handler.
//
// The gateway strips no prefix, so the paths here are the paths clients use:
// Traefik routes /api/trips to this service verbatim.
func NewRouter(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = NewLogger("info")
	}

	trips := &tripHandlers{store: deps.Store, users: deps.Users}
	health := &healthHandlers{store: deps.Store, verifier: deps.Verifier, broker: deps.Broker}

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

	r.Route("/api/trips", func(r chi.Router) {
		// Every trip endpoint requires a valid access token, discovery
		// included. The SPA holds one on every screen that lists trips, and
		// opening the busiest read path in the system to unauthenticated
		// callers is a product decision with a rate-limiting problem attached —
		// not something to fall into by leaving a route outside the group.
		r.Use(authenticate(deps.Verifier))

		r.Get("/", trips.searchTrips)
		r.Post("/", trips.createTrip)
		r.Get("/{tripID}", trips.getTrip)
		r.Get("/{tripID}/similar", trips.similarTrips)
		r.Patch("/{tripID}", trips.updateTrip)
		r.Delete("/{tripID}", trips.deleteTrip)
		r.Post("/{tripID}/publish", trips.publishTrip)
		r.Post("/{tripID}/cancel", trips.cancelTrip)

		// Participation. `me` is a static segment beside `{requestID}` and
		// chi resolves static before wildcard, so a request whose id is
		// literally the string "me" cannot shadow the caller's own — which is
		// moot, since request ids are uuids, but it is why the two can share a
		// level at all.
		r.Post("/{tripID}/requests", trips.createJoinRequest)
		r.Get("/{tripID}/requests", trips.listJoinRequests)
		r.Get("/{tripID}/requests/me", trips.myJoinRequest)
		r.Delete("/{tripID}/requests/me", trips.cancelJoinRequest)
		r.Post("/{tripID}/requests/{requestID}/approve", trips.approveJoinRequest)
		r.Post("/{tripID}/requests/{requestID}/reject", trips.rejectJoinRequest)

		r.Post("/{tripID}/participants/me", trips.leaveTrip)
		r.Delete("/{tripID}/participants/{userID}", trips.removeParticipant)

		r.Post("/{tripID}/invites", trips.inviteUsers)
		r.Get("/{tripID}/invites", trips.listInvites)
	})

	// The dashboard. A separate prefix rather than /api/trips/mine because it
	// is a different question: /api/trips is the catalogue, addressed by trip
	// id, and /api/my is addressed by nothing at all — the subject is the
	// token. CLAUDE.md routes the prefix here, and deploy/traefik/dynamic.yml
	// carries the rule.
	r.Route("/api/my", func(r chi.Router) {
		r.Use(authenticate(deps.Verifier))

		r.Get("/trips", trips.myTrips)
	})

	// The /internal group. Nothing here is routed at the gateway, and what is
	// mounted is decided by what main passed in: no scheduler or no token means
	// no routes, not open ones.
	if deps.Scheduler != nil && deps.InternalToken != "" {
		internal := &internalHandlers{scheduler: deps.Scheduler}
		r.Route("/internal", func(r chi.Router) {
			r.Use(requireInternalToken(deps.InternalToken))
			r.Post("/scheduler/tick", internal.tick)
		})
	}

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
