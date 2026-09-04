package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/togethergo/chat/internal/auth"
	"github.com/togethergo/chat/internal/domain"
)

// The API error shape, fixed by CLAUDE.md for every service:
//
//	{"error": {"code": "room_closed", "message": "...", "details": {...}}}
//
// The HTTP status carries the class of failure; `code` carries the specific
// reason. Clients switch on `code`, never on the message text. The websocket's
// `error` frame uses the same vocabulary — see internal/hub — so a client has
// one table of codes rather than two.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

type errorEnvelope struct {
	Error apiError `json:"error"`
}

// validationDetails is the `details` payload of a validation_error: the list of
// rejected fields, each with a path into the request body or a query parameter
// name.
type validationDetails struct {
	Fields []domain.FieldError `json:"fields"`
}

// writeError is the single place a domain error becomes a response. Every
// handler returns errors up to here; none of them chooses a status code.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	logger := loggerFrom(r.Context())

	var validation *domain.ValidationError
	if errors.As(err, &validation) {
		writeJSON(w, r, http.StatusBadRequest, errorEnvelope{apiError{
			Code:    "validation_error",
			Message: "The request is invalid. See details.fields.",
			Details: validationDetails{Fields: validation.Fields},
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

	case errors.Is(err, domain.ErrRoomNotFound):
		// 404 rather than 503, even though "the projection has not caught up"
		// is closer to the latter. A client asking for a room that does not
		// exist and a client asking for one that does not exist *yet* cannot be
		// told apart from here without calling the trip service, which is
		// exactly what this service does not do. Both are "no such room", and
		// the retry is the same.
		writeJSON(w, r, http.StatusNotFound, errorEnvelope{apiError{
			Code:    "room_not_found",
			Message: "No such chat room.",
		}})

	case errors.Is(err, domain.ErrNotMember):
		// 403, not 404. The caller supplied a trip id they were able to reach,
		// and hiding the room's existence would be theatre: the trip itself is
		// public, they can see it and its participant list through the trip
		// service. What they cannot do is read the conversation.
		writeJSON(w, r, http.StatusForbidden, errorEnvelope{apiError{
			Code:    "not_member",
			Message: "Only approved participants can use this trip's chat.",
		}})

	case errors.Is(err, domain.ErrRoomClosed):
		writeJSON(w, r, http.StatusConflict, errorEnvelope{apiError{
			Code:    "room_closed",
			Message: "This trip has been cancelled; the room is read-only.",
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

// errBadBody is a request body that could not be decoded at all — malformed
// JSON, or a field of the wrong type. Distinct from a ValidationError, which is
// a body this service understood and refused.
type errBadBody struct{ reason string }

func (e errBadBody) Error() string { return e.reason }

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
