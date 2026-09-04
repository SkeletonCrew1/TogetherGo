// Package httpapi is the trip service's HTTP surface: router, middleware,
// handlers and the one place domain errors become responses.
//
// The package is named httpapi rather than http, after the directory it lives
// in, because every file in it also imports net/http and one of the two would
// otherwise need an alias in all of them.
package httpapi

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// serviceName is stamped on every log line, so a `docker compose logs` across
// services stays greppable.
const serviceName = "trip"

// NewLogger builds the structured JSON logger described in CLAUDE.md: one JSON
// object per line on stdout, always carrying service, level and msg, plus
// request_id and user_id once a request is in scope.
//
// Nothing here ever logs a token, a request body or a header. The handlers log
// named fields only; there is no code path that dumps a whole request.
func NewLogger(level string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(level),
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Key {
			case slog.TimeKey:
				// `ts`, matching the identity service's formatter, so both
				// services' lines sort on the same key.
				a.Key = "ts"
			case slog.LevelKey:
				a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
			}
			return a
		},
	})
	return slog.New(handler).With(slog.String("service", serviceName))
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

type loggerContextKey struct{}

// withLogger attaches a request-scoped logger to the context.
func withLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerContextKey{}, logger)
}

// loggerFrom returns the request-scoped logger, falling back to a discarding
// one so that a handler reached outside the middleware chain — in a test, say —
// does not panic on a nil pointer.
func loggerFrom(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerContextKey{}).(*slog.Logger); ok {
		return logger
	}
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}
