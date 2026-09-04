package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/togethergo/trip/internal/auth"
	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/store"
	"github.com/togethergo/trip/internal/users"
)

// tripHandlers holds the dependencies the trip endpoints need.
//
// The handlers are thin on purpose: decode, call one store method, project the
// result. They contain no rule about who may do what and no status-code
// decision — the store enforces the first under a row lock, writeError makes
// the second in one place.
type tripHandlers struct {
	store *store.Store

	// users resolves organizer ids for the discovery endpoints. Nil is legal
	// and means "do not try": the block comes back with an id and nulls, which
	// is the same shape the degraded path produces when identity is down, so
	// there is one rendering path rather than two.
	users users.Resolver
}

// createTrip handles POST /api/trips.
//
// A new trip is always a draft: publishing is a separate, explicit act, so an
// organizer can build a route over several requests without anyone seeing a
// half-finished trip.
func (h *tripHandlers) createTrip(w http.ResponseWriter, r *http.Request) {
	var body createTripRequest
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	input, err := body.toInput()
	if err != nil {
		writeError(w, r, err)
		return
	}

	detail, err := h.store.CreateTrip(r.Context(), auth.UserIDFrom(r.Context()), input)
	if err != nil {
		writeError(w, r, err)
		return
	}

	w.Header().Set("Location", "/api/trips/"+detail.Trip.ID.String())
	writeJSON(w, r, http.StatusCreated, newTripResponse(detail))
}

// getTrip handles GET /api/trips/{tripID}.
//
// Visibility is applied here rather than in the store because "who is asking"
// is an HTTP fact. A draft belongs to its organizer alone, and to everyone else
// it does not exist — hence 404 and not 403. See domain.Trip.IsVisibleTo.
//
// This is the one trip response that carries a `viewer` block, and the one
// query behind it that is about the caller rather than about the trip. The
// detail page has four mutually exclusive primary actions — request to join,
// see a pending request, open the chat, leave — and without this it would have
// to guess at three of them or spend two more round trips finding out.
func (h *tripHandlers) getTrip(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	viewer := auth.UserIDFrom(r.Context())

	detail, err := h.store.TripDetail(r.Context(), tripID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !detail.Trip.IsVisibleTo(viewer) {
		writeError(w, r, domain.ErrTripNotFound)
		return
	}

	// After the visibility check, never before: a caller who may not know this
	// trip exists must not be told anything about it, including what their own
	// application to it did.
	latest, err := h.store.ViewerJoinRequest(r.Context(), tripID, viewer)
	if err != nil {
		writeError(w, r, err)
		return
	}

	body := newTripResponse(detail)
	body.Viewer = newViewer(domain.NewViewer(detail, viewer, latest))
	writeJSON(w, r, http.StatusOK, body)
}

// updateTrip handles PATCH /api/trips/{tripID}.
//
// Organizer only, and only while the trip is a draft or recruiting. Omitted
// keys are left alone; `points`, if present, replaces the whole route, because
// a route is an ordered sequence and there is no sensible way to merge one
// partially.
func (h *tripHandlers) updateTrip(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	var body updateTripRequest
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, r, err)
		return
	}

	patch, err := body.toPatch()
	if err != nil {
		writeError(w, r, err)
		return
	}

	detail, err := h.store.UpdateTrip(r.Context(), tripID, auth.UserIDFrom(r.Context()), patch)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, newTripResponse(detail))
}

// publishTrip handles POST /api/trips/{tripID}/publish: draft -> recruiting.
func (h *tripHandlers) publishTrip(w http.ResponseWriter, r *http.Request) {
	h.changeStatus(w, r, domain.StatusRecruiting)
}

// cancelTrip handles POST /api/trips/{tripID}/cancel.
//
// Legal from draft, recruiting and in_progress; refused from the two terminal
// states. The state machine is what says so, not this handler.
func (h *tripHandlers) cancelTrip(w http.ResponseWriter, r *http.Request) {
	h.changeStatus(w, r, domain.StatusCancelled)
}

// changeStatus is the one route both lifecycle endpoints take into the store's
// single status writer.
func (h *tripHandlers) changeStatus(w http.ResponseWriter, r *http.Request, to domain.Status) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	detail, err := h.store.ChangeStatus(r.Context(), tripID, auth.UserIDFrom(r.Context()), to)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, newTripResponse(detail))
}

// deleteTrip handles DELETE /api/trips/{tripID}: organizer only, draft only,
// hard delete. Anything already published is cancelled instead.
func (h *tripHandlers) deleteTrip(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	if err := h.store.DeleteTrip(r.Context(), tripID, auth.UserIDFrom(r.Context())); err != nil {
		writeError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
