package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/togethergo/trip/internal/auth"
	"github.com/togethergo/trip/internal/domain"
)

// The API error shape, fixed by CLAUDE.md for every service:
//
//	{"error": {"code": "trip_full", "message": "...", "details": {...}}}
//
// The HTTP status carries the class of failure; `code` carries the specific
// reason. Clients switch on `code`, never on the message text.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

// validationDetails is the `details` payload of a validation_error: the list of
// rejected fields, each with a path into the request body.
type validationDetails struct {
	Fields []domain.FieldError `json:"fields"`
}

// transitionDetails is the `details` payload of an invalid_transition. Telling
// the client what would have been allowed turns a dead end into something it
// can act on — usually by refetching, since the reason is almost always that
// its copy of the trip is stale.
type transitionDetails struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Allowed []string `json:"allowed"`
}

// userConflictDetails is the `details` payload of an invite refusal: the ids
// that caused it. A request naming twenty users that fails because of one of
// them has to say which one, or the organizer bisects their own request.
type userConflictDetails struct {
	UserIDs []uuid.UUID `json:"user_ids"`
}

// participationErrors maps the participation sentinels onto the wire.
//
// A table rather than another dozen cases in the switch below, because these
// all have the same shape — one sentinel, one status, one code, one sentence —
// and because domain.UserConflictError needs to look up the same three values
// for a sentinel it is wrapping. Two lookups, one source.
//
// The status codes divide the way the failures do: 404 for a resource that is
// not there (or that the caller may not know is there), 409 for a request that
// is well-formed and permitted but disagrees with the current state of the
// trip. Every code is distinct, because each one has a different remedy —
// refetch the trip, cancel your open request, wait for a seat, stop asking.
var participationErrors = []struct {
	sentinel error
	status   int
	code     string
	message  string
}{
	{domain.ErrOwnTrip, http.StatusConflict, "cannot_join_own_trip",
		"You organize this trip and are already on it."},
	{domain.ErrTripNotRecruiting, http.StatusConflict, "trip_not_recruiting",
		"This trip is not accepting join requests."},
	{domain.ErrAlreadyParticipant, http.StatusConflict, "already_participant",
		"That user is already on this trip."},
	{domain.ErrRequestPending, http.StatusConflict, "request_already_pending",
		"You already have an open request on this trip."},
	{domain.ErrTripFull, http.StatusConflict, "trip_full",
		"This trip has no free seats left."},
	{domain.ErrRequestNotPending, http.StatusConflict, "request_not_pending",
		"That join request has already been decided."},
	{domain.ErrCannotRemoveOrganizer, http.StatusConflict, "cannot_remove_organizer",
		"The organizer cannot leave or be removed from their own trip."},
	{domain.ErrTripEnded, http.StatusConflict, "trip_ended",
		"This trip has already ended; its roster cannot be changed."},
	{domain.ErrAlreadyInvited, http.StatusConflict, "already_invited",
		"That user has already been invited to this trip."},
	{domain.ErrCannotInviteSelf, http.StatusConflict, "cannot_invite_self",
		"You organize this trip and cannot invite yourself to it."},
	{domain.ErrRequestNotFound, http.StatusNotFound, "join_request_not_found",
		"No such join request."},
	{domain.ErrNotParticipant, http.StatusNotFound, "not_participant",
		"That user is not on this trip."},
}

