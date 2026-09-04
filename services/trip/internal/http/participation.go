package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/togethergo/trip/internal/auth"
	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/users"
)

// The participation endpoints. Same shape as the trip handlers: decode, call
// one store method, project the result. Not one of them decides who may do
// what — the store enforces that under the row lock, because a check made up
// here would be a check made against a snapshot — and not one of them chooses a
// status code, which writeError does in one place for the whole service.

// createJoinRequest handles POST /api/trips/{tripID}/requests.
func (h *tripHandlers) createJoinRequest(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	// The body is optional: asking to join without a note is normal, and
	// requiring `{}` would be a rule with no reason behind it.
	var body createJoinRequestBody
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, &body); err != nil {
			writeError(w, r, err)
			return
		}
	}

	request, err := h.store.CreateJoinRequest(r.Context(), tripID, auth.UserIDFrom(r.Context()), body.Message)
	if err != nil {
		writeError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/trips/"+tripID.String()+"/requests/me")
	writeJSON(w, r, http.StatusCreated, newJoinRequest(*request))
}

// myJoinRequest handles GET /api/trips/{tripID}/requests/me.
//
// The caller's most recent application, whatever became of it — which is the
// only way to read the organizer's free-text rejection reason, deliberately
// kept off the bus so that this is the only way.
func (h *tripHandlers) myJoinRequest(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	request, err := h.store.MyJoinRequest(r.Context(), tripID, auth.UserIDFrom(r.Context()))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newJoinRequest(*request))
}

// cancelJoinRequest handles DELETE /api/trips/{tripID}/requests/me.
//
// Addressed as `me` rather than by request id because that is what it is: a
// user may have exactly one open request on a trip, so there is nothing to
// identify. It also means the client does not have to have kept the id.
func (h *tripHandlers) cancelJoinRequest(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	if err := h.store.CancelJoinRequest(r.Context(), tripID, auth.UserIDFrom(r.Context())); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listJoinRequests handles GET /api/trips/{tripID}/requests — the organizer's
// queue, pending first, each row carrying the applicant's name, photo and
// rating.
//
// Those three fields belong to identity, and this service holds no copy of them
// (CLAUDE.md rule 1). The page collects its distinct user ids and makes one
// call to identity's batch resolver, exactly as discovery does for organizers —
// and, exactly as discovery does, it renders the page anyway if that call
// fails. An organizer must still be able to approve somebody when the identity
// service is having a bad afternoon; they will just be approving an id.
func (h *tripHandlers) listJoinRequests(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	query, err := parseJoinRequestQuery(r.URL.Query())
	if err != nil {
		writeError(w, r, err)
		return
	}

	page, err := h.store.JoinRequests(r.Context(), tripID, auth.UserIDFrom(r.Context()), query)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, newJoinRequestPage(page, h.requesters(r, page.Items)))
}

// approveJoinRequest handles POST /api/trips/{tripID}/requests/{requestID}/approve.
//
// The whole of the capacity invariant is in Store.ApproveJoinRequest, under one
// row lock. Nothing about it is visible here, which is the point: an
// availability check in this handler would be a check against a trip that a
// concurrent approval is in the middle of changing.
func (h *tripHandlers) approveJoinRequest(w http.ResponseWriter, r *http.Request) {
	tripID, requestID, err := tripAndRequestID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	request, err := h.store.ApproveJoinRequest(r.Context(), tripID, requestID, auth.UserIDFrom(r.Context()))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newJoinRequest(*request))
}

// rejectJoinRequest handles POST /api/trips/{tripID}/requests/{requestID}/reject.
func (h *tripHandlers) rejectJoinRequest(w http.ResponseWriter, r *http.Request) {
	tripID, requestID, err := tripAndRequestID(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var body rejectJoinRequestBody
	if r.ContentLength != 0 {
		if err := decodeBody(w, r, &body); err != nil {
			writeError(w, r, err)
			return
		}
	}

	request, err := h.store.RejectJoinRequest(r.Context(), tripID, requestID, auth.UserIDFrom(r.Context()), body.Reason)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newJoinRequest(*request))
}

// removeParticipant handles DELETE /api/trips/{tripID}/participants/{userID}.
//
// Organizer only, and the organizer cannot name themselves: a trip with nobody
// who can approve, cancel or complete it is not a trip anybody can rescue.
func (h *tripHandlers) removeParticipant(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	userID, err := pathUUID(chi.URLParam(r, "userID"), "user_id")
	if err != nil {
		writeError(w, r, err)
		return
	}

	detail, err := h.store.RemoveParticipant(r.Context(), tripID, userID, auth.UserIDFrom(r.Context()))
	if err != nil {
		writeError(w, r, err)
		return
	}
	// The updated trip rather than 204: the roster and approved_count have both
	// changed and the organizer is looking at a screen showing them.
	writeJSON(w, r, http.StatusOK, newTripResponse(detail))
}

// leaveTrip handles POST /api/trips/{tripID}/participants/me.
//
// POST and not DELETE, even though it deletes a row, because the path names the
// caller rather than a resource they own — and because DELETE on
// `/participants/{uid}` is already the organizer's removal, which is a
// different permission with a different event reason on the far side.
func (h *tripHandlers) leaveTrip(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	detail, err := h.store.LeaveTrip(r.Context(), tripID, auth.UserIDFrom(r.Context()))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newTripResponse(detail))
}

// inviteUsers handles POST /api/trips/{tripID}/invites.
//
// All or nothing: twenty ids where one is already invited writes no rows and
// answers 409 naming that one, rather than nineteen invitations and a silence.
func (h *tripHandlers) inviteUsers(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	var body inviteBody
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}
	ids, err := body.toIDs()
	if err != nil {
		writeError(w, r, err)
		return
	}

	invites, err := h.store.InviteUsers(r.Context(), tripID, auth.UserIDFrom(r.Context()), ids)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusCreated, newInvites(invites))
}

// listInvites handles GET /api/trips/{tripID}/invites. Organizer only: who was
// asked, and who has not answered, is the organizer's business.
func (h *tripHandlers) listInvites(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	invites, err := h.store.Invites(r.Context(), tripID, auth.UserIDFrom(r.Context()))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newInvites(invites))
}

// requesters resolves a queue page's applicants in one call to identity.
//
// The same "may fail without failing the request" rule as tripHandlers.
// organizers, and for the same reason: the answer to this endpoint is the
// requests, and the names on them are decoration that a client can render a
// placeholder for.
func (h *tripHandlers) requesters(r *http.Request, items []domain.JoinRequest) map[uuid.UUID]users.User {
	if h.users == nil || len(items) == 0 {
		return nil
	}

	ids := requesterIDs(items)
	resolved, err := h.users.Resolve(r.Context(), ids)
	if err != nil {
		loggerFrom(r.Context()).Warn("requester lookup degraded",
			slog.String("error", err.Error()),
			slog.Int("requested", len(ids)),
			slog.Int("resolved", len(resolved)),
		)
	}
	return resolved
}

// tripAndRequestID parses the two path parameters the decision endpoints take.
func tripAndRequestID(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	requestID, err := pathUUID(chi.URLParam(r, "requestID"), "request_id")
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return tripID, requestID, nil
}

// pathUUID parses a uuid path parameter, naming it in the error so a client
// with two ids in one path learns which of them it got wrong.
func pathUUID(raw, field string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, &domain.ValidationError{Fields: []domain.FieldError{{
			Field:   field,
			Message: "must be a uuid",
		}}}
	}
	return id, nil
}
