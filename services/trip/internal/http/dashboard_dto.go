package httpapi

import (
	"errors"
	"net/url"

	"github.com/togethergo/trip/internal/domain"
)

// myTripsParams is the closed set of query parameters GET /api/my/trips
// accepts. Same rule as discovery: an unrecognised parameter is a 400, not a
// silently ignored filter.
var myTripsParams = map[string]bool{
	"role":   true,
	"status": true,
	"limit":  true,
	"cursor": true,
}

// parseMyTripsFilter turns a query string into a validated domain filter.
//
// The same split the other parsers use: this function decides what the encoding
// got wrong — a limit that is not a number, a cursor that is not base64, a
// parameter nobody recognises — and domain decides what the values got wrong.
// Both kinds of failure are collected and reported in one response.
func parseMyTripsFilter(values url.Values) (domain.MyTripsFilter, error) {
	p := &queryParser{values: values, errs: &domain.ValidationError{}}
	p.rejectUnknown(myTripsParams)

	filter := domain.MyTripsFilter{
		// An absent `role` arrives as the empty string and is rejected by
		// Validate as "is required". Defaulting it would be worse than a 400:
		// the two tabs answer different questions, and guessing which one the
		// client meant is how somebody's drafts end up on the wrong screen.
		Role:     domain.DashboardRole(p.text("role")),
		Statuses: toStatuses(p.list("status")),
		Limit:    p.integer("limit"),
		Cursor:   p.cursor("cursor"),
	}

	filter = filter.Normalize()

	var validation *domain.ValidationError
	if err := filter.Validate(); errors.As(err, &validation) {
		p.errs.Fields = append(p.errs.Fields, validation.Fields...)
	}

	if len(p.errs.Fields) > 0 {
		return filter, p.errs
	}
	return filter, nil
}

// toStatuses retypes the comma-separated `status` parameter. Nothing is
// validated here — an unknown member is a domain rule and is reported by
// MyTripsFilter.Validate, which names the whole vocabulary in the message.
func toStatuses(values []string) []domain.Status {
	if values == nil {
		return nil
	}
	out := make([]domain.Status, len(values))
	for i, v := range values {
		out[i] = domain.Status(v)
	}
	return out
}

// --- responses --------------------------------------------------------------

// viewerResponse is the caller's relationship to one trip, on the detail
// endpoint. It is what the SPA chooses between "Request to join", "Requested",
// "Chat" and "Leave" with, and the reason it can do so from one response.
//
// `join_request_status` is not omitempty: "you have no open application" is an
// answer, and a client that has to distinguish an absent key from a null one is
// a client that will get it wrong.
type viewerResponse struct {
	IsOrganizer       bool    `json:"is_organizer"`
	IsParticipant     bool    `json:"is_participant"`
	JoinRequestStatus *string `json:"join_request_status"`
}

func newViewer(v domain.Viewer) *viewerResponse {
	body := &viewerResponse{
		IsOrganizer:   v.IsOrganizer,
		IsParticipant: v.IsParticipant,
	}
	if v.JoinRequestStatus != nil {
		status := string(*v.JoinRequestStatus)
		body.JoinRequestStatus = &status
	}
	return body
}
