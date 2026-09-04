// Package config loads the chat service's settings from the environment.
//
// One entry point, Load, which either returns a fully validated Config or an
// error that names the variable at fault. There is no os.Getenv anywhere else
// in the service: a process that is going to fail because of a missing setting
// should fail while it is starting, not on the first request that needs it.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the whole of the service's configuration.
type Config struct {
	// Port the HTTP server listens on. Client traffic arrives through the
	// gateway; this port is exposed in Compose for debugging only.
	Port int

	// DatabaseURL is a pgx-compatible DSN for chat_db, as chat_user. It must
	// not name any other database — see deploy/postgres/init/01-databases.sql,
	// which revokes CONNECT so that a stray DSN fails at connect time.
	DatabaseURL string

	// MaxDBConns bounds the pgx pool.
	MaxDBConns int32

	// JWKSURL is identity's key document, fetched over the internal network.
	// It authenticates the REST endpoints, POST /api/chat/tickets included;
	// the websocket handshake itself is authenticated by a ticket, because a
	// browser cannot put a bearer token on a websocket handshake.
	JWKSURL      string
	JWKSCacheTTL time.Duration

	// RedisURL is required, and this is the one service where that is true.
	//
	// In trip, Redis is a cache and losing it costs latency. Here it is the
	// fan-out between replicas, the ticket store and the rate limiter: without
	// it two users on two instances cannot see each other's messages at all,
	// and no websocket could be opened in the first place. A chat service that
	// started without Redis would look healthy and deliver nothing.
	RedisURL string

	// RabbitMQURL is the broker this service consumes `chat.trip-events` from.
	// Required: rooms and membership are a projection of that queue and nothing
	// else, so an instance that cannot reach the broker will refuse every new
	// connection for a trip created while it was disconnected.
	RabbitMQURL string

	// Queue is the inbox, named in contracts/events.md. Not declared by this
	// service — the broker imports the topology at boot from
	// deploy/rabbitmq/definitions.json — so this only has to name it.
	Queue string

	// TicketTTL is how long a websocket ticket is redeemable for. Thirty
	// seconds: long enough for a page to finish loading and open a socket,
	// short enough that a ticket captured from a client's memory is worthless
	// by the time it is used.
	TicketTTL time.Duration

	// The message rate limit, per user per room: RateLimit messages per
	// RateWindow. Over the limit the sender gets an error frame and the message
	// is dropped; the connection stays open.
	RateLimit  int
	RateWindow time.Duration

	// PingInterval and PongTimeout are the liveness probe on an open socket. A
	// TCP connection that has been silently dropped — a laptop lid, a NAT
	// timeout — looks exactly like an idle one until something is written to
	// it, so the server writes something on a timer.
	PingInterval time.Duration
	PongTimeout  time.Duration

	// SendBuffer is the depth of each connection's outbound queue. A client
	// that cannot keep up fills it and is disconnected rather than allowed to
	// block the room — see internal/hub.
	SendBuffer int

	// PresenceTTL is how long a member stays in a room's presence set without a
	// heartbeat. It must be comfortably longer than PingInterval, which is what
	// the heartbeat rides on: an instance that is killed leaves its users in the
	// set, and this is how long before another instance sweeps them out.
	PresenceTTL time.Duration

	// AllowedOrigins is the websocket origin allowlist.
	//
	// A websocket handshake is not subject to the same-origin policy the way an
	// XHR is: any page on the internet can open one to this service, and the
	// browser will attach the user's cookies. This service does not authenticate
	// with cookies — a ticket has to be fetched with a bearer token first, which
	// a cross-origin page cannot do — so the origin check is defence in depth
	// rather than the only lock. It is still checked, because the cost is a
	// configuration line.
	//
	// Patterns are matched against the Origin header's host, and may contain
	// `*` (see github.com/coder/websocket's AcceptOptions.OriginPatterns).
	AllowedOrigins []string

	LogLevel    string
	Environment string

	// ShutdownTimeout is how long in-flight requests and open sockets get after
	// a SIGTERM before the process stops waiting for them.
	ShutdownTimeout time.Duration
}

