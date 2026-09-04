package domain

// Status is the lifecycle state of a trip.
//
// The set of states and the edges between them are declared once, here. Nothing
// else in the service is allowed to reason about which status may follow which
// — handlers ask Can, and the single writer of the status column (Store.
// ChangeStatus) is the only place that acts on the answer.
type Status string

const (
	StatusDraft      Status = "draft"
	StatusRecruiting Status = "recruiting"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusCancelled  Status = "cancelled"
)

// StatusInitial is the state every trip is created in. Creation is not a
// transition — there is no "from" — so it is named rather than left as a bare
// literal in the insert statement.
const StatusInitial = StatusDraft

// allowedTransitions is the state machine. A status mapped to an empty slice is
// terminal: no edge leaves it, including a self-edge.
//
// Reading it as edges:
//
//	draft       -> recruiting, cancelled
//	recruiting  -> in_progress, cancelled
//	in_progress -> completed, cancelled
//	completed   -> (terminal)
//	cancelled   -> (terminal)
//
// Note that a status is never allowed to transition to itself. Publishing an
// already-recruiting trip is a client mistake, not a no-op, and answering it
// with 409 tells the caller its view of the trip is stale.
var allowedTransitions = map[Status][]Status{
	StatusDraft:      {StatusRecruiting, StatusCancelled},
	StatusRecruiting: {StatusInProgress, StatusCancelled},
	StatusInProgress: {StatusCompleted, StatusCancelled},
	StatusCompleted:  {},
	StatusCancelled:  {},
}

// editableStatuses are the states in which a trip's own fields may still be
// changed. Once a trip is under way its route and dates are history, and the
// participants who joined on the strength of them must not have them rewritten.
var editableStatuses = map[Status]bool{
	StatusDraft:      true,
	StatusRecruiting: true,
}

// Can reports whether `from` -> `to` is a legal transition.
//
// An unknown `from` has no entry in the map and therefore no outgoing edges, so
// a status that somehow reached the database outside the vocabulary fails
// closed instead of permitting everything.
func Can(from, to Status) bool {
	for _, candidate := range allowedTransitions[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

// AllowedFrom returns the statuses reachable from s, as a copy: callers that
// append to the result must not be able to extend the state machine.
func AllowedFrom(s Status) []Status {
	edges := allowedTransitions[s]
	out := make([]Status, len(edges))
	copy(out, edges)
	return out
}

// IsTerminal reports whether no transition leaves s.
func IsTerminal(s Status) bool {
	_, known := allowedTransitions[s]
	return known && len(allowedTransitions[s]) == 0
}

// IsEditable reports whether a trip in this status may still have its title,
// dates, capacity or route changed.
func IsEditable(s Status) bool {
	return editableStatuses[s]
}

// KnownStatus reports whether s is part of the vocabulary at all.
func KnownStatus(s Status) bool {
	_, known := allowedTransitions[s]
	return known
}

// AllStatuses returns every status in lifecycle order. Used by tests to prove
// the transition table is exhaustive, and by nothing else.
func AllStatuses() []Status {
	return []Status{StatusDraft, StatusRecruiting, StatusInProgress, StatusCompleted, StatusCancelled}
}
