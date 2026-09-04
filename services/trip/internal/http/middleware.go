package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/togethergo/trip/internal/auth"
)

// requestScope is the mutable per-request state the log line is assembled from.
//
// A pointer in the context rather than a value, because the user id is not
// known when the logging middleware runs — authentication happens further down
// the chain and writes it back here, so that the single access-log line at the
// end can carry it.
type requestScope struct {
	requestID string
	userID    string
}

type scopeContextKey struct{}

func scopeFrom(ctx context.Context) *requestScope {
	scope, _ := ctx.Value(scopeContextKey{}).(*requestScope)
	return scope
}

// quietPaths are the probe endpoints. Logging them would bury the traffic that
// matters under a line every ten seconds from Docker and another from Traefik.
var quietPaths = map[string]bool{"/healthz": true, "/readyz": true}

// requestContext mints or adopts a request id, hangs a logger carrying it off
// the context, and logs one line when the handler returns.
//
// An inbound X-Request-ID is honoured so a trace survives the gateway hop and
// can be followed across services; one is generated when there is none.
func requestContext(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" {
				requestID = uuid.NewString()
			}

			scope := &requestScope{requestID: requestID}
			ctx := context.WithValue(r.Context(), scopeContextKey{}, scope)
			ctx = withLogger(ctx, logger.With(slog.String("request_id", requestID)))

			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			recorder.Header().Set("X-Request-ID", requestID)

			started := time.Now()
			next.ServeHTTP(recorder, r.WithContext(ctx))

			if quietPaths[r.URL.Path] {
				return
			}

			attrs := []any{
				slog.String("method", r.Method),
				// r.URL.Path, never RawQuery: query strings are where
				// filter values and, one careless client away, tokens end up.
				slog.String("path", r.URL.Path),
				slog.Int("status", recorder.status),
				slog.Float64("duration_ms", float64(time.Since(started).Microseconds())/1000),
			}
			if scope.userID != "" {
				attrs = append(attrs, slog.String("user_id", scope.userID))
			}
			loggerFrom(ctx).Info("request completed", attrs...)
		})
	}
}

// statusRecorder remembers the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.written {
		r.status = status
		r.written = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// recoverer turns a panic into a 500 rather than a dropped connection, and logs
// the stack once.
func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			// A panic after the response has started cannot be turned into a
			// 500 — the status is already on the wire. Re-panicking lets
			// net/http close the connection, which is the only remaining way
			// to signal that the body is truncated.
			if recovered == http.ErrAbortHandler {
				panic(recovered)
			}

			loggerFrom(r.Context()).Error("panic recovered",
				slog.Any("panic", recovered),
				slog.String("stack", string(debug.Stack())),
			)
			writeJSON(w, r, http.StatusInternalServerError, errorEnvelope{apiError{
				Code:    "internal_error",
				Message: "An unexpected error occurred.",
			}})
		}()
		next.ServeHTTP(w, r)
	})
}

// authenticate verifies the bearer token and puts the caller on the context.
//
// This service verifies the signature itself against identity's JWKS. It does
// not read an identity header the gateway might have injected: in Compose every
// container is on the same network as every other, so such a header proves
// nothing about who sent the request (CLAUDE.md rule 6).
func authenticate(verifier *auth.Verifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				writeError(w, r, auth.ErrInvalidToken)
				return
			}

			claims, err := verifier.Verify(r.Context(), token)
			if err != nil {
				writeError(w, r, err)
				return
			}

			ctx := auth.WithClaims(r.Context(), claims)
			if scope := scopeFrom(ctx); scope != nil {
				scope.userID = claims.UserID.String()
			}
			// Every log line from here on names the caller. The email claim is
			// deliberately not logged: it is personal data and the id is
			// enough to join against identity when someone needs it.
			ctx = withLogger(ctx, loggerFrom(ctx).With(slog.String("user_id", claims.UserID.String())))

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken extracts the credential from an Authorization header, requiring
// the scheme to be exactly one space-separated "Bearer" (case-insensitive, per
// RFC 7235) followed by a non-empty token.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
