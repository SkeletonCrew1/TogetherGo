package scheduler_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/scheduler"
)

// The loop, with no database under it.
//
// What the store's own tests cover is what a pass *does*; what these cover is
// when it is called, with what, and what happens when it fails — which is the
// whole of this package.

// fakeLifecycle records every call and answers from a script.
type fakeLifecycle struct {
	mu      sync.Mutex
	calls   int
	batches []int
	result  domain.LifecycleResult
	err     error

	// ticked is signalled once per call, so a test can wait for the loop to
	// come round instead of sleeping and hoping.
	ticked chan struct{}
}

func newFakeLifecycle() *fakeLifecycle {
	return &fakeLifecycle{ticked: make(chan struct{}, 64)}
}

func (f *fakeLifecycle) LifecycleTick(_ context.Context, batch int) (domain.LifecycleResult, error) {
	f.mu.Lock()
	f.calls++
	f.batches = append(f.batches, batch)
	result, err := f.result, f.err
	f.mu.Unlock()

	select {
	case f.ticked <- struct{}{}:
	default:
	}
	return result, err
}

func (f *fakeLifecycle) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func quiet() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// awaitTicks waits for n passes, or fails.
func awaitTicks(t *testing.T, f *fakeLifecycle, n int) {
	t.Helper()

	for i := 0; i < n; i++ {
		select {
		case <-f.ticked:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d passes ran", i, n)
		}
	}
}

// run starts the loop for the duration of the test and returns a stop that
// waits for it, so assertions are made against a scheduler that has finished
// rather than against one that is still going.
func run(t *testing.T, s *scheduler.Scheduler) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Run(ctx)
	}()

	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("scheduler did not stop within 10s")
		}
	}
	t.Cleanup(stop)
	return stop
}

// TestTheFirstPassRunsImmediately. A service that has just come up after an
// outage has a backlog, and making it wait a full interval before noticing
// would be a choice with nothing behind it.
func TestTheFirstPassRunsImmediately(t *testing.T) {
	lifecycle := newFakeLifecycle()
	s := scheduler.New(scheduler.Options{
		Lifecycle: lifecycle,
		Interval:  time.Hour, // long enough that a second pass cannot be why this passes
		Logger:    quiet(),
	})
	stop := run(t, s)

	awaitTicks(t, lifecycle, 1)
	stop()
	require.Equal(t, 1, lifecycle.callCount())
}

// TestTheLoopKeepsTicking at the configured interval — the reason the interval
// is configurable at all.
func TestTheLoopKeepsTicking(t *testing.T) {
	lifecycle := newFakeLifecycle()
	s := scheduler.New(scheduler.Options{
		Lifecycle: lifecycle,
		Interval:  20 * time.Millisecond,
		Logger:    quiet(),
	})
	run(t, s)

	awaitTicks(t, lifecycle, 3)
}

// TestAFailedPassDoesNotStopTheLoop. The trips are still there and still due;
// the next tick is the retry, which is the same reasoning the outbox relay
// applies to a broker that is briefly down.
func TestAFailedPassDoesNotStopTheLoop(t *testing.T) {
	lifecycle := newFakeLifecycle()
	lifecycle.err = errors.New("database unavailable")

	s := scheduler.New(scheduler.Options{
		Lifecycle: lifecycle,
		Interval:  20 * time.Millisecond,
		Logger:    quiet(),
	})
	run(t, s)

	awaitTicks(t, lifecycle, 3)
}

// TestTickIsTheSameCallTheLoopMakes. The debug endpoint must not be a second
// path through the transitions, or it would be testing something production
// does not run.
func TestTickIsTheSameCallTheLoopMakes(t *testing.T) {
	lifecycle := newFakeLifecycle()
	lifecycle.result = domain.LifecycleResult{Started: 2, Completed: 1, CompletedEvents: 1}

	s := scheduler.New(scheduler.Options{
		Lifecycle: lifecycle,
		Interval:  time.Hour,
		BatchSize: 7,
		Logger:    quiet(),
	})

	result, err := s.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, domain.LifecycleResult{Started: 2, Completed: 1, CompletedEvents: 1}, result)
	require.Equal(t, []int{7}, lifecycle.batches, "the configured batch size reaches the store")
}

// TestTickReportsAFailure rather than swallowing it: the endpoint's caller is
// an operator asking whether the scheduler works.
func TestTickReportsAFailure(t *testing.T) {
	lifecycle := newFakeLifecycle()
	lifecycle.err = errors.New("database unavailable")

	s := scheduler.New(scheduler.Options{Lifecycle: lifecycle, Logger: quiet()})

	_, err := s.Tick(context.Background())
	require.ErrorContains(t, err, "database unavailable")
}

// TestDefaultsAreTheDocumentedOnes. The zero Options is what an incomplete
// wiring produces, and it must be the sixty-second production loop rather than
// a busy-wait.
func TestDefaultsAreTheDocumentedOnes(t *testing.T) {
	lifecycle := newFakeLifecycle()
	s := scheduler.New(scheduler.Options{Lifecycle: lifecycle, Logger: quiet()})

	_, err := s.Tick(context.Background())
	require.NoError(t, err)
	require.Equal(t, []int{scheduler.DefaultBatchSize}, lifecycle.batches)
	require.Equal(t, 60*time.Second, scheduler.DefaultInterval)
}

// TestEmptyIsWhatKeepsTheLogQuiet. This runs every minute forever; an info line
// per tick would bury the ones that mean something.
func TestEmptyIsWhatKeepsTheLogQuiet(t *testing.T) {
	require.True(t, domain.LifecycleResult{}.Empty())
	require.False(t, domain.LifecycleResult{Started: 1}.Empty())
	require.False(t, domain.LifecycleResult{Completed: 1}.Empty())
}
