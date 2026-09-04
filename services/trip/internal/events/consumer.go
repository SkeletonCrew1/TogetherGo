package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Queue is the trip service's inbox. One queue for the whole service rather
// than one per event type: contracts/events.md fixes that shape, and the
// dispatch on event_type happens below rather than in the broker.
const Queue = "trip.user-events"

// Projection is the consumer's half of the store.
//
// Declared here and implemented in internal/store because the dependency has to
// run this way round: internal/store imports this package for the outbox
// payloads, so this package cannot import internal/store. Each method performs
// its idempotency check and its projection write in one transaction and reports
// whether this delivery was the first — CLAUDE.md rule 5.
type Projection interface {
	ApplyProfileUpdate(ctx context.Context, eventID uuid.UUID, payload UserProfileUpdated) (bool, error)
	ApplyRatingUpdate(ctx context.Context, eventID uuid.UUID, payload UserRatingUpdated) (bool, error)

	// MarkProcessed records an event that needs no write — an event_type this
	// build does not recognise.
	MarkProcessed(ctx context.Context, eventID uuid.UUID) (bool, error)
}

// Consumer reads trip.user-events and projects it into user_ref.
//
// It is the mirror image of Relay and shares its shape deliberately: a
// goroutine inside this service, a lazily established connection with backoff,
// and no topology declaration ever. The broker imports exchanges, queues and
// bindings from deploy/rabbitmq/definitions.json before any service connects; a
// consumer that declared its own queue would eventually declare it with
// different arguments and earn a PRECONDITION_FAILED.
//
// Delivery is at-least-once, which is the contract every publisher in this
// system is written to. The duplicate absorber is processed_events, not
// anything on this side of the wire.
type Consumer struct {
	projection Projection
	url        string
	queue      string
	logger     *slog.Logger

	prefetch     int
	handlerLimit time.Duration
	dialTimeout  time.Duration
	maxBackoff   time.Duration
	baseBackoff  time.Duration
	retryBase    time.Duration

	mu        sync.Mutex
	conn      *amqp.Connection
	ch        *amqp.Channel
	connected atomic.Bool
}

// ConsumerOptions configures a Consumer. Only Projection and AMQPURL are
// required.
type ConsumerOptions struct {
	Projection Projection
	AMQPURL    string
	Queue      string
	Logger     *slog.Logger

	// Prefetch bounds how many unacked deliveries the broker will hand over.
	// Small: the handler is a single short transaction, and a large window
	// would only mean more messages to redeliver after a crash.
	Prefetch int

	// HandlerTimeout bounds one message's database work, so a stuck
	// transaction becomes a redelivery rather than a consumer that has stopped
	// consuming without saying so.
	HandlerTimeout time.Duration

	// RetryBase is the first backoff in the in-handler retry ladder; each
	// subsequent attempt doubles it. Configurable only so the tests do not
	// spend 1.4 seconds proving a message reaches the DLQ.
	RetryBase time.Duration

	DialTimeout time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

const (
	// defaultPrefetch is the 10 contracts/events.md fixes for every consumer in
	// the system. Unacked messages beyond it stay on the broker, so a restart
	// redelivers them and a slow consumer accumulates no unbounded backlog.
	defaultPrefetch = 10

	// handlerAttempts is one try plus the three retries the contract
	// prescribes for a transient failure. After the fourth, the message is
	// dead-lettered.
	handlerAttempts = 4

	// defaultRetryBase is the delay before the second attempt; the third and
	// fourth double it, giving the 200/400/800 ms ladder from the contract.
	defaultRetryBase = 200 * time.Millisecond

	// retryJitter is the ±20 % the contract asks for, so replicas that failed
	// together do not retry in lockstep.
	retryJitter = 0.2

	// defaultHandlerTimeout bounds one message including its retries: roughly
	// 1.4 s of backoff plus the work itself, with room to spare.
	defaultHandlerTimeout = 10 * time.Second

	defaultBaseBackoff        = time.Second
	defaultConsumerMaxBackoff = 30 * time.Second
)

// NewConsumer builds a consumer. It opens no connection: the broker is
// contacted from Run, so a trip service that starts before RabbitMQ starts
// anyway and catches up.
func NewConsumer(opts ConsumerOptions) *Consumer {
	c := &Consumer{
		projection:   opts.Projection,
		url:          opts.AMQPURL,
		queue:        orDefaultString(opts.Queue, Queue),
		logger:       opts.Logger,
		prefetch:     orDefaultInt(opts.Prefetch, defaultPrefetch),
		handlerLimit: orDefaultDuration(opts.HandlerTimeout, defaultHandlerTimeout),
		dialTimeout:  orDefaultDuration(opts.DialTimeout, defaultDialTimeout),
		baseBackoff:  orDefaultDuration(opts.BaseBackoff, defaultBaseBackoff),
		maxBackoff:   orDefaultDuration(opts.MaxBackoff, defaultConsumerMaxBackoff),
		retryBase:    orDefaultDuration(opts.RetryBase, defaultRetryBase),
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c
}

// Connected reports whether the consumer currently holds a live channel.
func (c *Consumer) Connected() bool { return c.connected.Load() }

// Run consumes until ctx is cancelled.
//
// The loop is "connect, drain until something breaks, back off, connect again".
// A broker restart therefore costs one backoff and no supervision: there is no
// health check that restarts this service and no separate state machine to get
// wrong. Unacked deliveries are redelivered by the broker when the connection
// drops, and processed_events makes that harmless.
func (c *Consumer) Run(ctx context.Context) {
	c.logger.Info("user projection consumer started", slog.String("queue", c.queue))

	delay := time.Duration(0)
	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			c.close()
			c.logger.Info("user projection consumer stopped")
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			continue
		}

		if err := c.consume(ctx); err != nil && ctx.Err() == nil {
			delay = nextDelay(delay, c.baseBackoff, c.maxBackoff)
			c.logger.Warn("user projection consumer disconnected",
				slog.String("error", err.Error()),
				slog.Duration("retry_in", delay),
			)
		} else {
			delay = c.baseBackoff
		}
		timer.Reset(delay)
	}
}

