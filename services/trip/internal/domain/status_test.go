package domain_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
)

// The transition table, written out a second time by hand.
//
// Restating it rather than importing it is the point: a test that iterated over
// the same map the implementation reads would pass no matter what that map said.
// These are the edges the specification asks for, and every one of the 25
// ordered pairs is checked against them below.
var allowedEdges = map[domain.Status]map[domain.Status]bool{
	domain.StatusDraft: {
		domain.StatusRecruiting: true,
		domain.StatusCancelled:  true,
	},
	domain.StatusRecruiting: {
		domain.StatusInProgress: true,
		domain.StatusCancelled:  true,
	},
	domain.StatusInProgress: {
		domain.StatusCompleted: true,
		domain.StatusCancelled: true,
	},
	domain.StatusCompleted: {},
	domain.StatusCancelled: {},
}

// TestCanCoversEveryOrderedPair walks the full 5x5 matrix, so both directions of
// the acceptance criterion are covered: every allowed transition is allowed, and
// every one of the twenty others is refused.
func TestCanCoversEveryOrderedPair(t *testing.T) {
	statuses := domain.AllStatuses()
	require.Len(t, statuses, 5, "the vocabulary changed; update this test's table")

	checked := 0
	for _, from := range statuses {
		for _, to := range statuses {
			from, to := from, to
			want := allowedEdges[from][to]

			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				require.Equal(t, want, domain.Can(from, to))
			})
			checked++
		}
	}
	require.Equal(t, 25, checked)
}

// TestAllowedTransitions names each legal edge on its own, so a failure reads as
// "draft cannot be published" rather than as a coordinate in a matrix.
func TestAllowedTransitions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to domain.Status
	}{
		{"a draft is published", domain.StatusDraft, domain.StatusRecruiting},
		{"a draft is abandoned", domain.StatusDraft, domain.StatusCancelled},
		{"a recruiting trip begins", domain.StatusRecruiting, domain.StatusInProgress},
		{"a recruiting trip is called off", domain.StatusRecruiting, domain.StatusCancelled},
		{"a trip under way finishes", domain.StatusInProgress, domain.StatusCompleted},
		{"a trip under way is abandoned", domain.StatusInProgress, domain.StatusCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.True(t, domain.Can(tc.from, tc.to))
		})
	}
}

// TestForbiddenTransitions names the twenty refused edges. The interesting ones
// are the terminal states and the backwards edges; the self-edges are here
// because "publish an already-recruiting trip" has to be a conflict rather than
// a silent success, which is what makes a stale client find out.
func TestForbiddenTransitions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to domain.Status
	}{
		// Self-edges: not a no-op, a conflict.
		{"draft to draft", domain.StatusDraft, domain.StatusDraft},
		{"recruiting to recruiting", domain.StatusRecruiting, domain.StatusRecruiting},
		{"in_progress to in_progress", domain.StatusInProgress, domain.StatusInProgress},
		{"completed to completed", domain.StatusCompleted, domain.StatusCompleted},
		{"cancelled to cancelled", domain.StatusCancelled, domain.StatusCancelled},

		// Skipping a state.
		{"draft straight to in_progress", domain.StatusDraft, domain.StatusInProgress},
		{"draft straight to completed", domain.StatusDraft, domain.StatusCompleted},
		{"recruiting straight to completed", domain.StatusRecruiting, domain.StatusCompleted},

		// Going backwards.
		{"recruiting back to draft", domain.StatusRecruiting, domain.StatusDraft},
		{"in_progress back to draft", domain.StatusInProgress, domain.StatusDraft},
		{"in_progress back to recruiting", domain.StatusInProgress, domain.StatusRecruiting},

		// Leaving a terminal state — completed.
		{"completed to draft", domain.StatusCompleted, domain.StatusDraft},
		{"completed to recruiting", domain.StatusCompleted, domain.StatusRecruiting},
		{"completed to in_progress", domain.StatusCompleted, domain.StatusInProgress},
		{"completed to cancelled", domain.StatusCompleted, domain.StatusCancelled},

		// Leaving a terminal state — cancelled. The last of these is the
		// acceptance criterion behind "publishing a cancelled trip is a 409".
		{"cancelled to draft", domain.StatusCancelled, domain.StatusDraft},
		{"cancelled to in_progress", domain.StatusCancelled, domain.StatusInProgress},
		{"cancelled to completed", domain.StatusCancelled, domain.StatusCompleted},
		{"cancelled to recruiting", domain.StatusCancelled, domain.StatusRecruiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, domain.Can(tc.from, tc.to))
		})
	}
}

