package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/togethergo/chat/internal/auth"
	"github.com/togethergo/chat/internal/store"
)

// Probe is a dependency /readyz asks about. An interface rather than the
// concrete Redis client and consumer so that internal/http keeps knowing
// nothing about either, and so a test can report any answer without them.
type Probe interface {
	Ping(ctx context.Context) error
}

type healthHandlers struct {
	store    *store.Store
	verifier *auth.Verifier
	redis    Probe
	broker   Probe
}

// healthz is liveness. It checks nothing on purpose: a liveness probe that
// fails because Postgres is slow gets the container killed and restarted, which
// cannot possibly help — and here it would also drop every open socket on the
// instance.
func (h *healthHandlers) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is readiness: can this instance actually serve a request.
//
// Four checks, and Redis is the one that distinguishes this service from the
// others. In trip, Redis is a cache and losing it costs latency; here it is the
// ticket store, the rate limiter and the fan-out between replicas. An instance
// that cannot reach it can issue no tickets, accept no sockets and deliver no
// message written on another replica — it is alive, it looks healthy, and it is
// useless. It must be taken out of the load balancer.
//
// The broker is checked for a subtler reason. Rooms and membership are a
// projection of `chat.trip-events` and nothing else, so an instance that has
// been disconnected from RabbitMQ is serving a view of the world that is
// falling behind: it will refuse handshakes for trips created since, and tell
// approved participants they are not members. Nothing is lost while it does —
// the messages wait on the queue — but it should not be taking traffic.
func (h *healthHandlers) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	logger := loggerFrom(r.Context())

	check := func(name string, probe func(context.Context) error) {
		if probe == nil {
			checks[name] = "disabled"
			return
		}
		if err := probe(ctx); err != nil {
			logger.Warn("readiness check failed",
				slog.String("check", name),
				slog.String("error", err.Error()))
			checks[name] = "unavailable"
			return
		}
		checks[name] = "ok"
	}

	check("database", h.store.Ping)
	if h.verifier == nil {
		checks["jwks"] = "disabled"
	} else {
		check("jwks", h.verifier.Warm)
	}
	if h.redis == nil {
		checks["redis"] = "disabled"
	} else {
		check("redis", h.redis.Ping)
	}
	if h.broker == nil {
		checks["broker"] = "disabled"
	} else {
		check("broker", h.broker.Ping)
	}

	ready := true
	for _, value := range checks {
		if value != "ok" && value != "disabled" {
			ready = false
		}
	}

	status := http.StatusOK
	body := map[string]any{"status": "ready", "checks": checks}
	if !ready {
		status = http.StatusServiceUnavailable
		body["status"] = "not_ready"
	}
	writeJSON(w, r, status, body)
}
