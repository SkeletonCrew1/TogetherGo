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

// Projection is the consumer's half of the store.
//
// Declared here and implemented in internal/store because the dependency runs
// this way round: internal/store imports this package for the event payloads,
// so this package cannot import internal/store. Each method performs its
// idempotency check and its projection write in one transaction and reports
// whether this delivery was the first — CLAUDE.md rule 5.
type Projection interface {
	ApplyTripCreated(ctx context.Context, eventID uuid.UUID, payload TripCreated) (bool, error)
	ApplyTripCancelled(ctx context.Context, eventID uuid.UUID, payload TripCancelled) (bool, error)
	ApplyJoinApproved(ctx context.Context, eventID uuid.UUID, payload JoinRequestApproved) (bool, error)
	ApplyParticipantRemoved(ctx context.Context, eventID uuid.UUID, payload ParticipantRemoved) (bool, error)

	// MarkProcessed records an event that needs no write — an event_type this
	// build does not recognise.
	MarkProcessed(ctx context.Context, eventID uuid.UUID) (bool, error)
}

// Evictor closes a user's open sockets in a room, on every replica.
//
// The `participant.removed` handler needs it, and it is an interface here for
// the same reason Projection is: internal/hub imports nothing from this
// package, and this package must not import it either — the two would otherwise
// be a cycle waiting for the first shared type.
//
// It is called *after* the transaction commits, never inside it. Closing a
// socket cannot be rolled back, so a transaction that failed afterwards would
// have thrown a member out of a room they were still in. This is the same rule
// contracts/events.md states for sending mail, and for the same reason.
type Evictor interface {
	Evict(ctx context.Context, tripID, userID uuid.UUID)
}

// Consumer reads chat.trip-events and projects it into rooms and room_members.
//
// A goroutine inside this service rather than a separate deployable, with a
// lazily established connection, backoff on failure, and no topology
// declaration ever. The broker imports exchanges, queues and
// bindings from deploy/rabbitmq/definitions.json before any service connects; a
// consumer that declared its own queue would eventually declare it with
// different arguments and earn a PRECONDITION_FAILED.
//
// Delivery is at-least-once, which is the contract every publisher in this
// system is written to. The duplicate absorber is processed_events, not
// anything on this side of the wire.
type Consumer struct {
	projection Projection
	evictor    Evictor
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

	// Evictor closes the sockets of a participant who has just been removed.
	// Optional: without one the projection still revokes access, and the
	// removed user's next message is refused — they simply keep *reading* the
	// room until their socket drops. Left nil in the projection tests, which
	// have no hub.
	Evictor Evictor

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

	// defaultDialTimeout bounds one attempt to reach the broker, so a
	// blackholed address becomes a backoff rather than a goroutine parked on a
	// TCP connect until the kernel gives up.
	defaultDialTimeout = 5 * time.Second
)

