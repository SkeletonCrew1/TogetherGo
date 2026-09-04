package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Validation bounds. The first four come from the service contract; the last
// three are this service's own choice, because a `text` column with no bound is
// an invitation to store a novel in it.
const (
	TitleMinLen       = 3
	TitleMaxLen       = 120
	CapacityMin       = 2
	CapacityMax       = 50
	PointsMin         = 2
	PointsMax         = 20
	MaxTripDuration   = 60 * 24 * time.Hour
	DescriptionMaxLen = 4000
	PointNameMaxLen   = 120
	TransportMaxLen   = 40
)

// PointInput is one route stop as it arrives from a client: coordinates as
// plain lat/lng, not as geometry.
type PointInput struct {
	Name      string
	Lat       float64
	Lng       float64
	ArriveAt  *time.Time
	Transport *string
}

// SameLocation reports whether two points sit on exactly the same coordinates.
// Exact comparison is deliberate: the rule is "no two *consecutive* points are
// identical", which is about a client sending the same stop twice, not about
// two genuinely distinct stops that happen to be metres apart.
func (p PointInput) SameLocation(other PointInput) bool {
	return p.Lat == other.Lat && p.Lng == other.Lng
}

// TripInput is a complete, self-consistent description of a trip. Both create
// and update validate this same shape — an update merges the patch onto the
// stored trip first (see TripPatch.Apply), so a partial edit is checked against
// the whole result rather than in isolation.
type TripInput struct {
	Title       string
	Description *string
	Category    string
	Capacity    int
	StartAt     time.Time
	EndAt       time.Time
	Points      []PointInput
}

// Departure and Destination are the first and last route points. They are what
// gets denormalised onto the trips row.
//
// Both index into Points and so require a validated input — Validate rejects
// anything with fewer than PointsMin stops, so the store calls these only after
// it has passed.
func (in TripInput) Departure() Coordinates {
	return Coordinates{Lat: in.Points[0].Lat, Lng: in.Points[0].Lng}
}

func (in TripInput) Destination() Coordinates {
	last := in.Points[len(in.Points)-1]
	return Coordinates{Lat: last.Lat, Lng: last.Lng}
}

// Normalize returns a copy with surrounding whitespace stripped and empty
// optional strings collapsed to nil.
//
// It runs before Validate, never after, so that what the length checks measure
// is exactly what the store will write. A title of "  ab  " is three characters
// short, not one character over the minimum.
func (in TripInput) Normalize() TripInput {
	out := in
	out.Title = strings.TrimSpace(in.Title)
	out.Category = strings.TrimSpace(in.Category)
	out.Description = normalizeOptional(in.Description)

	if in.Points != nil {
		out.Points = make([]PointInput, len(in.Points))
		for i, p := range in.Points {
			p.Name = strings.TrimSpace(p.Name)
			p.Transport = normalizeOptional(p.Transport)
			out.Points[i] = p
		}
	}
	// Timestamps are stored and compared in UTC (CLAUDE.md); a client sending
	// +03:00 must not sort differently from one sending Z.
	out.StartAt = in.StartAt.UTC()
	out.EndAt = in.EndAt.UTC()
	return out
}

