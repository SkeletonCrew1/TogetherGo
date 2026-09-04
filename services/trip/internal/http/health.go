package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/togethergo/trip/internal/auth"
	"github.com/togethergo/trip/internal/store"
)

type healthHandlers struct {
	store    *store.Store
	verifier *auth.Verifier
	broker   BrokerProbe
}

// healthz is liveness. It checks nothing on purpose: a liveness probe that
// fails because Postgres is slow gets the container killed and restarted, which
// cannot possibly help.
func (h *healthHandlers) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, r, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz is readiness: can this instance actually serve a request.
//
// It checks the database, the JWKS document and the broker: the first two
// because every endpoint needs them — a process that cannot verify a token
// would answer 401 to every caller, which is worse than being taken out of the
// load balancer until it can — and the third because this service publishes
// now, and an instance that cannot reach RabbitMQ is accumulating an outbox
// backlog rather than doing its job. Nothing is lost while it does (the rows
// wait), but it should not be reported as ready.
func (h *healthHandlers) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := map[string]string{}
	logger := loggerFrom(r.Context())

	if err := h.store.Ping(ctx); err != nil {
		logger.Warn("readiness: database check failed", slog.String("error", err.Error()))
		checks["database"] = "unavailable"
	} else {
		checks["database"] = "ok"
	}

	if h.verifier == nil {
		checks["jwks"] = "disabled"
	} else if err := h.verifier.Warm(ctx); err != nil {
		logger.Warn("readiness: jwks check failed", slog.String("error", err.Error()))
		checks["jwks"] = "unavailable"
	} else {
		checks["jwks"] = "ok"
	}

	if h.broker == nil {
		checks["broker"] = "disabled"
	} else if err := h.broker.Ping(ctx); err != nil {
		logger.Warn("readiness: broker check failed", slog.String("error", err.Error()))
		checks["broker"] = "unavailable"
	} else {
		checks["broker"] = "ok"
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
