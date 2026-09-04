package httpapi

import (
	"net/http"

	"github.com/togethergo/trip/internal/auth"
)

// myTrips handles GET /api/my/trips — the user dashboard.
//
// The same three steps as searchTrips, and deliberately the same shape on the
// wire: parse the query into a typed filter, hand it to the one store method
// that knows the query, resolve the page's organizers in a single batch call.
// A dashboard card and a search card are the same card, so they go through the
// same serializer and differ only by the two fields the dashboard adds.
//
// Who the caller is comes from the token's `sub` and is never a parameter.
// There is no "whose dashboard" to get wrong.
func (h *tripHandlers) myTrips(w http.ResponseWriter, r *http.Request) {
	filter, err := parseMyTripsFilter(r.URL.Query())
	if err != nil {
		writeError(w, r, err)
		return
	}

	page, err := h.store.MyTrips(r.Context(), auth.UserIDFrom(r.Context()), filter)
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
