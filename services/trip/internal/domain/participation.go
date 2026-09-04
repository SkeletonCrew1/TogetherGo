package domain

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// JoinRequestStatus is the lifecycle of one application to join a trip.
//
// Unlike a trip's Status this needs no transition table: `pending` is the only
// state anything leaves, and every edge out of it is terminal. The rule is
// therefore a single question — "is it still pending?" — asked under the row
// lock by every decision path in the store.
type JoinRequestStatus string

const (
	JoinPending   JoinRequestStatus = "pending"
	JoinApproved  JoinRequestStatus = "approved"
	JoinRejected  JoinRequestStatus = "rejected"
	JoinCancelled JoinRequestStatus = "cancelled"
)

const (
	// JoinMessageMaxLen matches the `message` field of join_request.created in
	// contracts/events.md. The event carries the message, so the column and the
	// contract have to agree on how long one can be.
	JoinMessageMaxLen = 500

	// DecisionReasonMaxLen bounds the organizer's free-text rejection reason.
	// It never reaches the bus — see the migration — but it is still stored and
	// still served, so it is still bounded.
	DecisionReasonMaxLen = 500

	// InviteBatchMax is how many users one POST /invites may name. Invitations
	// are addressed to people the organizer knows, not broadcast; a request
	// naming a hundred ids is a mistake or an abuse and either way is not
	// something to run.
	InviteBatchMax = 20

	// InviteTTL is how long an invitation stays worth acting on. It is not
	// enforced by a column — nothing expires invitations yet — but it is what
	// trip.invite_sent puts in `expires_at`, so the notification service can
	// say "expires in a week" without inventing the number itself.
	InviteTTL = 7 * 24 * time.Hour
)

// JoinRequest is one user's application to join one trip.
type JoinRequest struct {
	ID      uuid.UUID
	TripID  uuid.UUID
	UserID  uuid.UUID
	Status  JoinRequestStatus
	Message *string

	// DecisionReason is the organizer's own words on a rejection. Served to the
	// requester and to the organizer, published to nobody.
	DecisionReason *string

	CreatedAt time.Time
	DecidedAt *time.Time
	DecidedBy *uuid.UUID
}

// IsPending reports whether this request is still open. It is the only
// predicate any decision path asks of a request.
func (r JoinRequest) IsPending() bool { return r.Status == JoinPending }

// Invite is a direct invitation from an organizer to one user.
type Invite struct {
	ID            uuid.UUID
	TripID        uuid.UUID
	InvitedUserID uuid.UUID
	CreatedAt     time.Time
}

// ExpiresAt is the invitation's deadline, derived rather than stored: the TTL
// is a property of this build's policy, and storing it would mean a data
// migration to change a number that has no history worth keeping.
func (i Invite) ExpiresAt() time.Time { return i.CreatedAt.Add(InviteTTL) }

// ValidateJoinMessage checks the optional note a requester attaches.
//
// Runes, not bytes: "500 characters" is what the API documents and what a
// client counts in a textarea, and a Ukrainian sentence is roughly twice as
// many bytes as it is characters.
func ValidateJoinMessage(message *string) error {
	v := &ValidationError{}
	if message != nil && utf8.RuneCountInString(*message) > JoinMessageMaxLen {
		v.add("message", fmt.Sprintf("must be at most %d characters", JoinMessageMaxLen))
	}
	return v.orNil()
}

// ValidateDecisionReason checks the optional note an organizer attaches to a
// rejection.
func ValidateDecisionReason(reason *string) error {
	v := &ValidationError{}
	if reason != nil && utf8.RuneCountInString(*reason) > DecisionReasonMaxLen {
		v.add("reason", fmt.Sprintf("must be at most %d characters", DecisionReasonMaxLen))
	}
	return v.orNil()
}

