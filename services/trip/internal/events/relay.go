package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Relay carries outbox rows to RabbitMQ.
//
// It is a goroutine inside this service, not a separate deployable. That is
// what makes "the participant was approved" and "the join_request.approved
// event exists" one atomic fact: the domain write and the outbox insert share a
// transaction, and this loop is the only thing that ever turns the second into
// a message.
//
// Three properties matter and everything below is in service of them:
//
//   - **Nothing is dropped.** A row is marked published only after the broker
//     has confirmed it, in the same transaction that selected it. Any failure —
//     broker down, connection lost, process killed — rolls the batch back with
//     the rows still `published_at IS NULL`, and the next tick republishes
//     them. The cost is at-least-once delivery, which is the contract
//     consumers are written against (contracts/events.md).
//
//   - **Replicas do not collide.** `FOR UPDATE SKIP LOCKED` means a second
//     relay running the same query at the same instant skips the rows this one
//     holds and takes the next hundred. Scaling to three instances needs no
//     leader election and no coordination.
//
//   - **A broker restart is not an outage.** The connection is re-established
//     lazily, with backoff, from inside the publish loop. Nothing about
//     recovery involves restarting this service.
//
// The relay never declares topology. Exchanges, queues and bindings are
// imported by the broker at boot from deploy/rabbitmq/definitions.json; a
// service that declared its own would sooner or later declare it with different
// arguments and earn a PRECONDITION_FAILED on a Monday morning.
type Relay struct {
	pool     *pgxpool.Pool
	url      string
	exchange string
	logger   *slog.Logger

	pollInterval    time.Duration
	maxBackoff      time.Duration
	batchSize       int
	batchTimeout    time.Duration
	dialTimeout     time.Duration
	cleanupInterval time.Duration
	cleanupDelay    time.Duration
	retention       time.Duration

	// The AMQP connection and its publishing channel, replaced wholesale on
	// every reconnect. Guarded because /readyz reads `connected` from the HTTP
	// goroutines while the publish loop is writing it.
	mu        sync.Mutex
	conn      *amqp.Connection
	ch        *amqp.Channel
	connected atomic.Bool
}

// RelayOptions configures a Relay. Only Pool and AMQPURL are required;
// everything else has the default the contract or the prompt fixes.
type RelayOptions struct {
	Pool     *pgxpool.Pool
	AMQPURL  string
	Exchange string
	Logger   *slog.Logger

	PollInterval    time.Duration
	MaxBackoff      time.Duration
	BatchSize       int
	CleanupInterval time.Duration
	Retention       time.Duration

	// CleanupDelay is how long after startup the first sweep runs. Configurable
	// only so that the cleanup test does not have to wait out the default.
	CleanupDelay time.Duration

	// BatchTimeout bounds one batch. It exists because the batch runs on a
	// context deliberately detached from shutdown (see Run), so something has
	// to stop a dial into a black hole from holding the drain open forever.
	BatchTimeout time.Duration
	DialTimeout  time.Duration
}

const (
	defaultPollInterval    = 500 * time.Millisecond
	defaultMaxBackoff      = 30 * time.Second
	defaultBatchSize       = 100
	defaultBatchTimeout    = 20 * time.Second
	defaultDialTimeout     = 5 * time.Second
	defaultCleanupInterval = 24 * time.Hour
	defaultRetention       = 7 * 24 * time.Hour

	// cleanupChunk bounds one DELETE. Repeated until it removes fewer than this
	// many rows, so the job never takes a long lock or blows out a single
	// transaction, however far behind it has fallen.
	cleanupChunk = 10_000

	// defaultCleanupDelay is how long after startup the first sweep runs.
	// Without it a service restarted more often than once a day would never
	// prune at all; with it, the sweep is still nowhere near the startup path.
	defaultCleanupDelay = time.Minute
)

