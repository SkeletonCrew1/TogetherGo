// Package domain holds the trip service's types, its validation rules and its
// status state machine. Nothing here talks to a database, an HTTP request or
// the broker; everything here is testable with no fixtures.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// Role of a participant on a trip.
const (
	RoleOrganizer   = "organizer"
	RoleParticipant = "participant"
)

// Categories is the fixed allowlist a trip's category must come from. A closed
// vocabulary rather than free text because search facets and the SPA's filter
// UI are both generated from it.
var Categories = []string{"nature", "city", "abroad", "hiking", "food", "other"}

// IsCategory reports whether value is in the allowlist.
func IsCategory(value string) bool {
	for _, c := range Categories {
		if c == value {
			return true
		}
	}
	return false
}

// Point is one stop on a trip's route. Seq is dense and zero-based; the pair
// (TripID, Seq) is unique.
type Point struct {
	ID        uuid.UUID
	Seq       int
	Name      string
	Lat       float64
	Lng       float64
	ArriveAt  *time.Time
	Transport *string
}

// Participant is a user occupying a seat on a trip.
type Participant struct {
	UserID   uuid.UUID
	Role     string
	JoinedAt time.Time
}

// Trip is the aggregate root, without its route or its roster.
//
// Departure and Destination are the coordinates of the first and last Point,
// denormalised onto the trips row so the search radius filter is an indexed
// predicate. See the migration for why, and Store.writePoints for the guarantee
// that they cannot drift.
type Trip struct {
	ID            uuid.UUID
	OrganizerID   uuid.UUID
	Title         string
	Description   *string
	Category      string
	Status        Status
	Capacity      int
	ApprovedCount int
	StartAt       time.Time
	EndAt         time.Time
	Departure     Coordinates
	Destination   Coordinates
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SpotsLeft is the number of seats still available.
func (t Trip) SpotsLeft() int {
	left := t.Capacity - t.ApprovedCount
	if left < 0 {
		return 0
	}
	return left
}

// IsVisibleTo reports whether viewer is allowed to know this trip exists.
//
// A draft is the organizer's private workspace: to everybody else it does not
// exist, which is why an unauthorised read answers 404 and not 403. A 403 would
// confirm the id is real, and trip ids are guessable enough in aggregate that
// "which of these ids are drafts" is information worth withholding.
func (t Trip) IsVisibleTo(viewer uuid.UUID) bool {
	return t.Status != StatusDraft || t.OrganizerID == viewer
}

// Coordinates is a WGS84 point. Latitude and longitude in that order in Go,
// but PostGIS ST_MakePoint takes them the other way round; the store package is
// the only place that has to remember which.
type Coordinates struct {
	Lat float64
	Lng float64
}

// TripDetail is a trip together with its ordered route and its roster — the
// payload of GET /api/trips/{id} and the return value of every mutation.
type TripDetail struct {
	Trip         Trip
	Points       []Point
	Participants []Participant
}