// NormalizeInviteIDs deduplicates and validates the id list of one invite
// request, preserving the order the organizer sent.
//
// A repeated id is dropped rather than rejected: the same person named twice in
// one request is a client-side mistake with an obvious intent, and the unique
// index would otherwise turn it into a self-inflicted 409. Two *different*
// people, one of whom is already invited, is a different matter — that is a
// conflict the store reports, because the organizer's view of who has been
// invited is out of date.
func NormalizeInviteIDs(ids []uuid.UUID) ([]uuid.UUID, error) {
	v := &ValidationError{}

	seen := make(map[uuid.UUID]bool, len(ids))
	out := make([]uuid.UUID, 0, len(ids))
	for i, id := range ids {
		if id == uuid.Nil {
			v.add(fmt.Sprintf("user_ids[%d]", i), "must not be the nil uuid")
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}

	// Counted after deduplication: `["a","a","a"]` names one person, and
	// rejecting it for being over a limit it is not over would be nonsense.
	if len(out) == 0 && len(v.Fields) == 0 {
		v.add("user_ids", "must name at least one user")
	}
	if len(out) > InviteBatchMax {
		v.add("user_ids", fmt.Sprintf("must name at most %d users", InviteBatchMax))
	}

	if err := v.orNil(); err != nil {
		return nil, err
	}
	return out, nil
}

// UserConflictError is a refusal that names the people it is about.
//
// An invite request can name twenty users and fail because of one of them.
// Answering "already_invited" without saying who leaves the organizer to bisect
// their own request; carrying the ids means the SPA can grey out three rows and
// send the rest.
//
// Unwrap makes errors.Is match the sentinel, so the HTTP mapper keeps deciding
// the status code from the sentinel and reads the ids off the concrete type —
// the same shape as TransitionError.
type UserConflictError struct {
	Reason  error
	UserIDs []uuid.UUID
}

func (e *UserConflictError) Error() string {
	return fmt.Sprintf("%v: %d user(s)", e.Reason, len(e.UserIDs))
}

func (e *UserConflictError) Unwrap() error { return e.Reason }

// Limits on one page of a trip's join requests.
//
// The list is paginated for the same reason discovery is: a popular trip can
// collect far more applications than it has seats, and "how many requests can
// one trip have" has no answer the schema enforces. Keyset, never OFFSET
// (CLAUDE.md).
const (
	JoinRequestLimitDefault = 20
	JoinRequestLimitMax     = 100
)

// JoinRequestCursor is the position of the last row of a page, in the order the
// organizer's queue sorts by: pending first, then newest first.
//
// Three parts, because the ordering has three. `Decided` is the pending-first
// rank — false sorts before true — and it has to be in the cursor: without it,
// the first row of the second page would be compared against a timestamp from
// the wrong group, and either the whole decided section or the tail of the
// pending one would vanish. `ID` is the tiebreaker that makes the order total,
// exactly as it is for the discovery cursor.
type JoinRequestCursor struct {
	Decided   bool
	CreatedAt time.Time
	ID        uuid.UUID
}

// Encode renders the cursor as the opaque string clients echo back.
func (c JoinRequestCursor) Encode() string {
	rank := "0"
	if c.Decided {
		rank = "1"
	}
	raw := rank + cursorSeparator +
		c.CreatedAt.UTC().Format(time.RFC3339Nano) + cursorSeparator +
		c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeJoinRequestCursor parses what Encode produced and rejects everything
// else with a validation error — never a panic. Every step is a failure the
// client controls, so none of them may index into a slice a previous step has
// not proved is there.
func DecodeJoinRequestCursor(raw string) (*JoinRequestCursor, error) {
	invalid := func(reason string) error {
		return &ValidationError{Fields: []FieldError{{
			Field:   "cursor",
			Message: "is not a valid cursor (" + reason + "); omit it to start from the first page",
		}}}
	}

	if raw == "" {
		return nil, nil
	}
	if len(raw) > cursorMaxLen {
		return nil, invalid(fmt.Sprintf("longer than %d characters", cursorMaxLen))
	}

	decoded, err := decodeCursorBase64(raw)
	if err != nil {
		return nil, invalid("not base64")
	}

	rank, rest, found := strings.Cut(string(decoded), cursorSeparator)
	if !found || (rank != "0" && rank != "1") {
		return nil, invalid("wrong shape")
	}
	createdAt, id, found := strings.Cut(rest, cursorSeparator)
	if !found {
		return nil, invalid("wrong shape")
	}

	parsedCreated, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, invalid("bad timestamp")
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return nil, invalid("bad id")
	}

	return &JoinRequestCursor{
		Decided:   rank == "1",
		CreatedAt: parsedCreated.UTC(),
		ID:        parsedID,
	}, nil
}

// JoinRequestQuery is one page request against a trip's queue.
type JoinRequestQuery struct {
	Limit  int
	Cursor *JoinRequestCursor
}

// Normalize clamps the limit into range, so a caller that built the query by
// hand gets the same bounds the HTTP layer's parser applies.
func (q JoinRequestQuery) Normalize() JoinRequestQuery {
	if q.Limit <= 0 {
		q.Limit = JoinRequestLimitDefault
	}
	if q.Limit > JoinRequestLimitMax {
		q.Limit = JoinRequestLimitMax
	}
	return q
}

// JoinRequestPage is one page of a trip's queue plus the cursor for the next,
// nil when there is none.
type JoinRequestPage struct {
	Items      []JoinRequest
	NextCursor *JoinRequestCursor
}
