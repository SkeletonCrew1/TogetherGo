package httpapi

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/togethergo/trip/internal/domain"
	"github.com/togethergo/trip/internal/users"
)

// searchParams is the closed set of query parameters GET /api/trips accepts.
//
// Anything else is a 400, for the same reason the JSON decoder sets
// DisallowUnknownFields: a client that sends `catgories=hiking` and gets every
// trip in the system back has been told its filter worked. The failure would
// surface much later, as "search returns things it shouldn't", and by then
// nobody is looking at the query string.
var searchParams = map[string]bool{
	"date_from": true, "date_to": true,
	"near_lat": true, "near_lng": true, "radius_km": true,
	"dest_lat": true, "dest_lng": true, "dest_radius_km": true,
	"min_days": true, "max_days": true,
	"min_free_slots": true,
	"categories":     true,
	"q":              true,
	"limit":          true,
	"cursor":         true,
}

// parseSearchFilter turns a query string into a validated domain filter.
//
// The split follows the one dto.go already uses for bodies: this function
// decides what the *encoding* got wrong — a date that is not RFC 3339, a limit
// that is not a number, a parameter nobody recognises — and domain decides what
// the *values* got wrong. Both kinds of failure are collected and reported in
// one response, because a client that gets one error per round trip needs as
// many round trips as it has mistakes.
func parseSearchFilter(values url.Values) (domain.SearchFilter, error) {
	p := &queryParser{values: values, errs: &domain.ValidationError{}}
	p.rejectUnknown(searchParams)

	filter := domain.SearchFilter{
		DateFrom: p.timestamp("date_from"),
		DateTo:   p.timestamp("date_to"),
		Near: domain.RadiusFilter{
			Lat:      p.float("near_lat"),
			Lng:      p.float("near_lng"),
			RadiusKm: p.float("radius_km"),
		},
		Destination: domain.RadiusFilter{
			Lat:      p.float("dest_lat"),
			Lng:      p.float("dest_lng"),
			RadiusKm: p.float("dest_radius_km"),
		},
		MinDays:      p.integer("min_days"),
		MaxDays:      p.integer("max_days"),
		MinFreeSlots: p.integer("min_free_slots"),
		Categories:   p.list("categories"),
		Query:        p.text("q"),
		Cursor:       p.cursor("cursor"),
		Limit:        p.integer("limit"),
	}

	filter = filter.Normalize()

	// Domain's verdict is merged into the same envelope rather than returned
	// instead of it, so `?near_lat=abc&limit=900` reports both.
	var validation *domain.ValidationError
	if err := filter.Validate(); errors.As(err, &validation) {
		p.errs.Fields = append(p.errs.Fields, validation.Fields...)
	}

	if len(p.errs.Fields) > 0 {
		return filter, p.errs
	}
	return filter, nil
}

// queryParser reads typed values out of a query string, collecting every
// failure instead of returning at the first.
//
// An absent parameter and an empty one are the same thing here: `?q=` is a
// client that built its URL from an empty form field, not a request to search
// for the empty string.
type queryParser struct {
	values url.Values
	errs   *domain.ValidationError
}

func (p *queryParser) raw(name string) (string, bool) {
	list, present := p.values[name]
	if !present {
		return "", false
	}
	if len(list) > 1 {
		// Taking the first and discarding the rest is how `?limit=5&limit=50`
		// becomes a page size the client did not ask for and cannot see it did
		// not get.
		p.errs.Fields = append(p.errs.Fields, domain.FieldError{
			Field: name, Message: "must be given at most once",
		})
		return "", false
	}
	value := strings.TrimSpace(list[0])
	return value, value != ""
}

func (p *queryParser) fail(name, message string) {
	p.errs.Fields = append(p.errs.Fields, domain.FieldError{Field: name, Message: message})
}

// rejectUnknown reports every parameter outside the allowlist.
func (p *queryParser) rejectUnknown(allowed map[string]bool) {
	for name := range p.values {
		if !allowed[name] {
			p.fail(name, "is not a parameter of this endpoint")
		}
	}
}

func (p *queryParser) timestamp(name string) *time.Time {
	value, ok := p.raw(name)
	if !ok {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		p.fail(name, "must be an RFC 3339 timestamp, for example 2026-09-01T00:00:00Z")
		return nil
	}
	utc := parsed.UTC()
	return &utc
}

func (p *queryParser) float(name string) *float64 {
	value, ok := p.raw(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		p.fail(name, "must be a number")
		return nil
	}
	// NaN and ±Inf parse cleanly and would reach PostGIS as coordinates. They
	// are rejected here rather than left for a range check, because no
	// comparison against NaN is ever true and the range check would pass it.
	if parsed != parsed || parsed > 1e308 || parsed < -1e308 {
		p.fail(name, "must be a finite number")
		return nil
	}
	return &parsed
}

func (p *queryParser) integer(name string) *int {
	value, ok := p.raw(name)
	if !ok {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		p.fail(name, "must be a whole number")
		return nil
	}
	return &parsed
}

func (p *queryParser) text(name string) string {
	value, _ := p.raw(name)
	return value
}

// list splits a comma-separated parameter. Empty members are dropped by
// Normalize, so `categories=hiking,,food` is two categories and not an error.
func (p *queryParser) list(name string) []string {
	value, ok := p.raw(name)
	if !ok {
		return nil
	}
	return strings.Split(value, ",")
}

func (p *queryParser) cursor(name string) *domain.Cursor {
	value, ok := p.raw(name)
	if !ok {
		return nil
	}
	cursor, err := domain.DecodeCursor(value)
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

// placeResponse is a named coordinate: the endpoints of a trip as a list card
// shows them. The detail endpoint's `departure` carries no name because it has
// the whole route beside it; a card does not.
type placeResponse struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lng  float64 `json:"lng"`
}

