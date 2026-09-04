package config_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/config"
)

// setEnv installs a complete, valid environment and lets a test break one part
// of it. t.Setenv restores everything when the test ends.
func setEnv(t *testing.T, overrides map[string]string) {
	t.Helper()

	env := map[string]string{
		"TRIP_DATABASE_URL":     "postgres://trip_user:trip_pass@postgres:5432/trip_db?sslmode=disable",
		"JWKS_URL":              "http://identity:8001/.well-known/jwks.json",
		"INTERNAL_API_TOKEN":    "dev-internal-token",
		"RABBITMQ_URL":          "amqp://togethergo:togethergo@rabbitmq:5672/",
		"IDENTITY_INTERNAL_URL": "",
		"IDENTITY_TIMEOUT_MS":   "",
		"REDIS_URL":             "",
		"USER_CACHE_TTL":        "",
		"TRIP_PORT":             "",
		"TRIP_DB_MAX_CONNS":     "",
		"JWKS_CACHE_TTL":        "",
		"SHUTDOWN_TIMEOUT":      "",
		"LOG_LEVEL":             "",
		"ENVIRONMENT":           "",

		"TRIP_SCHEDULER_INTERVAL":   "",
		"TRIP_SCHEDULER_DEBUG_TICK": "",
	}
	for k, v := range overrides {
		env[k] = v
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, nil)

	cfg, err := config.Load()
	require.NoError(t, err)

	require.Equal(t, 8002, cfg.Port)
	require.EqualValues(t, 10, cfg.MaxDBConns)
	// Ten minutes, matching the Cache-Control identity puts on its JWKS.
	require.Equal(t, 10*time.Minute, cfg.JWKSCacheTTL)
	require.Equal(t, 15*time.Second, cfg.ShutdownTimeout)
	require.Equal(t, "info", cfg.LogLevel)
	require.Equal(t, "local", cfg.Environment)

	require.Equal(t, "http://identity:8001", cfg.IdentityInternalURL,
		"the internal origin, not the gateway: /internal is deliberately unrouted there")
	require.Equal(t, 2*time.Second, cfg.IdentityTimeout)
	// Sixty seconds, matching the Cache-Control identity puts on /internal/users.
	require.Equal(t, time.Minute, cfg.UserCacheTTL)
	// No REDIS_URL is a supported configuration, not a failure: it means the
	// service runs without a user cache.
	require.Empty(t, cfg.RedisURL)
}

func TestLoadReadsOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"TRIP_PORT":             "9002",
		"TRIP_DB_MAX_CONNS":     "25",
		"JWKS_CACHE_TTL":        "60",
		"SHUTDOWN_TIMEOUT":      "5",
		"LOG_LEVEL":             "DEBUG",
		"ENVIRONMENT":           "staging",
		"IDENTITY_INTERNAL_URL": "http://identity.internal:9001",
		"IDENTITY_TIMEOUT_MS":   "750",
		"REDIS_URL":             "redis://redis:6379/0",
		"USER_CACHE_TTL":        "30",
	})

	cfg, err := config.Load()
	require.NoError(t, err)

	require.Equal(t, 9002, cfg.Port)
	require.EqualValues(t, 25, cfg.MaxDBConns)
	require.Equal(t, time.Minute, cfg.JWKSCacheTTL)
	require.Equal(t, 5*time.Second, cfg.ShutdownTimeout)
	require.Equal(t, "debug", cfg.LogLevel, "the level is case-insensitive")
	require.Equal(t, "staging", cfg.Environment)
	require.Equal(t, "http://identity.internal:9001", cfg.IdentityInternalURL)
	require.Equal(t, 750*time.Millisecond, cfg.IdentityTimeout)
	require.Equal(t, "redis://redis:6379/0", cfg.RedisURL)
	require.Equal(t, 30*time.Second, cfg.UserCacheTTL)
}

