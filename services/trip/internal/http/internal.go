package httpapi

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"

	"github.com/togethergo/trip/internal/domain"
)

// The /internal group: routes that exist for other services and for operators,
// never for clients.
//
// `deploy/traefik/dynamic.yml` routes no /internal prefix at all, so nothing
// here is reachable from outside the Compose network. That is the first lock.
// The second is the shared secret below, because in Compose every container
// shares one network with every other and "not routed at the gateway" is a
// statement about Traefik rather than about who can open a socket to port 8002.

// Scheduler is the lifecycle worker, as the debug endpoint needs it. An
// interface rather than *scheduler.Scheduler so internal/http keeps depending
// on nothing but the store and the domain.
type Scheduler interface {
	Tick(ctx context.Context) (domain.LifecycleResult, error)
}

type internalHandlers struct {
	scheduler Scheduler
}

// requireInternalToken gates the /internal group on X-Internal-Token.
//
// Compared in constant time. The comparison is not a plausible timing target —
// an attacker would need to be inside the Compose network already — but a
// secret compared with == is a habit worth not having, and the cost is nothing.
//
// An unset token disables the group rather than opening it: see NewRouter.
func requireInternalToken(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := r.Header.Get("X-Internal-Token")
			if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				// 404, not 401. A caller who does not hold the secret should
				// not learn that the endpoint is there.
				writeJSON(w, r, http.StatusNotFound, errorEnvelope{apiError{
					Code:    "not_found",
					Message: "No such endpoint.",
				}})
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// tick handles POST /internal/scheduler/tick: one lifecycle pass, run
// synchronously, answering with what it did.
//
// It exists so that tests do not have to sleep. Asserting that a trip whose
// end_at has passed becomes `completed` otherwise means waiting out a tick
// interval, and a suite that sleeps is a suite that is either slow or flaky.
// It is also the thing to reach for in an incident, when the question is
// "did the scheduler stop, or is there genuinely nothing due?".
//
// Registered only when the service is built with a scheduler *and* the debug
// tick is enabled — off in production, where the loop is the only thing that
// should be advancing trips.
func (h *internalHandlers) tick(w http.ResponseWriter, r *http.Request) {
	result, err := h.scheduler.Tick(r.Context())
	if err != nil {
		loggerFrom(r.Context()).Error("manual lifecycle tick failed", slog.String("error", err.Error()))
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, tickResponse{
		Started:         result.Started,
		Completed:       result.Completed,
		CompletedEvents: result.CompletedEvents,
	})
}

// tickResponse is what one pass did. Counts only: the endpoint is an operator's
// and a test's, and neither wants a list of trip ids it did not ask for.
type tickResponse struct {
	Started         int `json:"started"`
	Completed       int `json:"completed"`
	CompletedEvents int `json:"completed_events"`
}
