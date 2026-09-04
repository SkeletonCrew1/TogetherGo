package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/config"
)

// The minimum a chat service needs to start. Every one of these is required
// because without it the service would run and be useless rather than fail:
// no database, no token verification, no fan-out, no room projection.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("CHAT_DATABASE_URL", "postgres://chat_user:chat_pass@postgres:5432/chat_db")
	t.Setenv("JWKS_URL", "http://identity:8001/.well-known/jwks.json")
	t.Setenv("REDIS_URL", "redis://redis:6379/0")
	t.Setenv("RABBITMQ_URL", "amqp://togethergo:togethergo@rabbitmq:5672/")
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	cfg, err := config.Load()
	require.NoError(t, err)

	require.Equal(t, 8003, cfg.Port)
	require.Equal(t, "chat.trip-events", cfg.Queue)
	require.Equal(t, 30, int(cfg.TicketTTL.Seconds()))
	require.Equal(t, 20, cfg.RateLimit)
	require.Equal(t, 10, int(cfg.RateWindow.Seconds()))
	require.Equal(t, 30, int(cfg.PingInterval.Seconds()))
	require.Equal(t, 60, int(cfg.PongTimeout.Seconds()))
	require.Equal(t, []string{"localhost:*", "127.0.0.1:*"}, cfg.AllowedOrigins)
}

func TestLoadNamesEveryMissingVariable(t *testing.T) {
	// Nothing set at all. The point of the assertion is that the error names
	// all four rather than the first one, so a fresh environment is one fix
	// rather than four rounds of trial and error.
	t.Setenv("CHAT_DATABASE_URL", "")
	t.Setenv("JWKS_URL", "")
	t.Setenv("REDIS_URL", "")
	t.Setenv("RABBITMQ_URL", "")

	_, err := config.Load()
	require.Error(t, err)
	for _, name := range []string{"CHAT_DATABASE_URL", "JWKS_URL", "REDIS_URL", "RABBITMQ_URL"} {
		require.Contains(t, err.Error(), name)
	}
}

// A pong deadline shorter than the ping interval would close healthy
// connections on a timer, which is the sort of misconfiguration that looks like
// a network problem for a week.
func TestPongTimeoutMustExceedThePingInterval(t *testing.T) {
	setRequired(t)
	t.Setenv("CHAT_PING_INTERVAL", "60")
	t.Setenv("CHAT_PONG_TIMEOUT", "30")

	_, err := config.Load()
	require.ErrorContains(t, err, "CHAT_PONG_TIMEOUT")
}

func TestPresenceTTLMustExceedThePingInterval(t *testing.T) {
	setRequired(t)
	t.Setenv("CHAT_PING_INTERVAL", "30")
	t.Setenv("CHAT_PRESENCE_TTL", "20")

	_, err := config.Load()
	require.ErrorContains(t, err, "CHAT_PRESENCE_TTL")
}

func TestRabbitURLMustBeAnAMQPURL(t *testing.T) {
	setRequired(t)
	t.Setenv("RABBITMQ_URL", "http://rabbitmq:5672/")

	_, err := config.Load()
	require.ErrorContains(t, err, "RABBITMQ_URL")
}