// consume opens a channel and processes deliveries until the channel closes or
// the context is cancelled. Returning nil means "cancelled"; anything else is
// worth a backoff.
func (c *Consumer) consume(ctx context.Context) error {
	ch, err := c.channel()
	if err != nil {
		return err
	}

	// Manual ack, always. An auto-ack consumer acknowledges on delivery, which
	// would lose every in-flight message on a crash — the exact failure the
	// outbox on the publishing side went to such lengths to rule out.
	deliveries, err := ch.Consume(c.queue, "trip-user-projection", false, false, false, false, nil)
	if err != nil {
		c.dropConnection()
		return fmt.Errorf("consume %s: %w", c.queue, err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case delivery, open := <-deliveries:
			if !open {
				c.dropConnection()
				return errors.New("delivery channel closed")
			}
			c.handleDelivery(ctx, delivery)
		}
	}
}

// handleDelivery processes one message and decides its fate.
//
// Two outcomes, because `requeue=true` is not one of them. contracts/events.md
// is blunt about why: a requeue puts the message back at the head of the same
// queue, it is redelivered at once, it fails again, and the consumer spins at
// full speed on a poison message. Retrying happens here, where the backoff is
// under this service's control, and what survives that goes to the DLQ.
//
//   - **Ack.** The event was applied, or was a duplicate, or was of a type this
//     build does not know. The contract is explicit about the last one: log at
//     warn, mark processed, ack. An unknown event is not an error, it is a
//     message meant for a newer version of this service, and dead-lettering it
//     would fill a DLQ with messages nobody will ever act on.
//
//   - **Dead-letter** (nack, no requeue). Either the message will never be
//     processable — a body that is not JSON, an envelope with no event_id — or
//     it is good but four attempts against an unavailable database were not
//     enough. The DLQ is a human queue: nothing drains it on a timer, and a
//     message in it is an alert. Replaying from it is safe, because
//     processed_events makes a replay a no-op for anything already handled.
func (c *Consumer) handleDelivery(ctx context.Context, delivery amqp.Delivery) {
	var envelope Envelope
	if err := json.Unmarshal(delivery.Body, &envelope); err != nil {
		c.deadLetter(delivery, "body is not a valid event envelope", err)
		return
	}
	if envelope.EventID == uuid.Nil {
		c.deadLetter(delivery, "envelope carries no event_id", nil)
		return
	}

	log := c.logger.With(
		slog.String("event_id", envelope.EventID.String()),
		slog.String("event_type", envelope.EventType),
	)

	// Detached from the consumer's context deliberately: a message already
	// taken off the wire should finish its transaction during a shutdown rather
	// than be abandoned half-done and redelivered on the next boot.
	handlerCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.handlerLimit)
	defer cancel()

	applied, err := c.applyWithRetries(handlerCtx, envelope, log)
	if err != nil {
		var permanent *permanentError
		if errors.As(err, &permanent) {
			c.deadLetter(delivery, permanent.reason, permanent.cause)
			return
		}
		c.deadLetter(delivery, "transient failure outlived every retry", err)
		return
	}

	if err := delivery.Ack(false); err != nil {
		// The work is committed and processed_events holds the id, so a
		// redelivery after a failed ack is absorbed. Nothing to undo.
		log.Error("ack failed; the event is applied and a redelivery will be ignored",
			slog.String("error", err.Error()))
		return
	}
	if !applied {
		log.Debug("user event already processed, ignored")
	}
}