// Load reads the environment, or explains what is missing.
func Load() (*Config, error) {
	var problems []string

	cfg := &Config{}

	cfg.DatabaseURL = os.Getenv("CHAT_DATABASE_URL")
	if cfg.DatabaseURL == "" {
		problems = append(problems, "CHAT_DATABASE_URL is required")
	}

	cfg.JWKSURL = os.Getenv("JWKS_URL")
	if cfg.JWKSURL == "" {
		problems = append(problems, "JWKS_URL is required")
	} else if !strings.HasPrefix(cfg.JWKSURL, "http://") && !strings.HasPrefix(cfg.JWKSURL, "https://") {
		problems = append(problems, "JWKS_URL must be an http(s) URL")
	}

	port, err := intFromEnv("CHAT_PORT", 8003)
	if err != nil {
		problems = append(problems, err.Error())
	} else if port < 1 || port > 65535 {
		problems = append(problems, "CHAT_PORT must be between 1 and 65535")
	}
	cfg.Port = port

	maxConns, err := intFromEnv("CHAT_DB_MAX_CONNS", 10)
	if err != nil {
		problems = append(problems, err.Error())
	} else if maxConns < 1 {
		problems = append(problems, "CHAT_DB_MAX_CONNS must be at least 1")
	}
	cfg.MaxDBConns = int32(maxConns)

	// Seconds, matching JWKS_CACHE_TTL in .env.example, which the identity
	// service's Cache-Control header also reflects.
	jwksTTL, err := intFromEnv("JWKS_CACHE_TTL", 600)
	if err != nil {
		problems = append(problems, err.Error())
	} else if jwksTTL < 1 {
		problems = append(problems, "JWKS_CACHE_TTL must be at least 1 second")
	}
	cfg.JWKSCacheTTL = time.Duration(jwksTTL) * time.Second

	cfg.RedisURL = os.Getenv("REDIS_URL")
	if cfg.RedisURL == "" {
		problems = append(problems, "REDIS_URL is required")
	}

	cfg.RabbitMQURL = os.Getenv("RABBITMQ_URL")
	if cfg.RabbitMQURL == "" {
		problems = append(problems, "RABBITMQ_URL is required")
	} else if !strings.HasPrefix(cfg.RabbitMQURL, "amqp://") && !strings.HasPrefix(cfg.RabbitMQURL, "amqps://") {
		problems = append(problems, "RABBITMQ_URL must be an amqp(s) URL")
	}

	cfg.Queue = stringFromEnv("CHAT_QUEUE", "chat.trip-events")

	ticketTTL, err := intFromEnv("CHAT_TICKET_TTL", 30)
	if err != nil {
		problems = append(problems, err.Error())
	} else if ticketTTL < 1 {
		problems = append(problems, "CHAT_TICKET_TTL must be at least 1 second")
	}
	cfg.TicketTTL = time.Duration(ticketTTL) * time.Second

	rateLimit, err := intFromEnv("CHAT_RATE_LIMIT", 20)
	if err != nil {
		problems = append(problems, err.Error())
	} else if rateLimit < 1 {
		problems = append(problems, "CHAT_RATE_LIMIT must be at least 1")
	}
	cfg.RateLimit = rateLimit

	rateWindow, err := intFromEnv("CHAT_RATE_WINDOW", 10)
	if err != nil {
		problems = append(problems, err.Error())
	} else if rateWindow < 1 {
		problems = append(problems, "CHAT_RATE_WINDOW must be at least 1 second")
	}
	cfg.RateWindow = time.Duration(rateWindow) * time.Second

	pingInterval, err := intFromEnv("CHAT_PING_INTERVAL", 30)
	if err != nil {
		problems = append(problems, err.Error())
	} else if pingInterval < 1 {
		problems = append(problems, "CHAT_PING_INTERVAL must be at least 1 second")
	}
	cfg.PingInterval = time.Duration(pingInterval) * time.Second

	pongTimeout, err := intFromEnv("CHAT_PONG_TIMEOUT", 60)
	if err != nil {
		problems = append(problems, err.Error())
	} else if pongTimeout <= pingInterval {
		// A pong deadline shorter than the interval between pings would close
		// healthy connections; equal is a coin flip. The default pair is 30 and
		// 60, which is one missed pong of tolerance.
		problems = append(problems, "CHAT_PONG_TIMEOUT must be greater than CHAT_PING_INTERVAL")
	}
	cfg.PongTimeout = time.Duration(pongTimeout) * time.Second

	sendBuffer, err := intFromEnv("CHAT_SEND_BUFFER", 64)
	if err != nil {
		problems = append(problems, err.Error())
	} else if sendBuffer < 1 {
		problems = append(problems, "CHAT_SEND_BUFFER must be at least 1")
	}
	cfg.SendBuffer = sendBuffer

	presenceTTL, err := intFromEnv("CHAT_PRESENCE_TTL", 90)
	if err != nil {
		problems = append(problems, err.Error())
	} else if presenceTTL <= pingInterval {
		problems = append(problems, "CHAT_PRESENCE_TTL must be greater than CHAT_PING_INTERVAL")
	}
	cfg.PresenceTTL = time.Duration(presenceTTL) * time.Second

	// Hosts, not URLs: `localhost:*` matches the gateway on 8080 and the Vite
	// dev server on 5173 without naming either.
	cfg.AllowedOrigins = splitList(stringFromEnv("CHAT_ALLOWED_ORIGINS", "localhost:*,127.0.0.1:*"))

	shutdown, err := intFromEnv("SHUTDOWN_TIMEOUT", 15)
	if err != nil {
		problems = append(problems, err.Error())
	} else if shutdown < 1 {
		problems = append(problems, "SHUTDOWN_TIMEOUT must be at least 1 second")
	}
	cfg.ShutdownTimeout = time.Duration(shutdown) * time.Second

	cfg.LogLevel = strings.ToLower(stringFromEnv("LOG_LEVEL", "info"))
	switch cfg.LogLevel {
	case "debug", "info", "warn", "warning", "error":
	default:
		problems = append(problems, fmt.Sprintf("LOG_LEVEL %q is not one of debug, info, warn, error", cfg.LogLevel))
	}

	cfg.Environment = stringFromEnv("ENVIRONMENT", "local")

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid chat service configuration: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}

func stringFromEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func intFromEnv(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New(key + " must be an integer")
	}
	return value, nil
}

// splitList parses a comma-separated setting, dropping blanks so a trailing
// comma or a stray space is not a pattern that matches nothing.
func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
