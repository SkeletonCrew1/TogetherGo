package domain

import (
	"fmt"
	"strings"
	"time"
)

// Discovery bounds. The three limits come from the endpoint contract; the
// route-summary cap is this service's own choice about how much of a route
// belongs on a list card.
const (
	SearchLimitDefault = 20
	SearchLimitMax     = 50
	RadiusMinKm        = 1.0
	RadiusMaxKm        = 500.0

	// RouteSummaryMax is how many stop names a list item carries. A twenty-stop
	// route on a search card is noise; the detail endpoint has the whole thing.
	RouteSummaryMax = 6

	// SimilarLimit and SimilarRadiusKm parameterise GET /api/trips/{id}/similar.
	SimilarLimit    = 5
	SimilarRadiusKm = 100.0

	// SearchQueryMaxLen bounds `q` before it reaches websearch_to_tsquery.
	// Parsing a megabyte of text into a tsquery is work done on behalf of
	// somebody who is not searching for anything.
	SearchQueryMaxLen = 200
)

// RadiusFilter is one "within N km of here" group: near_lat/near_lng/radius_km
// or dest_lat/dest_lng/dest_radius_km.
//
// Three pointers rather than three floats because every one of them has a
// meaningful zero — (0, 0) is a real coordinate — and because the rule this
// type exists to enforce is about presence: all three or none. A caller that
// sends two of them has made a mistake, and defaulting the third would answer
// a question nobody asked.
type RadiusFilter struct {
	Lat      *float64
	Lng      *float64
	RadiusKm *float64
}

// Active reports whether the group was given at all.
func (f RadiusFilter) Active() bool {
	return f.Lat != nil || f.Lng != nil || f.RadiusKm != nil
}

// Complete reports whether all three members arrived.
func (f RadiusFilter) Complete() bool {
	return f.Lat != nil && f.Lng != nil && f.RadiusKm != nil
}

// Meters is the radius in the unit ST_DWithin takes for a geography argument.
// Only meaningful once Complete.
func (f RadiusFilter) Meters() float64 { return *f.RadiusKm * 1000 }

// validate checks one radius group, naming its own parameters so the error
// says `near_lng` and not `radius filter member 2`.
func (f RadiusFilter) validate(v *ValidationError, latName, lngName, radiusName string) {
	if !f.Active() {
		return
	}

	if !f.Complete() {
		// Reported against each missing member rather than once against the
		// group: the client's fix is to add a parameter, and this says which.
		missing := fmt.Sprintf("is required when %s, %s or %s is given", latName, lngName, radiusName)
		if f.Lat == nil {
			v.add(latName, missing)
		}
		if f.Lng == nil {
			v.add(lngName, missing)
		}
		if f.RadiusKm == nil {
			v.add(radiusName, missing)
		}
		return
	}

	if *f.Lat < -90 || *f.Lat > 90 {
		v.add(latName, "must be between -90 and 90")
	}
	if *f.Lng < -180 || *f.Lng > 180 {
		v.add(lngName, "must be between -180 and 180")
	}
	if *f.RadiusKm < RadiusMinKm || *f.RadiusKm > RadiusMaxKm {
		v.add(radiusName, fmt.Sprintf("must be between %g and %g", RadiusMinKm, RadiusMaxKm))
	}
}

// SearchFilter is the whole of GET /api/trips's query string, parsed into
// types. Every field is optional; a zero SearchFilter is "everything that is
// recruiting and has not started yet", which is the browse page.
//
// The two invariants that are *not* fields here — status = 'recruiting' and
// start_at in the future — are not filters and are not negotiable. They are
// applied by the store on every discovery query, so there is no combination of
// query parameters that reveals a draft or a trip that has already left.
type SearchFilter struct {
	DateFrom *time.Time
	DateTo   *time.Time

	Near        RadiusFilter
	Destination RadiusFilter

	MinDays *int
	MaxDays *int

	MinFreeSlots *int

	Categories []string
	Query      string

	// Limit is nil when the client did not ask for a page size; Normalize
	// fills the default. A pointer rather than a zero sentinel because
	// `?limit=0` is a request this endpoint must refuse, not a request it did
	// not receive — and an int cannot tell those apart.
	Limit *int

	Cursor *Cursor
}

// PageSize is the page size to query with. Normalize has already filled it in
// on any filter that reached the store; the fallback is here so that a filter
// built in a test and passed straight through still asks for a bounded page
// rather than for the whole table.
func (f SearchFilter) PageSize() int {
	if f.Limit == nil {
		return SearchLimitDefault
	}
	return *f.Limit
}

