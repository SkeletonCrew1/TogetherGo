// Command server runs the chat service's HTTP API and its websockets.
//
// It never runs migrations. Schema changes are an explicit, separate step
// (CLAUDE.md) — `cmd/migrate`, or `make migrate-chat` from the repository root.
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

	"github.com/togethergo/chat/internal/auth"
	"github.com/togethergo/chat/internal/config"
	"github.com/togethergo/chat/internal/events"
	httpapi "github.com/togethergo/chat/internal/http"
	"github.com/togethergo/chat/internal/hub"
	"github.com/togethergo/chat/internal/realtime"
	"github.com/togethergo/chat/internal/store"
)

func main() {
	if err := run(); err != nil {
		// Configuration and startup failures happen before the logger is
		// necessarily usable, so they go to stderr in plain text and the
		// process exits non-zero. Everything after startup is structured JSON.
		_, _ = os.Stderr.WriteString("chat: " + err.Error() + "\n")
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

	redisClient, err := openRedis(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer func() { _ = redisClient.Close() }()

	verifier := auth.NewVerifier(auth.NewKeySet(cfg.JWKSURL, cfg.JWKSCacheTTL, nil))
	st := store.New(pool)

	tickets := realtime.NewTickets(redisClient, cfg.TicketTTL)
	bus := realtime.NewBus(redisClient, logger)
	presence := realtime.NewPresence(redisClient, cfg.PresenceTTL)
	limiter := realtime.NewLimiter(redisClient, cfg.RateLimit, cfg.RateWindow)

	rooms := hub.New(hub.Options{
		Store:          st,
		Bus:            bus,
		Presence:       presence,
		Limiter:        limiter,
		Logger:         logger,
		SendBuffer:     cfg.SendBuffer,
		PingInterval:   cfg.PingInterval,
		PongTimeout:    cfg.PongTimeout,
		AllowedOrigins: cfg.AllowedOrigins,
	})

	// The hub's loops and the consumer run on a context of their own rather
	// than on the signal context, so that shutdown can be ordered: close the
	// sockets first, then drain the HTTP requests, and only then stop the
	// machinery those two depend on. Cancelling everything at once would leave
	// closing sockets unable to publish their "offline" frames.
	backgroundCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		rooms.Run(backgroundCtx)
	}()

	// The room projection consumer, with the hub as its evictor: a
	// `participant.removed` handled on this replica has to close that user's
	// socket wherever it is, and it reaches the other replicas through the same
	// Redis fan-out every message uses.
	consumer := events.NewConsumer(events.ConsumerOptions{
		Projection: st,
		Evictor:    rooms,
		AMQPURL:    cfg.RabbitMQURL,
		Queue:      cfg.Queue,
		Logger:     logger,
	})
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		consumer.Run(backgroundCtx)
	}()

	server := &http.Server{
		Addr: net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.Port)),
		Handler: httpapi.NewRouter(httpapi.Deps{
			Store:    st,
			Verifier: verifier,
			Tickets:  tickets,
			Hub:      rooms,
			Logger:   logger,
			Redis:    realtime.NewHealth(redisClient),
			Broker:   consumer,
		}),
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout and no WriteTimeout, and that is the one place this
		// server differs from the trip service's.
		//
		// Both are deadlines on the whole connection, and net/http applies them
		// to a hijacked one as well: a WriteTimeout of thirty seconds would
		// kill every websocket thirty seconds after it opened, healthy or not.
		// The protections they would give are provided per-operation instead —
		// ReadHeaderTimeout above for the handshake, a read limit and a
		// ping/pong deadline on the socket (internal/hub), and a write deadline
		// around each frame.
		IdleTimeout: 120 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("chat service started",
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

	// Shutdown closes the listeners first and then waits for handlers to
	// return — and a websocket handler does not return until its socket
	// closes. So it is started here and the sockets are closed underneath it;
	// waiting for it first would block for the whole timeout and then kill
	// every connection with a TCP reset instead of a close frame.
	shutdownErr := make(chan error, 1)
	go func() { shutdownErr <- server.Shutdown(shutdownCtx) }()

	rooms.Shutdown()

	if err := <-shutdownErr; err != nil {
		return err
	}

	// Every socket is closed and every request has been answered, so nothing
	// is left that needs the fan-out or the projection.
	stopBackground()
	select {
	case <-hubDone:
	case <-shutdownCtx.Done():
		logger.Warn("the room fan-out did not stop within the shutdown timeout")
	}
	select {
	case <-consumerDone:
	case <-shutdownCtx.Done():
		logger.Warn("the room projection consumer did not stop within the shutdown timeout")
	}

	logger.Info("chat service stopped")
	return <-serverErr
}

// openRedis builds the client every realtime component shares.
//
// A malformed DSN is fatal, because it is a configuration mistake and belongs
// at startup. An unreachable Redis is not: the connection is lazy, Compose
// already orders the containers, and a chat service that refused to boot
// because Redis was a few seconds behind would turn a transient into an outage.
// It is reported on /readyz for as long as it lasts, which keeps the instance
// out of the load balancer — and this is the one dependency whose absence makes
// the service useless rather than merely slower.
func openRedis(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*redis.Client, error) {
	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, fmt.Errorf("REDIS_URL is not a valid redis DSN: %w", err)
	}
	client := redis.NewClient(options)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		logger.Warn("redis is not reachable yet; sockets and tickets will fail until it is",
			slog.String("error", err.Error()))
	}
	return client, nil
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