// NewRelay builds a relay. It opens no connection: the broker is contacted
// lazily from the publish loop, so a trip service that starts before RabbitMQ
// does starts anyway and catches up.
func NewRelay(opts RelayOptions) *Relay {
	r := &Relay{
		pool:            opts.Pool,
		url:             opts.AMQPURL,
		exchange:        orDefaultString(opts.Exchange, Exchange),
		logger:          opts.Logger,
		pollInterval:    orDefaultDuration(opts.PollInterval, defaultPollInterval),
		maxBackoff:      orDefaultDuration(opts.MaxBackoff, defaultMaxBackoff),
		batchSize:       orDefaultInt(opts.BatchSize, defaultBatchSize),
		batchTimeout:    orDefaultDuration(opts.BatchTimeout, defaultBatchTimeout),
		dialTimeout:     orDefaultDuration(opts.DialTimeout, defaultDialTimeout),
		cleanupInterval: orDefaultDuration(opts.CleanupInterval, defaultCleanupInterval),
		cleanupDelay:    orDefaultDuration(opts.CleanupDelay, defaultCleanupDelay),
		retention:       orDefaultDuration(opts.Retention, defaultRetention),
	}
	if r.logger == nil {
		r.logger = slog.Default()
	}
	return r
}

// Connected reports whether the relay currently holds a live channel. Read by
// /readyz: this service publishes, so the broker is one of the things it needs
// in order to be doing its job.
func (r *Relay) Connected() bool { return r.connected.Load() }

// Run drives both loops until ctx is cancelled, then closes the connection.
// It blocks, so main runs it in a goroutine with a context of its own.
//
// Shutdown is deliberately not immediate. Cancelling ctx stops the loop from
// *starting* a batch; a batch already in flight runs to completion on a
// detached context, because abandoning one between "the broker confirmed these
// hundred messages" and "the rows are marked published" would republish all
// hundred on the next boot for no reason. Only once both loops have returned is
// the channel closed.
func (r *Relay) Run(ctx context.Context) {
	r.logger.Info("outbox relay started",
		slog.String("exchange", r.exchange),
		slog.Duration("poll_interval", r.pollInterval),
	)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.publishLoop(ctx) }()
	go func() { defer wg.Done(); r.cleanupLoop(ctx) }()
	wg.Wait()

	r.close()
	r.logger.Info("outbox relay stopped")
}

func (r *Relay) publishLoop(ctx context.Context) {
	// A backoff that only ever grows on failure and resets on success. There is
	// no separate retry queue and no per-row attempt counter: the rows are
	// still in the table, so "retry" and "the next tick" are the same thing,
	// and the only state worth keeping is how long to wait before it.
	delay := time.Duration(0)
	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		// select picks at random between two ready channels, so a cancelled
		// context can lose the draw on the tick that shuts the service down.
		// Checked explicitly rather than relied on.
		if ctx.Err() != nil {
			return
		}

		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.batchTimeout)
		published, err := r.drainOnce(batchCtx)
		cancel()

		switch {
		case err != nil:
			delay = nextDelay(delay, r.pollInterval, r.maxBackoff)
			// Warn, not error: the rows are safe, the next tick retries them,
			// and a broker that is down for a minute would otherwise page
			// somebody a hundred and twenty times.
			r.logger.Warn("outbox relay tick failed",
				slog.String("error", err.Error()),
				slog.Duration("retry_in", delay),
			)
		case published >= r.batchSize:
			// A full batch means there is probably more waiting. Going straight
			// round again is what keeps a backlog draining at the speed of the
			// broker rather than at the speed of the poll interval.
			delay = 0
		default:
			delay = r.pollInterval
		}
		timer.Reset(delay)
	}
}

// nextDelay doubles the backoff, starting from the poll interval, and caps it.
func nextDelay(current, base, max time.Duration) time.Duration {
	if current < base {
		return base
	}
	next := current * 2
	if next > max {
		return max
	}
	return next
}

// outboxRow is one unpublished event, as the relay reads it.
type outboxRow struct {
	ID          uuid.UUID
	AggregateID uuid.UUID
	EventType   string
	Payload     json.RawMessage
	CreatedAt   time.Time
}