// Normalize returns a copy with the default page size applied and text fields
// trimmed. It runs before Validate, never after, so the length checks measure
// what the query will actually use.
func (f SearchFilter) Normalize() SearchFilter {
	out := f
	if out.Limit == nil {
		fallback := SearchLimitDefault
		out.Limit = &fallback
	}
	out.Query = strings.TrimSpace(f.Query)

	if f.Categories != nil {
		// Deduplicated, order preserved. `categories=hiking,hiking` is the same
		// question asked twice and must not widen the `= ANY(...)` array.
		seen := make(map[string]bool, len(f.Categories))
		out.Categories = make([]string, 0, len(f.Categories))
		for _, c := range f.Categories {
			c = strings.TrimSpace(c)
			if c == "" || seen[c] {
				continue
			}
			seen[c] = true
			out.Categories = append(out.Categories, c)
		}
	}

	if f.DateFrom != nil {
		utc := f.DateFrom.UTC()
		out.DateFrom = &utc
	}
	if f.DateTo != nil {
		utc := f.DateTo.UTC()
		out.DateTo = &utc
	}
	return out
}

// Validate checks every rule and reports all failures at once, with each field
// named as the query parameter the client sent.
func (f SearchFilter) Validate() error {
	v := &ValidationError{}

	if f.DateFrom != nil && f.DateTo != nil && f.DateTo.Before(*f.DateFrom) {
		v.add("date_to", "must not be before date_from")
	}

	f.Near.validate(v, "near_lat", "near_lng", "radius_km")
	f.Destination.validate(v, "dest_lat", "dest_lng", "dest_radius_km")

	if f.MinDays != nil && *f.MinDays < 1 {
		v.add("min_days", "must be at least 1")
	}
	if f.MaxDays != nil && *f.MaxDays < 1 {
		v.add("max_days", "must be at least 1")
	}
	if f.MinDays != nil && f.MaxDays != nil && *f.MaxDays < *f.MinDays {
		v.add("max_days", "must not be less than min_days")
	}

	if f.MinFreeSlots != nil && (*f.MinFreeSlots < 0 || *f.MinFreeSlots > CapacityMax) {
		v.add("min_free_slots", fmt.Sprintf("must be between 0 and %d", CapacityMax))
	}

	for _, c := range f.Categories {
		if !IsCategory(c) {
			v.add("categories", fmt.Sprintf("%q is not one of %s", c, strings.Join(Categories, ", ")))
		}
	}

	if len([]rune(f.Query)) > SearchQueryMaxLen {
		v.add("q", fmt.Sprintf("must be at most %d characters", SearchQueryMaxLen))
	}

	if f.Limit != nil && (*f.Limit < 1 || *f.Limit > SearchLimitMax) {
		v.add("limit", fmt.Sprintf("must be between 1 and %d", SearchLimitMax))
	}

	return v.orNil()
}

// RouteSummary is the ordered stop names shown on a list card, capped at
// RouteSummaryMax.
//
// TotalPoints is the route's real length, so a client can render the ellipsis
// without being told how the cap was applied. The marker is a boolean rather
// than a literal "…" appended to Points, because a caller must never have to
// decide whether the last element is a place or a piece of punctuation.
type RouteSummary struct {
	Points      []string
	TotalPoints int
}

// Truncated reports whether stops were left out of Points.
func (r RouteSummary) Truncated() bool { return r.TotalPoints > len(r.Points) }

// TripListItem is one row of a discovery page: the trip, the names of its
// endpoints and enough of its route to recognise it.
//
// It embeds Trip rather than repeating its fields, so SpotsLeft and the
// coordinates come along and there is one definition of what a trip's scalars
// are. What it deliberately does not carry is the roster and the full route —
// a fifty-item page that joined those would be reading a few thousand rows to
// render fifty cards.
// The two dashboard fields are on this type rather than on a second one
// because a card is a card: /my-trips renders the same component search does,
// and a parallel MyTripListItem would mean a parallel serializer, a parallel
// organizer batch call and two definitions of what a trip looks like in a list.
// Both are zero on a discovery result and omitted from its JSON.
type TripListItem struct {
	Trip

	DepartureName   string
	DestinationName string
	Route           RouteSummary

	// Membership is the caller's standing on this trip. Set only by the
	// dashboard queries, which know who is asking; discovery does not and
	// leaves it empty.
	Membership MembershipStatus

	// PendingRequests is how many applications are waiting on this trip. Set
	// only on the organizer tab — it is the count that lets the dashboard
	// render the inline approve/reject list without a second round trip, and it
	// is nobody else's business. A pointer because 0 is a real answer there and
	// "not applicable" is the answer everywhere else.
	PendingRequests *int
}

// SearchPage is one page of trips plus the cursor that continues it. Next is
// nil on the last page — the store knows the difference because it reads one
// row more than it returns.
//
// Shared by every keyset listing of trips: discovery, /similar's fixed page and
// both dashboard tabs. They differ in what they select and how they order it,
// not in the shape of a page.
type SearchPage struct {
	Items []TripListItem
	Next  *Cursor
}
