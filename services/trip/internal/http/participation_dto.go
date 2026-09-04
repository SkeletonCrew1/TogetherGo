package httpapi

import (
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/users"
)

// --- requests ---------------------------------------------------------------

// createJoinRequestBody is the body of POST /api/trips/{id}/requests. The
// message is the only thing a requester supplies; who they are comes from the
// token, and which trip from the path.
type createJoinRequestBody struct {
	Message *string `json:"message"`
}

// rejectJoinRequestBody is the body of .../reject. The reason is stored and
// shown to the requester; it is not published (contracts/events.md).
type rejectJoinRequestBody struct {
	Reason *string `json:"reason"`
}

// inviteBody is the body of POST /api/trips/{id}/invites.
//
// The ids arrive as strings and are parsed here rather than being declared as
// []uuid.UUID, because uuid's own UnmarshalJSON fails the whole decode on the
// first bad value and the client learns only that its body "is not valid JSON".
// Parsed by hand, `user_ids[3]` is named and the other nineteen are still
// checked.
type inviteBody struct {
	UserIDs []string `json:"user_ids"`
}

func (b inviteBody) toIDs() ([]uuid.UUID, error) {
	v := &domain.ValidationError{}
	out := make([]uuid.UUID, 0, len(b.UserIDs))
	for i, raw := range b.UserIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			v.Fields = append(v.Fields, domain.FieldError{
				Field:   "user_ids[" + itoa(i) + "]",
				Message: "must be a uuid",
			})
			continue
		}
		out = append(out, id)
	}
	if len(v.Fields) > 0 {
		return nil, v
	}
	return out, nil
}

// joinRequestParams is the closed set of query parameters the organizer's queue
// accepts. Same rule as discovery: an unrecognised parameter is a 400, not a
// silently ignored filter.
var joinRequestParams = map[string]bool{"limit": true, "cursor": true}

func parseJoinRequestQuery(values url.Values) (domain.JoinRequestQuery, error) {
	p := &queryParser{values: values, errs: &domain.ValidationError{}}
	p.rejectUnknown(joinRequestParams)

	query := domain.JoinRequestQuery{}
	if limit := p.integer("limit"); limit != nil {
		if *limit < 1 || *limit > domain.JoinRequestLimitMax {
			p.fail("limit", "must be between 1 and "+itoa(domain.JoinRequestLimitMax))
		} else {
			query.Limit = *limit
		}
	}
	query.Cursor = p.joinRequestCursor("cursor")

	if len(p.errs.Fields) > 0 {
		return query, p.errs
	}
	return query.Normalize(), nil
}

// joinRequestCursor is the queue's cursor, which sorts differently from
// discovery's and therefore encodes differently. Sharing the method name across
// two cursor types is what would let one be decoded as the other.
func (p *queryParser) joinRequestCursor(name string) *domain.JoinRequestCursor {
	value, ok := p.raw(name)
	if !ok {
		return nil
	}
	cursor, err := domain.DecodeJoinRequestCursor(value)
	if err != nil {
		var validation *domain.ValidationError
		if errors.As(err, &validation) {
			p.errs.Fields = append(p.errs.Fields, validation.Fields...)
		} else {
			p.fail(name, "is not a valid cursor")
		}
		return nil
	}
	return cursor
}

// --- responses --------------------------------------------------------------

// requesterResponse is the applicant as the organizer's queue shows them,
// resolved from identity in one batch call per page.
//
// The same degraded shape as organizerResponse: every field but the id is
// nullable, and an unreachable identity means a queue rendered with ids and
// nulls rather than a 500. `rating_count` is here and not on the organizer
// block because this is the screen where it changes a decision — "4.9" from one
// rating and "4.9" from forty are not the same recommendation.
type requesterResponse struct {
	ID          uuid.UUID `json:"id"`
	FullName    *string   `json:"full_name"`
	PhotoURL    *string   `json:"photo_url"`
	RatingAvg   *float64  `json:"rating_avg"`
	RatingCount *int      `json:"rating_count"`
}