// NewConsumer builds a consumer. It opens no connection: the broker is
// contacted from Run, so a chat service that starts before RabbitMQ starts
// anyway and catches up.
func NewConsumer(opts ConsumerOptions) *Consumer {
	c := &Consumer{
		projection:   opts.Projection,
		evictor:      opts.Evictor,
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
	c.logger.Info("room projection consumer started", slog.String("queue", c.queue))

	delay := time.Duration(0)
	timer := time.NewTimer(delay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			c.close()
			c.logger.Info("room projection consumer stopped")
			return
		case <-timer.C:
		}
		if ctx.Err() != nil {
			continue
		}

		if err := c.consume(ctx); err != nil && ctx.Err() == nil {
			delay = nextDelay(delay, c.baseBackoff, c.maxBackoff)
			c.logger.Warn("room projection consumer disconnected",
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
	deliveries, err := ch.Consume(c.queue, "chat-room-projection", false, false, false, false, nil)
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
		log.Debug("trip event already processed, ignored")
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
		log.Warn("trip event failed, retrying",
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
//
// Four handlers, which are exactly the four routing keys bound to this
// service's queue in deploy/rabbitmq/definitions.json. Chat is deliberately not
// bound to `trip.status_changed`: a room is open or closed, and the only
// transition that closes one has its own event.
func (c *Consumer) apply(ctx context.Context, envelope Envelope, log *slog.Logger) (bool, error) {
	switch envelope.EventType {
	case TypeTripCreated:
		var payload TripCreated
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return false, &permanentError{reason: "trip.created payload is malformed", cause: err}
		}
		// A payload with no trip or no organizer cannot become a room, and no
		// number of retries will give it one — the contract's "referenced
		// aggregate cannot exist" case, straight to the DLQ.
		if payload.TripID == uuid.Nil || payload.OrganizerID == uuid.Nil {
			return false, &permanentError{reason: "trip.created payload carries no trip_id or organizer_id"}
		}
		return c.projection.ApplyTripCreated(ctx, envelope.EventID, payload)

	case TypeJoinRequestApproved:
		var payload JoinRequestApproved
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return false, &permanentError{reason: "join_request.approved payload is malformed", cause: err}
		}
		if payload.TripID == uuid.Nil || payload.ParticipantID == uuid.Nil {
			return false, &permanentError{reason: "join_request.approved payload carries no trip_id or participant_id"}
		}
		return c.projection.ApplyJoinApproved(ctx, envelope.EventID, payload)

	case TypeParticipantRemoved:
		var payload ParticipantRemoved
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return false, &permanentError{reason: "participant.removed payload is malformed", cause: err}
		}
		if payload.TripID == uuid.Nil || payload.ParticipantID == uuid.Nil {
			return false, &permanentError{reason: "participant.removed payload carries no trip_id or participant_id"}
		}

		applied, err := c.projection.ApplyParticipantRemoved(ctx, envelope.EventID, payload)
		if err != nil {
			return false, err
		}
		// After the commit, never inside it. Closing a socket cannot be rolled
		// back, so evicting first and then failing to commit would have thrown
		// somebody out of a room they were still a member of. A crash in this
		// window leaves the socket open until its next message is refused,
		// which is the correct side to err on.
		//
		// Skipped on a duplicate delivery: the eviction has already happened,
		// and the removed user may since have been re-approved.
		if applied && c.evictor != nil {
			c.evictor.Evict(ctx, payload.TripID, payload.ParticipantID)
		}
		return applied, nil

	case TypeTripCancelled:
		var payload TripCancelled
		if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
			return false, &permanentError{reason: "trip.cancelled payload is malformed", cause: err}
		}
		if payload.TripID == uuid.Nil {
			return false, &permanentError{reason: "trip.cancelled payload carries no trip_id"}
		}
		// Nobody is disconnected. The trip is off but the conversation about it
		// is not — contracts/events.md calls for the room to be closed to new
		// messages, not emptied, and people who are in it stay in it.
		return c.projection.ApplyTripCancelled(ctx, envelope.EventID, payload)

	default:
		// Not an error. The binding could be widened, or a newer build could be
		// publishing something this one has never heard of; either way the
		// message is recorded as handled so a redelivery does not re-log it,
		// and acked so it does not pile up.
		log.Warn("unknown event type on the chat queue, ignored")
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
// chat.trip-events.dlq via the queue's x-dead-letter-routing-key.
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
	c.logger.Error("dead-lettering trip event", attrs...)

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
		Properties: amqp.Table{"connection_name": "chat-room-projection"},
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
	c.logger.Info("room projection consumer connected to rabbitmq", slog.String("queue", c.queue))
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

// Ping reports whether this instance can reach the broker, for /readyz.
//
// It asks for a channel rather than sending anything: a consumer has nothing to
// publish, and establishing the channel is the same work a delivery would need.
// The goroutine is there because the dial has its own timeout and the caller's
// context deadline has to win if it is shorter.
func (c *Consumer) Ping(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		_, err := c.channel()
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errors.New("broker check timed out")
	}
}

// nextDelay doubles the backoff, starting from the base, and caps it.
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
