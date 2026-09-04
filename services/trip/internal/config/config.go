// Package config loads the trip service's settings from the environment.
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

	// DatabaseURL is a pgx-compatible DSN for trip_db, as trip_user. It must
	// not name any other database — see deploy/postgres/init/01-databases.sql,
	// which revokes CONNECT so that a stray DSN fails at connect time.
	DatabaseURL string

	// MaxDBConns bounds the pgx pool.
	MaxDBConns int32

	// JWKSURL is identity's key document, fetched over the internal network.
	JWKSURL      string
	JWKSCacheTTL time.Duration

	// IdentityInternalURL is the origin of identity's /internal endpoints —
	// the container address, never the gateway. `deploy/traefik/dynamic.yml`
	// routes no /internal prefix at all, deliberately, so a URL pointing at the
	// gateway would resolve every organizer to a 404.
	IdentityInternalURL string

	// InternalAPIToken is presented as X-Internal-Token on those calls.
	InternalAPIToken string

	// IdentityTimeout bounds one call to identity. It sits inside a page
	// render, so an unresponsive identity has to become "no organizer names"
	// quickly rather than hold search requests open.
	IdentityTimeout time.Duration

	// RedisURL is the user cache. Optional: empty means no caching, every
	// discovery page resolves its organizers straight from identity, and
	// nothing else changes.
	RedisURL     string
	UserCacheTTL time.Duration

	// RabbitMQURL is the broker the outbox relay publishes through. Required:
	// this service emits events now, and one started without a broker would
	// accept join requests, approve them, and quietly build an outbox backlog
	// that nothing drains — healthy-looking and wrong. A broker that is merely
	// *down* is a different matter and is not fatal; the relay retries and
	// /readyz says so.
	RabbitMQURL string

	// RabbitMQExchange is the topic exchange every event goes to. It is not
	// declared by this service — the broker imports the topology at boot from
	// deploy/rabbitmq/definitions.json — so this only has to name it.
	RabbitMQExchange string

	// OutboxPollInterval is how often the relay looks for unpublished rows.
	OutboxPollInterval time.Duration

	// OutboxRetention is how long published rows are kept before the daily
	// sweep deletes them. Unpublished rows are never deleted, however old.
	OutboxRetention time.Duration

	// SchedulerInterval is how often the lifecycle pass runs — the loop that
	// starts trips whose start_at has gone by and completes those whose end_at
	// has. Configurable mainly so that a test or a local demo can run it every
	// second instead of every minute.
	SchedulerInterval time.Duration

	// SchedulerDebugTick registers POST /internal/scheduler/tick, which runs
	// one pass synchronously. Defaults to on everywhere but production: in
	// production the loop should be the only thing advancing trips, and an
	// endpoint that does it on demand is a lever nobody needs and somebody will
	// eventually pull.
	SchedulerDebugTick bool

	LogLevel    string
	Environment string

	// ShutdownTimeout is how long in-flight requests get to finish after a
	// SIGTERM before the process stops waiting for them.
	ShutdownTimeout time.Duration
}

