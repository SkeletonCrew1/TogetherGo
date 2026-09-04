package domain

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// The dashboard read model — GET /api/my/trips — and the viewer block the trip
// detail page needs.
//
// Discovery answers "what is out there"; this answers "where am I". The two are
// different questions and the difference is not a filter: discovery is scoped
// in SQL to recruiting trips that have not left yet (see store.discoveryScope),
// and no combination of its query parameters can widen that. A draft, a
// finished trip, a trip somebody has only applied to — none of them are things
// the browse page is allowed to return, and all of them are things the
// dashboard exists to show.

// DashboardRole is which of the two tabs is being asked for. It is required and
// closed: there is no "everything" tab, because the two halves have different
// scopes, different extra fields and different visibility rules, and a caller
// that has not said which one it wants has not asked a well-formed question.
type DashboardRole string

const (
	DashboardOrganizer   DashboardRole = "organizer"
	DashboardParticipant DashboardRole = "participant"
)

// DashboardRoles is the vocabulary, for the error message that lists it.
var DashboardRoles = []DashboardRole{DashboardOrganizer, DashboardParticipant}

// MembershipStatus is the caller's relationship to one trip on their dashboard.
//
// Deliberately not the same vocabulary as JoinRequestStatus. A dashboard row is
// about standing, not about paperwork: `requested` is "you have asked and
// nobody has answered", and there is no `rejected` or `cancelled` member
// because a trip you were turned down for is not one of your trips. The join
// request's own status is available on the trip detail endpoint, which is where
// a client that needs it is going anyway.
type MembershipStatus string

const (
	MembershipOrganizer MembershipStatus = "organizer"
	MembershipApproved  MembershipStatus = "approved"
	MembershipRequested MembershipStatus = "requested"
)

// The dashboard's page bounds, which are discovery's numbers. Named separately
// because they are this endpoint's contract and not a borrowed one — if the two
// ever have to diverge, the constant to change is already here.
const (
	MyTripsLimitDefault = SearchLimitDefault
	MyTripsLimitMax     = SearchLimitMax
)

// MyTripsFilter is the whole of GET /api/my/trips's query string, parsed into
// types. Who the caller is comes from the token and is never a parameter.
type MyTripsFilter struct {
	// Role is required. See DashboardRole.
	Role DashboardRole

	// Statuses is an optional filter over the trip's own status. Empty means
	// every status, which on the organizer tab includes draft and cancelled —
	// this is the only endpoint in the service that lists either.
	Statuses []Status

	// Limit is nil when the client did not ask for a page size; Normalize fills
	// the default. A pointer rather than a zero sentinel because `?limit=0` is a
	// request this endpoint must refuse, not a request it did not receive.
	Limit *int

	Cursor *Cursor
}

// PageSize is the page size to query with.
func (f MyTripsFilter) PageSize() int {
	if f.Limit == nil {
		return MyTripsLimitDefault
	}
	return *f.Limit
}

// Normalize returns a copy with the default page size applied and the status
// list deduplicated. It runs before Validate, never after.
func (f MyTripsFilter) Normalize() MyTripsFilter {
	out := f
	if out.Limit == nil {
		fallback := MyTripsLimitDefault
		out.Limit = &fallback
	}

	if f.Statuses != nil {
		// Deduplicated, order preserved, for the same reason SearchFilter
		// deduplicates categories: `status=draft,draft` is one question asked
		// twice and must not widen the `= ANY(...)` array.
		seen := make(map[Status]bool, len(f.Statuses))
		out.Statuses = make([]Status, 0, len(f.Statuses))
		for _, s := range f.Statuses {
			s = Status(strings.TrimSpace(string(s)))
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out.Statuses = append(out.Statuses, s)
		}
	}
	return out
}

// Validate checks every rule and reports all failures at once, with each field
// named as the query parameter the client sent.
func (f MyTripsFilter) Validate() error {
	v := &ValidationError{}

	switch f.Role {
	case DashboardOrganizer, DashboardParticipant:
	case "":
		v.add("role", fmt.Sprintf("is required and must be one of %s", joinRoles()))
	default:
		v.add("role", fmt.Sprintf("%q is not one of %s", f.Role, joinRoles()))
	}

	for _, s := range f.Statuses {
		if !KnownStatus(s) {
			v.add("status", fmt.Sprintf("%q is not one of %s", s, joinStatuses()))
		}
	}

	if f.Limit != nil && (*f.Limit < 1 || *f.Limit > MyTripsLimitMax) {
		v.add("limit", fmt.Sprintf("must be between 1 and %d", MyTripsLimitMax))
	}

	return v.orNil()
}

func joinRoles() string {
	names := make([]string, 0, len(DashboardRoles))
	for _, r := range DashboardRoles {
		names = append(names, string(r))
	}
	return strings.Join(names, ", ")
}

func joinStatuses() string {
	names := make([]string, 0, 5)
	for _, s := range AllStatuses() {
		names = append(names, string(s))
	}
	return strings.Join(names, ", ")
}

// Viewer is one caller's relationship to one trip, as the detail endpoint
// reports it.
//
// It exists so the SPA can choose between "Request to join", "Requested",
// "Chat" and "Leave" from one response instead of three. Every field is derived
// — nothing here is stored — and it is deliberately about the caller alone: a
// trip's roster is public to the people who can see the trip, but whether *you*
// have an open application is between you and the organizer.
type Viewer struct {
	IsOrganizer   bool
	IsParticipant bool

	// JoinRequestStatus is the caller's own standing application, and only
	// `pending` or `rejected` ever reach it. An approved request has become a
	// participants row and IsParticipant says so; a cancelled one has been
	// withdrawn and the caller may ask again, which is the same position as
	// never having asked. Nil in both of those cases.
	JoinRequestStatus *JoinRequestStatus
}

// NewViewer builds the block from a detail the caller has already loaded and
// the status of their most recent join request, nil when they have never asked.
//
// The roster is read from the detail rather than re-queried: GET
// /api/trips/{id} already carries every participant, and asking the database a
// second time for a row that is in memory is a round trip spent on nothing.
func NewViewer(detail *TripDetail, viewer uuid.UUID, latest *JoinRequestStatus) Viewer {
	v := Viewer{IsOrganizer: detail.Trip.OrganizerID == viewer}

	// The organizer occupies a seat and has a participants row, so this is true
	// for them too. That is the honest answer — it is the same roster the
	// response carries — and a client deciding what button to draw looks at
	// IsOrganizer first.
	for _, p := range detail.Participants {
		if p.UserID == viewer {
			v.IsParticipant = true
			break
		}
	}

	if latest != nil && (*latest == JoinPending || *latest == JoinRejected) {
		v.JoinRequestStatus = latest
	}
	return v
}