// drainOnce publishes one batch. Everything in it — the select, the publishes,
// the confirms and the mark — is one transaction.
//
// Returns the number of rows published, which the caller uses only to decide
// whether to poll again immediately.
func (r *Relay) drainOnce(ctx context.Context) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin outbox transaction: %w", err)
	}
	// Unconditional, and a no-op after a successful commit. It is what makes
	// every early return below leave the rows unpublished and unlocked.
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := claimUnpublished(ctx, tx, r.batchSize)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		// Committing an empty read rather than rolling it back: identical in
		// effect, and it keeps "rollback" in the logs meaning something failed.
		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit empty outbox batch: %w", err)
		}
		return 0, nil
	}

	ch, err := r.channel()
	if err != nil {
		return 0, err
	}

	confirms := make([]*amqp.DeferredConfirmation, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))

	for _, row := range rows {
		envelope, err := Build(row.ID, row.EventType, row.AggregateID, row.CreatedAt, row.Payload)
		if err != nil {
			// An event_type with no registered version cannot be published and
			// will never become publishable by waiting. Failing the batch is
			// still the right answer: it is a bug in this build, the row stays
			// put, and the log names it every half second until somebody looks.
			return 0, fmt.Errorf("build envelope for outbox row %s: %w", row.ID, err)
		}
		body, err := json.Marshal(envelope)
		if err != nil {
			return 0, fmt.Errorf("marshal envelope for outbox row %s: %w", row.ID, err)
		}

		confirm, err := ch.PublishWithDeferredConfirmWithContext(ctx, r.exchange, row.EventType,
			// mandatory=false: an event with no queue bound to its routing key
			// is not an error. `participant.removed` has one consumer today and
			// may have none tomorrow, and a publisher is not the place to
			// encode who is listening.
			false, false,
			amqp.Publishing{
				ContentType:     "application/json",
				ContentEncoding: "utf-8",
				DeliveryMode:    amqp.Persistent,
				MessageId:       row.ID.String(),
				Type:            row.EventType,
				Timestamp:       row.CreatedAt.UTC(),
				Body:            body,
			})
		if err != nil {
			r.dropConnection()
			return 0, fmt.Errorf("publish outbox row %s: %w", row.ID, err)
		}
		confirms = append(confirms, confirm)
		ids = append(ids, row.ID)
	}

	// Waited for after the whole batch is on the wire, not one at a time: the
	// confirms are pipelined, so a hundred messages cost one round trip rather
	// than a hundred.
	for i, confirm := range confirms {
		acked, err := confirm.WaitContext(ctx)
		if err != nil {
			r.dropConnection()
			return 0, fmt.Errorf("await confirm for outbox row %s: %w", ids[i], err)
		}
		if !acked {
			// A nack is the broker saying it did not take responsibility for
			// the message. The row stays unpublished and the next tick tries
			// again; nothing is marked, including the rows it did ack, because
			// a partial mark is not worth the second code path.
			return 0, fmt.Errorf("broker nacked outbox row %s", ids[i])
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		return 0, fmt.Errorf("mark outbox rows published: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		// The messages are already at the broker. Rolling back here means they
		// are republished on the next tick with the same event_id, and the
		// consumers' processed_events tables absorb the duplicate. That is
		// at-least-once working as designed, not a failure mode to engineer
		// away.
		return 0, fmt.Errorf("commit outbox batch: %w", err)
	}

	r.logger.Info("outbox batch published",
		slog.Int("count", len(ids)),
		slog.String("exchange", r.exchange),
	)
	return len(ids), nil
}

// claimUnpublished takes the next batch and holds it locked for the life of the
// transaction.
//
// SKIP LOCKED rather than NOWAIT or a plain FOR UPDATE: a second relay must
// take different work, not block on this one's and not fail.
func claimUnpublished(ctx context.Context, tx pgx.Tx, limit int) ([]outboxRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, aggregate_id, event_type, payload, created_at
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, limit)
	if err != nil {
		return nil, fmt.Errorf("select unpublished outbox rows: %w", err)
	}
	defer rows.Close()

	out := make([]outboxRow, 0, limit)
	for rows.Next() {
		var row outboxRow
		if err := rows.Scan(&row.ID, &row.AggregateID, &row.EventType, &row.Payload, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan outbox row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate outbox rows: %w", err)
	}
	return out, nil
}

// cleanupLoop prunes rows the relay has already published.
//
// Published rows are kept for a week — long enough to answer "did we actually
// emit that?" during an incident, short enough that the table stays small.
// Unpublished rows are never touched however old they are: a row still NULL
// after seven days is a stuck event and wants a human, not a DELETE.
func (r *Relay) cleanupLoop(ctx context.Context) {
	timer := time.NewTimer(r.cleanupDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			return
		}

		deleted, err := r.cleanupOnce(ctx)
		switch {
		case err != nil:
			r.logger.Warn("outbox cleanup failed", slog.String("error", err.Error()))
		case deleted > 0:
			r.logger.Info("outbox rows pruned", slog.Int("count", deleted))
		}
		timer.Reset(r.cleanupInterval)
	}
}