// Load reads the environment, or explains what is missing.
func Load() (*Config, error) {
	var problems []string

	cfg := &Config{}

	cfg.DatabaseURL = os.Getenv("TRIP_DATABASE_URL")
	if cfg.DatabaseURL == "" {
		problems = append(problems, "TRIP_DATABASE_URL is required")
	}

	cfg.JWKSURL = os.Getenv("JWKS_URL")
	if cfg.JWKSURL == "" {
		problems = append(problems, "JWKS_URL is required")
	} else if !strings.HasPrefix(cfg.JWKSURL, "http://") && !strings.HasPrefix(cfg.JWKSURL, "https://") {
		problems = append(problems, "JWKS_URL must be an http(s) URL")
	}

	port, err := intFromEnv("TRIP_PORT", 8002)
	if err != nil {
		problems = append(problems, err.Error())
	} else if port < 1 || port > 65535 {
		problems = append(problems, "TRIP_PORT must be between 1 and 65535")
	}
	cfg.Port = port

	maxConns, err := intFromEnv("TRIP_DB_MAX_CONNS", 10)
	if err != nil {
		problems = append(problems, err.Error())
	} else if maxConns < 1 {
		problems = append(problems, "TRIP_DB_MAX_CONNS must be at least 1")
	}
	cfg.MaxDBConns = int32(maxConns)

	// Seconds, matching JWKS_CACHE_TTL in .env.example, which the identity
	// service's Cache-Control header also reflects.
	ttl, err := intFromEnv("JWKS_CACHE_TTL", 600)
	if err != nil {
		problems = append(problems, err.Error())
	} else if ttl < 1 {
		problems = append(problems, "JWKS_CACHE_TTL must be at least 1 second")
	}
	cfg.JWKSCacheTTL = time.Duration(ttl) * time.Second

	cfg.IdentityInternalURL = stringFromEnv("IDENTITY_INTERNAL_URL", "http://identity:8001")
	if !strings.HasPrefix(cfg.IdentityInternalURL, "http://") && !strings.HasPrefix(cfg.IdentityInternalURL, "https://") {
		problems = append(problems, "IDENTITY_INTERNAL_URL must be an http(s) URL")
	}

	cfg.InternalAPIToken = os.Getenv("INTERNAL_API_TOKEN")
	if cfg.InternalAPIToken == "" {
		// Required even though a missing token only degrades organizer
		// resolution: a service that starts without it would serve every
		// search page with null names and look healthy doing it. That is a
		// misconfiguration, and misconfigurations belong at startup.
		problems = append(problems, "INTERNAL_API_TOKEN is required")
	}

	identityTimeout, err := intFromEnv("IDENTITY_TIMEOUT_MS", 2000)
	if err != nil {
		problems = append(problems, err.Error())
	} else if identityTimeout < 1 {
		problems = append(problems, "IDENTITY_TIMEOUT_MS must be at least 1 millisecond")
	}
	cfg.IdentityTimeout = time.Duration(identityTimeout) * time.Millisecond

	// Unset is legal and means "run without a cache". The DSN itself is parsed
	// in main, where the client is built, so a malformed one still fails at
	// startup rather than on the first search.
	cfg.RedisURL = os.Getenv("REDIS_URL")

	userCacheTTL, err := intFromEnv("USER_CACHE_TTL", 60)
	if err != nil {
		problems = append(problems, err.Error())
	} else if userCacheTTL < 1 {
		problems = append(problems, "USER_CACHE_TTL must be at least 1 second")
	}
	cfg.UserCacheTTL = time.Duration(userCacheTTL) * time.Second

	cfg.RabbitMQURL = os.Getenv("RABBITMQ_URL")
	if cfg.RabbitMQURL == "" {
		problems = append(problems, "RABBITMQ_URL is required")
	} else if !strings.HasPrefix(cfg.RabbitMQURL, "amqp://") && !strings.HasPrefix(cfg.RabbitMQURL, "amqps://") {
		problems = append(problems, "RABBITMQ_URL must be an amqp(s) URL")
	}

	cfg.RabbitMQExchange = stringFromEnv("RABBITMQ_EXCHANGE", "togethergo.events")

	pollMs, err := intFromEnv("TRIP_OUTBOX_POLL_MS", 500)
	if err != nil {
		problems = append(problems, err.Error())
	} else if pollMs < 10 {
		problems = append(problems, "TRIP_OUTBOX_POLL_MS must be at least 10 milliseconds")
	}
	cfg.OutboxPollInterval = time.Duration(pollMs) * time.Millisecond

	retentionDays, err := intFromEnv("OUTBOX_RETENTION_DAYS", 7)
	if err != nil {
		problems = append(problems, err.Error())
	} else if retentionDays < 1 {
		problems = append(problems, "OUTBOX_RETENTION_DAYS must be at least 1 day")
	}
	cfg.OutboxRetention = time.Duration(retentionDays) * 24 * time.Hour

	schedulerSeconds, err := intFromEnv("TRIP_SCHEDULER_INTERVAL", 60)
	if err != nil {
		problems = append(problems, err.Error())
	} else if schedulerSeconds < 1 {
		problems = append(problems, "TRIP_SCHEDULER_INTERVAL must be at least 1 second")
	}
	cfg.SchedulerInterval = time.Duration(schedulerSeconds) * time.Second

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

	// Read after Environment, because that is what it defaults from. Explicit
	// values win in both directions: a production instance can be told to
	// enable the endpoint, and a local one can be told not to.
	debugTick, err := boolFromEnv("TRIP_SCHEDULER_DEBUG_TICK", cfg.Environment != "production")
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.SchedulerDebugTick = debugTick

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid trip service configuration: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}

func stringFromEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// boolFromEnv accepts what strconv.ParseBool accepts — "1", "t", "true",
// "TRUE", and their negatives — and names the variable when it does not.
func boolFromEnv(key string, fallback bool) (bool, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.New(key + " must be a boolean")
	}
	return value, nil
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
