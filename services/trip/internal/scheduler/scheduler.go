// Package scheduler moves trips along their lifecycle when their clock runs
// out: recruiting -> in_progress once start_at has passed, in_progress ->
// completed once end_at has.
//
// It owns no rules. Which edges are legal is internal/domain's answer, and the
// write itself is internal/store's — the scheduler is a timer with a batch size
// attached. That is the whole design: the automatic transitions go through the
// same code the organizer's own publish and cancel go through, so there is one
// description of the lifecycle rather than one for people and one for the
// clock.
//
// Two properties make it safe to run more than one replica:
//
//   - The claim query is `SELECT ... FOR UPDATE SKIP LOCKED` over a bounded
//     batch, so a second scheduler ticking at the same instant takes different
//     trips rather than blocking on these or failing on them.
//   - `trip.completed` — the event that opens the rating window in identity —
//     is guarded by a column, not by a read. See store.emitTripCompleted.
//
// Neither needs a leader election, an advisory lock, or any coordination
// between instances.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/togethergo/trip/internal/domain"
)

// Lifecycle is the store, as this package needs it. An interface so the loop
// can be tested without a database and so this package does not import
// internal/store just to name a type.
type Lifecycle interface {
	LifecycleTick(ctx context.Context, batch int) (domain.LifecycleResult, error)
}

// Scheduler runs the lifecycle pass on an interval.
type Scheduler struct {
	lifecycle Lifecycle
	interval  time.Duration
	batch     int
	logger    *slog.Logger
}

// Options configures a Scheduler. Only Lifecycle is required.
type Options struct {
	Lifecycle Lifecycle
	Logger    *slog.Logger

	// Interval is how often the pass runs. Sixty seconds by default, which is
	// the resolution the product needs: nothing depends on a trip becoming
	// `in_progress` in the same second its start time passes, and a tighter
	// loop would be two more queries a second against a table whose answer is
	// almost always empty.
	Interval time.Duration

	// BatchSize bounds one pass. Deliberately small — a pass holds row locks on
	// everything it claims, and a backlog is better drained over several ticks
	// than in one long transaction. A full batch means there is more waiting,
	// and the next tick takes it.
	BatchSize int
}

const (
	// DefaultInterval is the tick interval when none is configured.
	DefaultInterval = 60 * time.Second

	// DefaultBatchSize is the number of trips one pass may claim.
	DefaultBatchSize = 100
)

// New builds a scheduler. It starts nothing; Run does.
func New(opts Options) *Scheduler {
	s := &Scheduler{
		lifecycle: opts.Lifecycle,
		interval:  opts.Interval,
		batch:     opts.BatchSize,
		logger:    opts.Logger,
	}
	if s.interval <= 0 {
		s.interval = DefaultInterval
	}
	if s.batch <= 0 {
		s.batch = DefaultBatchSize
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

// Run ticks until ctx is cancelled. It blocks, so main runs it in a goroutine.
//
// The first pass happens immediately rather than one interval in. A service
// that has just come up after being down for an hour has a backlog, and making
// it wait a minute before noticing would be a choice with nothing behind it.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("lifecycle scheduler started",
		slog.Duration("interval", s.interval),
		slog.Int("batch_size", s.batch),
	)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		s.tickAndLog(ctx)

		select {
		case <-ctx.Done():
			s.logger.Info("lifecycle scheduler stopped")
			return
		case <-ticker.C:
		}
	}
}

// tickAndLog runs one pass and reports it. A pass that found nothing says
// nothing: this runs every minute forever, and an info line per tick would bury
// the ones that mean something.
func (s *Scheduler) tickAndLog(ctx context.Context) {
	result, err := s.Tick(ctx)
	if err != nil {
		// Warn rather than error, and no retry. The trips are still there, they
		// are still due, and the next tick is the retry — the same reasoning the
		// outbox relay uses about a broker that is briefly down.
		s.logger.Warn("lifecycle pass failed",
			slog.String("error", err.Error()),
			slog.Int("started", result.Started),
			slog.Int("completed", result.Completed),
		)
		return
	}
	if result.Empty() {
		return
	}
	s.logger.Info("lifecycle pass",
		slog.Int("started", result.Started),
		slog.Int("completed", result.Completed),
		// Lower than `completed` whenever a trip finished with only its
		// organizer aboard: there is nobody to rate, so no rating window is
		// opened. Logged so that rule is observable and not merely documented.
		slog.Int("completed_events", result.CompletedEvents),
	)
}

// Tick runs a single pass synchronously and returns what it did.
//
// Exported for the debug endpoint (`POST /internal/scheduler/tick`) and for the
// tests, which would otherwise have to sleep out a tick interval to observe
// anything. It is the same call the loop makes — there is no test-only path
// through the transitions.
func (s *Scheduler) Tick(ctx context.Context) (domain.LifecycleResult, error) {
	return s.lifecycle.LifecycleTick(ctx, s.batch)
}