func normalizeOptional(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// Validate checks every rule and reports all failures at once.
//
// requireFutureStart is false for an edit that leaves start_at alone: a
// recruiting trip that is about to begin must still accept a fixed typo in its
// title, and re-checking an unchanged date against the clock would refuse it.
// Whenever start_at is actually being set — on create, or on an edit that
// carries it — the rule applies.
func (in TripInput) Validate(now time.Time, requireFutureStart bool) error {
	v := &ValidationError{}

	if n := utf8.RuneCountInString(in.Title); n < TitleMinLen || n > TitleMaxLen {
		v.add("title", fmt.Sprintf("must be between %d and %d characters", TitleMinLen, TitleMaxLen))
	}
	if in.Description != nil && utf8.RuneCountInString(*in.Description) > DescriptionMaxLen {
		v.add("description", fmt.Sprintf("must be at most %d characters", DescriptionMaxLen))
	}
	if !IsCategory(in.Category) {
		v.add("category", "must be one of "+strings.Join(Categories, ", "))
	}
	if in.Capacity < CapacityMin || in.Capacity > CapacityMax {
		v.add("capacity", fmt.Sprintf("must be between %d and %d", CapacityMin, CapacityMax))
	}

	switch {
	case in.StartAt.IsZero():
		v.add("start_at", "is required")
	case requireFutureStart && !in.StartAt.After(now):
		v.add("start_at", "must be in the future")
	}

	switch {
	case in.EndAt.IsZero():
		v.add("end_at", "is required")
	case in.StartAt.IsZero():
		// Nothing to compare against; start_at already carries its own error.
	case in.EndAt.Before(in.StartAt):
		v.add("end_at", "must not be before start_at")
	case in.EndAt.Sub(in.StartAt) > MaxTripDuration:
		v.add("end_at", fmt.Sprintf("trip must not last longer than %d days", int(MaxTripDuration.Hours()/24)))
	}

	v.validatePoints(in.Points)

	return v.orNil()
}

func (v *ValidationError) validatePoints(points []PointInput) {
	if len(points) < PointsMin || len(points) > PointsMax {
		v.add("points", fmt.Sprintf("must contain between %d and %d points", PointsMin, PointsMax))
		// The per-point checks below still run: a client that sent one point
		// with a broken latitude should hear about both problems now.
	}

	for i, p := range points {
		path := fmt.Sprintf("points[%d]", i)

		if n := utf8.RuneCountInString(p.Name); n == 0 || n > PointNameMaxLen {
			v.add(path+".name", fmt.Sprintf("must be between 1 and %d characters", PointNameMaxLen))
		}
		if p.Lat < -90 || p.Lat > 90 {
			v.add(path+".lat", "must be between -90 and 90")
		}
		if p.Lng < -180 || p.Lng > 180 {
			v.add(path+".lng", "must be between -180 and 180")
		}
		if p.Transport != nil && utf8.RuneCountInString(*p.Transport) > TransportMaxLen {
			v.add(path+".transport", fmt.Sprintf("must be at most %d characters", TransportMaxLen))
		}
		if i > 0 && p.SameLocation(points[i-1]) {
			v.add(path, "must not repeat the coordinates of the previous point")
		}
	}
}

// TripPatch is a partial edit. A nil field means "leave it alone"; the
// distinction matters because PATCH must not silently reset what it omits.
//
// Description needs two fields because `"description": null` (clear it) and an
// absent key (keep it) are different requests, and one nil pointer cannot say
// which of the two arrived.
type TripPatch struct {
	Title          *string
	DescriptionSet bool
	Description    *string
	Category       *string
	Capacity       *int
	StartAt        *time.Time
	EndAt          *time.Time
	// Points nil means unchanged. A non-nil empty slice is an explicit
	// "replace the route with nothing", which Validate then rejects.
	Points []PointInput
}

// Apply merges the patch onto the trip's current values and returns the result
// to be validated and stored. It never mutates its argument.
func (p TripPatch) Apply(current TripInput) TripInput {
	out := current
	if p.Title != nil {
		out.Title = *p.Title
	}
	if p.DescriptionSet {
		out.Description = p.Description
	}
	if p.Category != nil {
		out.Category = *p.Category
	}
	if p.Capacity != nil {
		out.Capacity = *p.Capacity
	}
	if p.StartAt != nil {
		out.StartAt = *p.StartAt
	}
	if p.EndAt != nil {
		out.EndAt = *p.EndAt
	}
	if p.Points != nil {
		out.Points = p.Points
	}
	return out
}

// IsEmpty reports whether the patch would change nothing.
func (p TripPatch) IsEmpty() bool {
	return p.Title == nil && !p.DescriptionSet && p.Category == nil &&
		p.Capacity == nil && p.StartAt == nil && p.EndAt == nil && p.Points == nil
}
