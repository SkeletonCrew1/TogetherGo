package domain

import (
	"time"

	"github.com/google/uuid"
)

// RatingWindow is how long after a trip completes its participants may still
// rate one another.
//
// It lives here, in the publisher, and travels on `trip.completed` as an
// absolute `rating_window_closes_at`. Identity enforces it; identity does not
// compute it. A duration duplicated as a constant in two languages is a
// duration that drifts, and "which service is right about when the window
// closed" is not a question worth being able to ask.
const RatingWindow = 14 * 24 * time.Hour

// LifecycleResult is what one scheduler pass did.
//
// Counts rather than ids: the pass is a batch and its interesting property is
// how much work it found, which is what the log line and the debug endpoint
// report. A pass that transitions nothing is the normal case and produces
// zeroes.
type LifecycleResult struct {
	// Started is the number of recruiting trips moved to in_progress because
	// their start_at had passed.
	Started int

	// Completed is the number of in_progress trips moved to completed because
	// their end_at had passed.
	Completed int

	// CompletedEvents is how many of those Completed trips actually emitted a
	// trip.completed. It is lower than Completed whenever a trip finished with
	// only its organizer aboard — there is nobody to rate, so no rating window
	// is opened — and the two numbers being visibly different is what makes
	// that rule observable in the logs rather than merely documented.
	CompletedEvents int
}

// Empty reports whether the pass found nothing to do, which is what the loop
// uses to decide between an info line and silence.
func (r LifecycleResult) Empty() bool {
	return r.Started == 0 && r.Completed == 0
}

// UserRef is this service's local copy of a user, projected from identity's
// user.profile_updated and user.rating_updated events.
//
// It is deliberately the same shape as what identity's `GET /internal/users`
// returns, because the two are alternative sources for the same read and the
// code path that merges them should not have to translate between two vocabularies.
type UserRef struct {
	UserID      uuid.UUID
	FullName    string
	PhotoURL    *string
	RatingAvg   *float64
	RatingCount int
}