// TestCanFailsClosedOnUnknownStatus: a status that reached the row outside the
// vocabulary — a hand-written UPDATE, a botched migration — must not become a
// wildcard that permits every transition.
func TestCanFailsClosedOnUnknownStatus(t *testing.T) {
	rogue := domain.Status("archived")

	require.False(t, domain.KnownStatus(rogue))
	for _, to := range domain.AllStatuses() {
		require.False(t, domain.Can(rogue, to), "unknown status must have no outgoing edges")
	}
	require.False(t, domain.Can(domain.StatusDraft, rogue), "no edge may lead to an unknown status")
	require.Empty(t, domain.AllowedFrom(rogue))
}

func TestIsTerminal(t *testing.T) {
	require.True(t, domain.IsTerminal(domain.StatusCompleted))
	require.True(t, domain.IsTerminal(domain.StatusCancelled))
	require.False(t, domain.IsTerminal(domain.StatusDraft))
	require.False(t, domain.IsTerminal(domain.StatusRecruiting))
	require.False(t, domain.IsTerminal(domain.StatusInProgress))
	require.False(t, domain.IsTerminal(domain.Status("archived")), "an unknown status is not terminal, it is unknown")
}

func TestIsEditable(t *testing.T) {
	require.True(t, domain.IsEditable(domain.StatusDraft))
	require.True(t, domain.IsEditable(domain.StatusRecruiting))
	require.False(t, domain.IsEditable(domain.StatusInProgress))
	require.False(t, domain.IsEditable(domain.StatusCompleted))
	require.False(t, domain.IsEditable(domain.StatusCancelled))
}

func TestAllowedFromMatchesCan(t *testing.T) {
	for _, from := range domain.AllStatuses() {
		for _, to := range domain.AllowedFrom(from) {
			require.True(t, domain.Can(from, to), "AllowedFrom listed an edge Can refuses")
		}
		require.Len(t, domain.AllowedFrom(from), len(allowedEdges[from]))
	}
}

// TestAllowedFromCannotExtendTheStateMachine: the slice is a copy, so a caller
// appending to it does not add an edge for everybody else.
func TestAllowedFromCannotExtendTheStateMachine(t *testing.T) {
	edges := domain.AllowedFrom(domain.StatusCompleted)
	edges = append(edges, domain.StatusDraft) //nolint:staticcheck // the append is the test
	_ = edges

	require.False(t, domain.Can(domain.StatusCompleted, domain.StatusDraft))
	require.Empty(t, domain.AllowedFrom(domain.StatusCompleted))
}

// TestEveryNonTerminalStatusCanBeCancelled is the property the cancel endpoint
// depends on: a trip that has not already ended can always be called off.
func TestEveryNonTerminalStatusCanBeCancelled(t *testing.T) {
	for _, from := range domain.AllStatuses() {
		if domain.IsTerminal(from) {
			require.False(t, domain.Can(from, domain.StatusCancelled))
			continue
		}
		require.True(t, domain.Can(from, domain.StatusCancelled), "%s must be cancellable", from)
	}
}

// TestEveryStatusIsReachableFromDraft: no state in the table is stranded, which
// would mean a lifecycle nothing can ever enter.
func TestEveryStatusIsReachableFromDraft(t *testing.T) {
	seen := map[domain.Status]bool{domain.StatusDraft: true}
	queue := []domain.Status{domain.StatusDraft}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, next := range domain.AllowedFrom(current) {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}

	for _, status := range domain.AllStatuses() {
		require.True(t, seen[status], "%s is unreachable from draft", status)
	}
}
