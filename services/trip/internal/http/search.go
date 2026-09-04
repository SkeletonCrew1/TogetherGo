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

// searchTrips handles GET /api/trips — trip discovery.
//
// The handler is three steps and no rules: parse the query into a typed filter,
// hand it to the one store method that knows the query shape, and resolve the
// page's organizers in a single batch call. What may be searched is decided in
// internal/domain; how it is searched is decided in internal/store.
func (h *tripHandlers) searchTrips(w http.ResponseWriter, r *http.Request) {
	filter, err := parseSearchFilter(r.URL.Query())
	if err != nil {
		writeError(w, r, err)
		return
	}

	page, err := h.store.SearchTrips(r.Context(), filter)
	if err != nil {
		writeError(w, r, err)
		return
	}

	body := searchResponse{
		Items:      newTripListItems(page.Items, h.organizers(r, page.Items)),
		NextCursor: encodeCursor(page.Next),
	}
	writeJSON(w, r, http.StatusOK, body)
}

// similarTrips handles GET /api/trips/{tripID}/similar.
//
// Visibility is applied to the *source* trip exactly as it is on the detail
// endpoint: a draft belongs to its organizer, so asking what is similar to
// somebody else's draft answers 404 and not "here are five trips, and by the
// way that id is real".
func (h *tripHandlers) similarTrips(w http.ResponseWriter, r *http.Request) {
	tripID, err := tripIDFrom(chi.URLParam(r, "tripID"))
	if err != nil {
		writeError(w, r, err)
		return
	}

	trip, err := h.store.Trip(r.Context(), tripID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !trip.IsVisibleTo(auth.UserIDFrom(r.Context())) {
		writeError(w, r, domain.ErrTripNotFound)
		return
	}

	items, err := h.store.SimilarTrips(r.Context(), trip)
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeJSON(w, r, http.StatusOK, similarResponse{
		Items: newTripListItems(items, h.organizers(r, items)),
	})
}

// organizers resolves every organizer on a page in one call to identity.
//
// This is the only place in the request that may fail without failing the
// request. Discovery is the busiest read path in the system and its answer is
// the trips; the organizer block is decoration on top of it. If identity is
// down the page still goes out — with ids and null names — and the outage is a
// warning in this service's log rather than a 500 in the client's face.
func (h *tripHandlers) organizers(r *http.Request, items []domain.TripListItem) map[uuid.UUID]users.User {
	if h.users == nil || len(items) == 0 {
		return nil
	}

	ids := organizerIDs(items)
	resolved, err := h.users.Resolve(r.Context(), ids)
	if err != nil {
		loggerFrom(r.Context()).Warn("organizer lookup degraded",
			slog.String("error", err.Error()),
			slog.Int("requested", len(ids)),
			slog.Int("resolved", len(resolved)),
		)
	}
	return resolved
}

// encodeCursor renders the next-page cursor, or null on the last page.
func encodeCursor(cursor *domain.Cursor) *string {
	if cursor == nil {
		return nil
	}
	encoded := cursor.Encode()
	return &encoded
}