// writeError is the single place a domain error becomes a response. Every
// handler returns errors up to here; none of them chooses a status code.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	logger := loggerFrom(r.Context())

	var validation *domain.ValidationError
	if errors.As(err, &validation) {
		// "The request", not "the request body": the discovery endpoints
		// validate a query string through this same mapper, and each field
		// error names the parameter or the body path it came from anyway.
		writeJSON(w, r, http.StatusBadRequest, errorEnvelope{apiError{
			Code:    "validation_error",
			Message: "The request is invalid. See details.fields.",
			Details: validationDetails{Fields: validation.Fields},
		}})
		return
	}

	var transition *domain.TransitionError
	if errors.As(err, &transition) {
		allowed := make([]string, 0, 2)
		for _, s := range transition.Allowed() {
			allowed = append(allowed, string(s))
		}
		writeJSON(w, r, http.StatusConflict, errorEnvelope{apiError{
			Code:    "invalid_transition",
			Message: transition.Error() + ".",
			Details: transitionDetails{From: string(transition.From), To: string(transition.To), Allowed: allowed},
		}})
		return
	}

	var badBody errBadBody
	if errors.As(err, &badBody) {
		writeJSON(w, r, http.StatusBadRequest, errorEnvelope{apiError{
			Code:    "invalid_body",
			Message: badBody.Error(),
		}})
		return
	}

	// Before the sentinel table, because this one wraps a sentinel the table
	// also matches and carries the ids that go in `details`.
	var conflict *domain.UserConflictError
	if errors.As(err, &conflict) {
		for _, mapping := range participationErrors {
			if errors.Is(conflict.Reason, mapping.sentinel) {
				writeJSON(w, r, mapping.status, errorEnvelope{apiError{
					Code:    mapping.code,
					Message: mapping.message,
					Details: userConflictDetails{UserIDs: conflict.UserIDs},
				}})
				return
			}
		}
	}

	for _, mapping := range participationErrors {
		if errors.Is(err, mapping.sentinel) {
			writeJSON(w, r, mapping.status, errorEnvelope{apiError{
				Code:    mapping.code,
				Message: mapping.message,
			}})
			return
		}
	}

	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		// RFC 6750: a 401 on a bearer-protected resource says so. The reason
		// the token failed is logged at debug and never returned — see
		// auth.ErrInvalidToken.
		logger.Debug("rejected access token", slog.String("reason", err.Error()))
		w.Header().Set("WWW-Authenticate", `Bearer`)
		writeJSON(w, r, http.StatusUnauthorized, errorEnvelope{apiError{
			Code:    "invalid_access_token",
			Message: "The access token is missing or invalid.",
		}})

	case errors.Is(err, domain.ErrTripNotFound):
		writeJSON(w, r, http.StatusNotFound, errorEnvelope{apiError{
			Code:    "trip_not_found",
			Message: "No such trip.",
		}})

	case errors.Is(err, domain.ErrNotOrganizer):
		writeJSON(w, r, http.StatusForbidden, errorEnvelope{apiError{
			Code:    "not_organizer",
			Message: "Only the trip organizer may do that.",
		}})

	case errors.Is(err, domain.ErrNotEditable):
		writeJSON(w, r, http.StatusConflict, errorEnvelope{apiError{
			Code:    "trip_not_editable",
			Message: "A trip can only be edited while it is a draft or recruiting.",
		}})

	case errors.Is(err, domain.ErrNotDraft):
		writeJSON(w, r, http.StatusConflict, errorEnvelope{apiError{
			Code:    "trip_not_draft",
			Message: "Only a draft can be deleted. Cancel the trip instead.",
		}})

	case errors.Is(err, domain.ErrCapacityBelowApproved):
		writeJSON(w, r, http.StatusConflict, errorEnvelope{apiError{
			Code:    "capacity_below_participants",
			Message: "Capacity cannot be lower than the number of approved participants.",
		}})

	default:
		// The error text can quote a query parameter or a row value, so it is
		// logged and never returned.
		logger.Error("unhandled error", slog.String("error", err.Error()))
		writeJSON(w, r, http.StatusInternalServerError, errorEnvelope{apiError{
			Code:    "internal_error",
			Message: "An unexpected error occurred.",
		}})
	}
}

// writeJSON encodes a response body.
//
// It marshals before touching the ResponseWriter: an encoder writing straight
// to the socket would already have sent a 200 and half an object by the time it
// discovered the value could not be marshalled, leaving the client with
// truncated JSON and no way to tell.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		loggerFrom(r.Context()).Error("encode response body", slog.String("error", err.Error()))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"An unexpected error occurred."}}`))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(encoded)
	}
}
