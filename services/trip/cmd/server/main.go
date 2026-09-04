// Command server runs the trip service's HTTP API.
//
// It never runs migrations. Schema changes are an explicit, separate step
// (CLAUDE.md) — `cmd/migrate`, or `make migrate-trip` from the repository root.
// A server that migrated on startup would let a rolling deploy run two schema
// versions against one database and would make an unrelated restart into a
// schema change.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/togethergo/trip/internal/auth"
	"github.com/togethergo/trip/internal/config"
	"github.com/togethergo/trip/internal/events"
	httpapi "github.com/togethergo/trip/internal/http"
	"github.com/togethergo/trip/internal/scheduler"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/users"
)

func main() {
	if err := run(); err != nil {
		// Configuration and startup failures happen before the logger is
		// necessarily usable, so they go to stderr in plain text and the
		// process exits non-zero. Everything after startup is structured JSON.
		_, _ = os.Stderr.WriteString("trip: " + err.Error() + "\n")
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := httpapi.NewLogger(cfg.LogLevel)

	// Signals are trapped before anything is opened, so a Ctrl-C during a slow
	// database connect still gets a clean shutdown rather than a killed
	// process holding a half-open pool.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	verifier := auth.NewVerifier(auth.NewKeySet(cfg.JWKSURL, cfg.JWKSCacheTTL, nil))

	userCache, closeCache, err := openUserCache(cfg, logger)
	if err != nil {
		return err
	}
	defer closeCache()

	st := store.New(pool)

	// The read path for user display data, outermost first:
	//
	//	user_ref (local table, fed by the consumer)
	//	  └─ identity's GET /internal/users, behind a 60s Redis cache
	//
	// The projection answers almost everything and keeps answering while
	// identity is down; the client behind it fills the gaps a cold start or a
	// missed event leaves, and backfills them so the next page is local again.
	// See internal/users/projection.go for why both halves are kept.
	identityClient := users.New(users.Options{
		BaseURL:       cfg.IdentityInternalURL,
		InternalToken: cfg.InternalAPIToken,
		Timeout:       cfg.IdentityTimeout,
		Cache:         userCache,
		Logger:        logger,
	})
	resolver := users.NewProjectionResolver(users.ProjectionOptions{
		Projection: st,
		Upstream:   identityClient,
		Logger:     logger,
	})

	// The outbox relay runs on a context of its own rather than on the signal
	// context, so that shutdown can be ordered: stop taking requests, drain the
	// ones in flight, and only then stop the thing that publishes what they
	// wrote. Cancelling both at once would abandon events belonging to
	// transactions that had just committed.
	relayCtx, stopRelay := context.WithCancel(context.Background())
	defer stopRelay()

	relay := events.NewRelay(events.RelayOptions{
		Pool:         pool,
		AMQPURL:      cfg.RabbitMQURL,
		Exchange:     cfg.RabbitMQExchange,
		Logger:       logger,
		PollInterval: cfg.OutboxPollInterval,
		Retention:    cfg.OutboxRetention,
	})
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		relay.Run(relayCtx)
	}()

	// The user projection consumer. It shares the relay's context for the same
	// reason: a message already taken off the wire should finish its
	// transaction during the drain rather than be abandoned and redelivered.
	consumer := events.NewConsumer(events.ConsumerOptions{
		Projection: st,
		AMQPURL:    cfg.RabbitMQURL,
		Logger:     logger,
	})
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		consumer.Run(relayCtx)
	}()

	// The lifecycle scheduler. On the signal context rather than the relay's:
	// it is a *producer* of events, so it must stop when the HTTP server does
	// and before the relay drains what it wrote. Starting a transition during
	// the shutdown would be writing outbox rows nothing is left to publish.
	scheduled := scheduler.New(scheduler.Options{
		Lifecycle: st,
		Interval:  cfg.SchedulerInterval,
		Logger:    logger,
	})
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		scheduled.Run(ctx)
	}()

	// nil unless the debug tick is enabled, and the route does not exist
	// without it — see NewRouter.
	var debugTick httpapi.Scheduler
	if cfg.SchedulerDebugTick {
		debugTick = scheduled
		logger.Info("debug scheduler tick enabled", slog.String("path", "/internal/scheduler/tick"))
	}

	server := &http.Server{
		Addr: net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.Port)),
		Handler: httpapi.NewRouter(httpapi.Deps{
			Store:         st,
			Verifier:      verifier,
			Logger:        logger,
			Users:         resolver,
			Broker:        relay,
			Scheduler:     debugTick,
			InternalToken: cfg.InternalAPIToken,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("trip service started",
			slog.String("addr", server.Addr),
			slog.String("environment", cfg.Environment),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining")
	}

	// Stop trapping signals: a second Ctrl-C during the drain should kill the
	// process outright rather than be swallowed.
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}

	// The scheduler was cancelled by the same signal that stopped the server,
	// so this only waits for the pass it may have been in the middle of. It has
	// to finish before the relay does: a pass that committed a transition wrote
	// outbox rows with it, and the relay is what carries them.
	select {
	case <-schedulerDone:
	case <-shutdownCtx.Done():
		logger.Warn("lifecycle scheduler did not stop within the shutdown timeout")
	}

	// Now that no new events can be written, let the relay and the consumer
	// stop. The relay finishes the batch it is in the middle of — abandoning
	// one between "the broker confirmed these" and "the rows are marked
	// published" would republish the lot on the next boot — and then closes the
	// AMQP channel.
	stopRelay()
	select {
	case <-relayDone:
	case <-shutdownCtx.Done():
		logger.Warn("outbox relay did not stop within the shutdown timeout")
	}
	select {
	case <-consumerDone:
	case <-shutdownCtx.Done():
		logger.Warn("user projection consumer did not stop within the shutdown timeout")
	}

	logger.Info("trip service stopped")
	return <-serverErr
}

// openUserCache builds the Redis cache in front of identity's batch resolver,
// or a no-op one when REDIS_URL is unset.
//
// A bad DSN fails here, at startup, because it is a configuration mistake. An
// unreachable Redis does not: the connection is lazy, every cache operation
// swallows its own errors, and a broker-less run degrades to calling identity
// on every page. Caching is an optimisation on this path and must never be the
// reason a search fails.
func openUserCache(cfg *config.Config, logger *slog.Logger) (users.Cache, func(), error) {
	if cfg.RedisURL == "" {
		logger.Info("user cache disabled", slog.String("reason", "REDIS_URL is not set"))
		return users.NoCache{}, func() {}, nil
	}

	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, nil, fmt.Errorf("REDIS_URL is not a valid redis DSN: %w", err)
	}
	client := redis.NewClient(options)

	return users.NewRedisCache(client, cfg.UserCacheTTL, logger),
		func() { _ = client.Close() },
		nil
}

func openPool(ctx context.Context, cfg *config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	poolCfg.MaxConns = cfg.MaxDBConns
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 15 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, err
	}

	// Fail fast on an unreachable or misconfigured database. A DSN naming
	// another service's database fails here, at connect time, because
	// deploy/postgres/init/01-databases.sql revokes CONNECT — which is the
	// point of doing it that way.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