func newRequester(id uuid.UUID, resolved map[uuid.UUID]users.User) requesterResponse {
	user, known := resolved[id]
	if !known {
		return requesterResponse{ID: id}
	}
	count := user.RatingCount
	return requesterResponse{
		ID:          id,
		FullName:    &user.FullName,
		PhotoURL:    user.PhotoURL,
		RatingAvg:   user.RatingAvg,
		RatingCount: &count,
	}
}

// joinRequestResponse is one application.
//
// `reason` is the organizer's own words on a rejection. It is served to the
// requester and to the organizer and to nobody else, and it is deliberately
// absent from join_request.rejected on the bus — a notification template acts
// on the reason *code*, and the sentence is between the two people involved.
type joinRequestResponse struct {
	ID        uuid.UUID `json:"id"`
	TripID    uuid.UUID `json:"trip_id"`
	UserID    uuid.UUID `json:"user_id"`
	Status    string    `json:"status"`
	Message   *string   `json:"message"`
	Reason    *string   `json:"reason"`
	CreatedAt time.Time `json:"created_at"`

	DecidedAt *time.Time `json:"decided_at"`
	DecidedBy *uuid.UUID `json:"decided_by"`

	// Requester is nil on the requester's own view of their request: they know
	// who they are, and resolving one user to tell them would be a call to
	// identity per request for nothing.
	Requester *requesterResponse `json:"requester,omitempty"`
}

func newJoinRequest(r domain.JoinRequest) joinRequestResponse {
	return joinRequestResponse{
		ID:        r.ID,
		TripID:    r.TripID,
		UserID:    r.UserID,
		Status:    string(r.Status),
		Message:   r.Message,
		Reason:    r.DecisionReason,
		CreatedAt: r.CreatedAt.UTC(),
		DecidedAt: utcPtr(r.DecidedAt),
		DecidedBy: r.DecidedBy,
	}
}

// joinRequestPageResponse is the list shape CLAUDE.md fixes for every list
// endpoint: items plus an opaque cursor, null when there is no next page.
type joinRequestPageResponse struct {
	Items      []joinRequestResponse `json:"items"`
	NextCursor *string               `json:"next_cursor"`
}

func newJoinRequestPage(page *domain.JoinRequestPage, resolved map[uuid.UUID]users.User) joinRequestPageResponse {
	items := make([]joinRequestResponse, 0, len(page.Items))
	for _, r := range page.Items {
		item := newJoinRequest(r)
		requester := newRequester(r.UserID, resolved)
		item.Requester = &requester
		items = append(items, item)
	}

	body := joinRequestPageResponse{Items: items}
	if page.NextCursor != nil {
		encoded := page.NextCursor.Encode()
		body.NextCursor = &encoded
	}
	return body
}

// requesterIDs is the set of users one page needs resolved — the input to the
// single batch call, and the reason the queue costs one request to identity
// however many applications it shows.
func requesterIDs(items []domain.JoinRequest) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(items))
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		if seen[item.UserID] {
			continue
		}
		seen[item.UserID] = true
		ids = append(ids, item.UserID)
	}
	return ids
}

// inviteResponse is one invitation. `expires_at` is derived from created_at and
// the build's TTL rather than stored — the same number trip.invite_sent carries,
// so the mail and the API agree without either reading the other.
type inviteResponse struct {
	ID            uuid.UUID `json:"id"`
	TripID        uuid.UUID `json:"trip_id"`
	InvitedUserID uuid.UUID `json:"invited_user_id"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type invitesResponse struct {
	Items []inviteResponse `json:"items"`
}

func newInvites(invites []domain.Invite) invitesResponse {
	items := make([]inviteResponse, 0, len(invites))
	for _, i := range invites {
		items = append(items, inviteResponse{
			ID:            i.ID,
			TripID:        i.TripID,
			InvitedUserID: i.InvitedUserID,
			CreatedAt:     i.CreatedAt.UTC(),
			ExpiresAt:     i.ExpiresAt().UTC(),
		})
	}
	return invitesResponse{Items: items}
}

// itoa keeps strconv out of the message-building call sites above, where the
// conversion is incidental to the sentence being assembled.
func itoa(v int) string { return strconv.Itoa(v) }