// TestLoadFailsFast: a process that is going to fail because of a missing
// setting should fail while it is starting, not on the first request.
func TestLoadFailsFast(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides map[string]string
		mentions  string
	}{
		{"no database url", map[string]string{"TRIP_DATABASE_URL": ""}, "TRIP_DATABASE_URL"},
		{"no jwks url", map[string]string{"JWKS_URL": ""}, "JWKS_URL"},
		{"a jwks url that is not a url", map[string]string{"JWKS_URL": "identity:8001/jwks"}, "JWKS_URL"},
		{"a port that is not a number", map[string]string{"TRIP_PORT": "eight-thousand"}, "TRIP_PORT"},
		{"a port out of range", map[string]string{"TRIP_PORT": "70000"}, "TRIP_PORT"},
		{"a pool of zero connections", map[string]string{"TRIP_DB_MAX_CONNS": "0"}, "TRIP_DB_MAX_CONNS"},
		{"a zero jwks ttl", map[string]string{"JWKS_CACHE_TTL": "0"}, "JWKS_CACHE_TTL"},
		{"an unknown log level", map[string]string{"LOG_LEVEL": "chatty"}, "LOG_LEVEL"},
		{"no internal token", map[string]string{"INTERNAL_API_TOKEN": ""}, "INTERNAL_API_TOKEN"},
		{"no broker url", map[string]string{"RABBITMQ_URL": ""}, "RABBITMQ_URL"},
		{"a broker url that is not amqp", map[string]string{"RABBITMQ_URL": "http://rabbitmq:5672"}, "RABBITMQ_URL"},
		{"an identity url that is not a url", map[string]string{"IDENTITY_INTERNAL_URL": "identity:8001"}, "IDENTITY_INTERNAL_URL"},
		{"a zero identity timeout", map[string]string{"IDENTITY_TIMEOUT_MS": "0"}, "IDENTITY_TIMEOUT_MS"},
		{"a zero user cache ttl", map[string]string{"USER_CACHE_TTL": "0"}, "USER_CACHE_TTL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.overrides)

			cfg, err := config.Load()
			require.Error(t, err)
			require.Nil(t, cfg)
			require.Contains(t, err.Error(), tc.mentions,
				"the error must name the variable at fault")
		})
	}
}

// TestLoadReportsEveryProblemAtOnce: fixing a misconfigured deployment one
// restart per variable is a miserable way to spend an afternoon.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setEnv(t, map[string]string{
		"TRIP_DATABASE_URL": "",
		"JWKS_URL":          "",
		"LOG_LEVEL":         "chatty",
	})

	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "TRIP_DATABASE_URL")
	require.Contains(t, err.Error(), "JWKS_URL")
	require.Contains(t, err.Error(), "LOG_LEVEL")
}

// TestSchedulerDefaults covers the two settings the background workers added,
// including the one that defaults from another setting.
func TestSchedulerDefaults(t *testing.T) {
	t.Run("the interval defaults to a minute", func(t *testing.T) {
		setEnv(t, nil)
		cfg, err := config.Load()
		require.NoError(t, err)
		require.Equal(t, time.Minute, cfg.SchedulerInterval)
	})

	t.Run("the interval is configurable", func(t *testing.T) {
		setEnv(t, map[string]string{"TRIP_SCHEDULER_INTERVAL": "5"})
		cfg, err := config.Load()
		require.NoError(t, err)
		require.Equal(t, 5*time.Second, cfg.SchedulerInterval)
	})

	t.Run("a zero interval is refused", func(t *testing.T) {
		setEnv(t, map[string]string{"TRIP_SCHEDULER_INTERVAL": "0"})
		_, err := config.Load()
		require.ErrorContains(t, err, "TRIP_SCHEDULER_INTERVAL")
	})

	// The debug tick is on everywhere but production. An endpoint that advances
	// trips on demand is a lever nobody needs in production and somebody will
	// eventually pull.
	t.Run("the debug tick follows the environment", func(t *testing.T) {
		for environment, want := range map[string]bool{"": true, "staging": true, "production": false} {
			setEnv(t, map[string]string{"ENVIRONMENT": environment})
			cfg, err := config.Load()
			require.NoError(t, err)
			require.Equalf(t, want, cfg.SchedulerDebugTick, "ENVIRONMENT=%q", environment)
		}
	})

	t.Run("an explicit value wins in both directions", func(t *testing.T) {
		setEnv(t, map[string]string{
			"ENVIRONMENT":               "production",
			"TRIP_SCHEDULER_DEBUG_TICK": "true",
		})
		cfg, err := config.Load()
		require.NoError(t, err)
		require.True(t, cfg.SchedulerDebugTick)

		setEnv(t, map[string]string{"TRIP_SCHEDULER_DEBUG_TICK": "false"})
		cfg, err = config.Load()
		require.NoError(t, err)
		require.False(t, cfg.SchedulerDebugTick)
	})

	t.Run("a non-boolean is refused", func(t *testing.T) {
		setEnv(t, map[string]string{"TRIP_SCHEDULER_DEBUG_TICK": "yes please"})
		_, err := config.Load()
		require.ErrorContains(t, err, "TRIP_SCHEDULER_DEBUG_TICK")
	})
}
