package httpapi

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/togethergo/chat/internal/auth"
	"github.com/togethergo/chat/internal/domain"
	"github.com/togethergo/chat/internal/store"
)

type roomHandlers struct {
	store *store.Store
}

// listRooms serves the caller's rooms, most recently active first, with the
// last thing said in each and how much of it they have not read.
//
// Everything a room list needs in one request. The alternative — a list of
// rooms plus a message fetch per room plus an unread count per room — is the
// N+1 that makes a chat sidebar slow, and the two lateral joins in
// store.ListRooms are what avoid it.
func (h *roomHandlers) listRooms(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFrom(r.Context())

	cursor, err := decodeRoomsCursor(strings.TrimSpace(r.URL.Query().Get("cursor")))
	if err != nil {
		writeError(w, r, err)
		return
	}

	// One more than asked for, so "is there another page" is answered by the
	// same query rather than by a count.
	limit := domain.DefaultPageLimit
	summaries, err := h.store.ListRooms(r.Context(), userID, cursor, limit+1)
	if err != nil {
		writeError(w, r, err)
		return
	}

	var next *string
	if len(summaries) > limit {
		summaries = summaries[:limit]
		last := summaries[len(summaries)-1]
		encoded := encodeRoomsCursor(store.RoomsCursor{
			LastActivity: last.LastActivity,
			TripID:       last.Room.TripID,
		})
		next = &encoded
	}

	items := make([]roomResponse, 0, len(summaries))
	for _, summary := range summaries {
		items = append(items, newRoomResponse(summary))
	}
	writeJSON(w, r, http.StatusOK, page[roomResponse]{Items: items, NextCursor: next})
}

// listMessages serves a page of history, newest first.
//
// Newest first because that is the order a room is read in: the client wants
// the bottom of the conversation and pages backwards from there. A history
// endpoint that returned oldest-first would make the common case — open a room,
// see the last thing said — require paging to the end first.
//
// Membership is checked before anything is read. A cancelled room is still
// readable, which is the point of keeping it: the trip was called off, and the
// conversation about it is often the reason people want to look.
func (h *roomHandlers) listMessages(w http.ResponseWriter, r *http.Request) {
	tripID, err := pathUUID(chi.URLParam(r, "tripID"), "trip_id")
	if err != nil {
		writeError(w, r, err)
		return
	}

	userID := auth.UserIDFrom(r.Context())
	access, err := h.store.Authorize(r.Context(), tripID, userID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !access.IsMember {
		writeError(w, r, domain.ErrNotMember)
		return
	}

	query, err := parseMessagesQuery(r)
	if err != nil {
		writeError(w, r, err)
		return
	}

	messages, hasMore, err := h.store.ListMessages(r.Context(), tripID, query.beforeID, query.limit)
	if err != nil {
		writeError(w, r, err)
		return
	}

	items := make([]messageResponse, 0, len(messages))
	for _, message := range messages {
		items = append(items, newMessageResponse(message))
	}

	var next *string
	if hasMore && len(messages) > 0 {
		// The oldest id on this page. The next page is everything before it,
		// which is what makes paging backwards stable while new messages are
		// arriving at the other end.
		encoded := encodeCursor(messages[len(messages)-1].ID)
		next = &encoded
	}

	writeJSON(w, r, http.StatusOK, page[messageResponse]{Items: items, NextCursor: next})
}