func (r *Relay) cleanupOnce(ctx context.Context) (int, error) {
	cutoff := time.Now().Add(-r.retention)
	total := 0

	for {
		tag, err := r.pool.Exec(ctx, `
			DELETE FROM outbox
			WHERE id IN (
				SELECT id
				FROM outbox
				WHERE published_at IS NOT NULL
				  AND published_at < $1
				LIMIT $2
			)`, cutoff, cleanupChunk)
		if err != nil {
			return total, fmt.Errorf("delete published outbox rows: %w", err)
		}
		deleted := int(tag.RowsAffected())
		total += deleted
		if deleted < cleanupChunk {
			return total, nil
		}
		if ctx.Err() != nil {
			return total, nil
		}
	}
}

// channel returns a live publishing channel, dialling if there is not one.
//
// Reconnection lives here rather than in a supervisor because this is the only
// place that needs a channel, and doing it lazily means a broker restart costs
// one failed tick and its backoff — no health check, no restart of this
// service, no separate state machine to get wrong.
func (r *Relay) channel() (*amqp.Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.ch != nil && !r.ch.IsClosed() && r.conn != nil && !r.conn.IsClosed() {
		return r.ch, nil
	}
	r.closeLocked()

	conn, err := amqp.DialConfig(r.url, amqp.Config{
		Dial:      amqp.DefaultDial(r.dialTimeout),
		Heartbeat: 10 * time.Second,
		Locale:    "en_US",
		// Names the connection in the management UI, so "which of these six is
		// publishing" is answerable without guessing from the port.
		Properties: amqp.Table{"connection_name": "trip-outbox-relay"},
	})
	if err != nil {
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open amqp channel: %w", err)
	}
	// Publisher confirms. Without them a publish is fire-and-forget and
	// published_at would mean "we wrote to a socket", which is not the same
	// claim at all.
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("put channel into confirm mode: %w", err)
	}

	// The exchange is not declared. It is the broker's, imported from
	// definitions.json before any service connects.

	r.conn, r.ch = conn, ch
	r.connected.Store(true)
	r.logger.Info("outbox relay connected to rabbitmq", slog.String("exchange", r.exchange))
	return ch, nil
}

// dropConnection forces the next tick to redial.
//
// A publish or a confirm that failed may have left the channel in an
// unrecoverable state — amqp091 closes a channel on any protocol error — and
// carrying on with it would fail every subsequent batch identically. Throwing
// it away costs one dial.
func (r *Relay) dropConnection() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked()
}

func (r *Relay) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeLocked()
}

func (r *Relay) closeLocked() {
	if r.ch != nil {
		_ = r.ch.Close()
		r.ch = nil
	}
	if r.conn != nil {
		_ = r.conn.Close()
		r.conn = nil
	}
	r.connected.Store(false)
}

// Ping reports whether the broker is reachable, for /readyz on a service that
// has not needed to publish anything yet.
func (r *Relay) Ping(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		_, err := r.channel()
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errors.New("broker check timed out")
	}
}

func orDefaultDuration(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

func orDefaultInt(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func orDefaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