// applyWithRetries runs the handler up to handlerAttempts times, backing off
// between transient failures.
//
// A permanent failure returns immediately: retrying a body that is not JSON
// three more times is three more log lines and the same answer. The backoff
// ladder is the contract's — 200, 400, 800 ms — with ±20 % jitter so that
// replicas which failed on the same database outage do not come back in
// lockstep and knock it over again.
func (c *Consumer) applyWithRetries(ctx context.Context, envelope Envelope, log *slog.Logger) (bool, error) {
	delay := c.retryBase
	var lastErr error

	for attempt := 1; attempt <= handlerAttempts; attempt++ {
		applied, err := c.apply(ctx, envelope, log)
		if err == nil {
			return applied, nil
		}

		var permanent *permanentError
		if errors.As(err, &permanent) {
			return false, err
		}
		lastErr = err

		if attempt == handlerAttempts {
			break
		}
		log.Warn("user event failed, retrying",
			slog.Int("attempt", attempt),
			slog.Duration("retry_in", delay),
			slog.String("error", err.Error()),
		)
		select {
		case <-time.After(jittered(delay)):
		case <-ctx.Done():
			return false, fmt.Errorf("%w (handler context ended during retry backoff)", lastErr)
		}
		delay *= 2
	}
	return false, fmt.Errorf("gave up after %d attempts: %w", handlerAttempts, lastErr)
}

// jittered spreads a delay uniformly over ±retryJitter.
//
// math/rand rather than crypto/rand: this is scheduling noise, not a secret,
// and the only property it needs is that two processes do not agree.
func jittered(delay time.Duration) time.Duration {
	spread := float64(delay) * retryJitter
	return time.Duration(float64(delay) - spread + rand.Float64()*2*spread)
}

// apply dispatches on event_type and returns whether the payload was actually
// written (false for a duplicate).
func (c *Consumer) apply(ctx context.Context, envelope Envelope, log *slog.Logger) (bool, error) {
	switch envelope.EventType {
	case TypeUserProfileUpdated:
		var payload UserProfileUpdated
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return false, &permanentError{reason: "profile payload is malformed", cause: err}
		}
		if payload.UserID == uuid.Nil {
			return false, &permanentError{reason: "profile payload carries no user_id"}
		}
		return c.projection.ApplyProfileUpdate(ctx, envelope.EventID, payload)

	case TypeUserRatingUpdated:
		var payload UserRatingUpdated
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return false, &permanentError{reason: "rating payload is malformed", cause: err}
		}
		if payload.UserID == uuid.Nil {
			return false, &permanentError{reason: "rating payload carries no user_id"}
		}
		return c.projection.ApplyRatingUpdate(ctx, envelope.EventID, payload)

	default:
		// Not an error. The binding could be widened, or a newer build could be
		// publishing something this one has never heard of; either way the
		// message is recorded as handled so a redelivery does not re-log it,
		// and acked so it does not pile up.
		log.Warn("unknown event type on the user queue, ignored")
		return c.projection.MarkProcessed(ctx, envelope.EventID)
	}
}

// permanentError marks a failure that will still fail on redelivery.
type permanentError struct {
	reason string
	cause  error
}

func (e *permanentError) Error() string {
	if e.cause == nil {
		return e.reason
	}
	return e.reason + ": " + e.cause.Error()
}

func (e *permanentError) Unwrap() error { return e.cause }

// deadLetter nacks without requeue, which sends the message to
// trip.user-events.dlq via the queue's x-dead-letter-routing-key.
func (c *Consumer) deadLetter(delivery amqp.Delivery, reason string, cause error) {
	attrs := []any{
		slog.String("reason", reason),
		slog.String("message_id", delivery.MessageId),
	}
	if cause != nil {
		attrs = append(attrs, slog.String("error", cause.Error()))
	}
	// The body is not logged. It belongs to another service and the rule about
	// never logging full request bodies applies to messages too; the DLQ holds
	// the message itself for whoever needs to read it.
	c.logger.Error("dead-lettering user event", attrs...)

	if err := delivery.Nack(false, false); err != nil {
		c.logger.Error("dead-letter nack failed", slog.String("error", err.Error()))
	}
}

// channel returns a live consuming channel, dialling if there is not one.
func (c *Consumer) channel() (*amqp.Channel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.ch != nil && !c.ch.IsClosed() && c.conn != nil && !c.conn.IsClosed() {
		return c.ch, nil
	}
	c.closeLocked()

	conn, err := amqp.DialConfig(c.url, amqp.Config{
		Dial:       amqp.DefaultDial(c.dialTimeout),
		Heartbeat:  10 * time.Second,
		Locale:     "en_US",
		Properties: amqp.Table{"connection_name": "trip-user-projection"},
	})
	if err != nil {
		return nil, fmt.Errorf("dial rabbitmq: %w", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open amqp channel: %w", err)
	}
	if err := ch.Qos(c.prefetch, 0, false); err != nil {
		_ = ch.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("set prefetch: %w", err)
	}

	// The queue is not declared. It is the broker's, imported from
	// definitions.json before any service connects.

	c.conn, c.ch = conn, ch
	c.connected.Store(true)
	c.logger.Info("user projection consumer connected to rabbitmq", slog.String("queue", c.queue))
	return ch, nil
}

func (c *Consumer) dropConnection() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *Consumer) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *Consumer) closeLocked() {
	if c.ch != nil {
		_ = c.ch.Close()
		c.ch = nil
	}
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connected.Store(false)
}