// routeSummaryResponse is the route as a card shows it.
//
// The ellipsis is a boolean, not a "…" appended to Points. A client must never
// have to decide whether the last element of the list is a place or a piece of
// punctuation, and a stop legitimately named "…" would be indistinguishable
// from the marker.
type routeSummaryResponse struct {
	Points      []string `json:"points"`
	TotalPoints int      `json:"total_points"`
	Truncated   bool     `json:"truncated"`
}

// organizerResponse is the block resolved from identity.
//
// Every field but the id is nullable, and that is the degraded mode rather than
// an edge case: when identity cannot be reached the page still renders, with an
// id the SPA can retry on its own and nulls everywhere else.
type organizerResponse struct {
	ID        uuid.UUID `json:"id"`
	FullName  *string   `json:"full_name"`
	PhotoURL  *string   `json:"photo_url"`
	RatingAvg *float64  `json:"rating_avg"`
}

// tripListItemResponse is one card in a list of trips — a search result, a
// /similar suggestion or a row of the dashboard.
//
// One type for all three. The dashboard adds two fields to a card; it does not
// have a different card, and giving it its own type would mean a second
// serializer to keep in step with this one for no gain to any client.
//
// `free_slots` is the same number the detail endpoint calls `spots_left`. The
// two names are the price of not renaming a field that is already in a shipped
// contract; the discovery endpoint uses the name its own filter uses
// (`min_free_slots`), which is the one a caller of this endpoint has in hand.
type tripListItemResponse struct {
	ID            uuid.UUID `json:"id"`
	Title         string    `json:"title"`
	Category      string    `json:"category"`
	Status        string    `json:"status"`
	StartAt       time.Time `json:"start_at"`
	EndAt         time.Time `json:"end_at"`
	Capacity      int       `json:"capacity"`
	ApprovedCount int       `json:"approved_count"`
	FreeSlots     int       `json:"free_slots"`

	Departure   placeResponse        `json:"departure"`
	Destination placeResponse        `json:"destination"`
	Route       routeSummaryResponse `json:"route_summary"`
	Organizer   organizerResponse    `json:"organizer"`

	// The two dashboard fields, absent from a discovery result rather than
	// null in it. `omitempty` on a pointer omits only nil, so an organizer
	// with nothing in their queue still gets `"pending_requests_count": 0` —
	// which is a number the dashboard renders, not a fact it has to infer
	// from a missing key.
	//
	// Additive and optional on purpose: GET /api/trips is a shipped contract
	// and a client that has never heard of these keys must keep working.
	MembershipStatus     *string `json:"membership_status,omitempty"`
	PendingRequestsCount *int    `json:"pending_requests_count,omitempty"`
}

// searchResponse is the paginated list shape CLAUDE.md fixes for every list
// endpoint: items plus an opaque cursor, null when there is no next page.
type searchResponse struct {
	Items      []tripListItemResponse `json:"items"`
	NextCursor *string                `json:"next_cursor"`
}

// similarResponse has no cursor: the endpoint returns at most
// domain.SimilarLimit trips and there is deliberately no way to ask for more.
type similarResponse struct {
	Items []tripListItemResponse `json:"items"`
}

// newTripListItems projects a page onto the wire, filling each organizer block
// from whatever identity returned.
//
// An id that is missing from `organizers` — because identity was unreachable,
// or because the account has been deleted — becomes a block with the id and
// nulls. The two cases are indistinguishable to a client on purpose: in both,
// the only thing it can do is render a placeholder.
func newTripListItems(items []domain.TripListItem, organizers map[uuid.UUID]users.User) []tripListItemResponse {
	out := make([]tripListItemResponse, 0, len(items))
	for _, item := range items {
		row := tripListItemResponse{
			ID:            item.ID,
			Title:         item.Title,
			Category:      item.Category,
			Status:        string(item.Status),
			StartAt:       item.StartAt.UTC(),
			EndAt:         item.EndAt.UTC(),
			Capacity:      item.Capacity,
			ApprovedCount: item.ApprovedCount,
			FreeSlots:     item.SpotsLeft(),
			Departure: placeResponse{
				Name: item.DepartureName,
				Lat:  item.Departure.Lat,
				Lng:  item.Departure.Lng,
			},
			Destination: placeResponse{
				Name: item.DestinationName,
				Lat:  item.Destination.Lat,
				Lng:  item.Destination.Lng,
			},
			Route: routeSummaryResponse{
				// Never nil: an empty JSON array is a route with no stops, and
				// `null` would make every client write the same nil check.
				Points:      orEmpty(item.Route.Points),
				TotalPoints: item.Route.TotalPoints,
				Truncated:   item.Route.Truncated(),
			},
			Organizer: newOrganizer(item.OrganizerID, organizers),

			PendingRequestsCount: item.PendingRequests,
		}
		if item.Membership != "" {
			membership := string(item.Membership)
			row.MembershipStatus = &membership
		}
		out = append(out, row)
	}
	return out
}

func newOrganizer(id uuid.UUID, organizers map[uuid.UUID]users.User) organizerResponse {
	user, known := organizers[id]
	if !known {
		return organizerResponse{ID: id}
	}
	return organizerResponse{
		ID:        id,
		FullName:  &user.FullName,
		PhotoURL:  user.PhotoURL,
		RatingAvg: user.RatingAvg,
	}
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// organizerIDs is the set of ids one page needs resolved — the input to the
// single batch call. A page where one person organises everything is one id,
// not one per row.
func organizerIDs(items []domain.TripListItem) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(items))
	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		if seen[item.OrganizerID] {
			continue
		}
		seen[item.OrganizerID] = true
		ids = append(ids, item.OrganizerID)
	}
	return ids
}
