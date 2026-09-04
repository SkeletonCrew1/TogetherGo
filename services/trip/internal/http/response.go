package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/togethergo/trip/internal/domain"
)

// coordinatesResponse is a WGS84 point. Always lat then lng in JSON, which is
// the order every mapping client expects and the reverse of the order PostGIS
// takes them in — the store is the only place that has to know that.
type coordinatesResponse struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type pointResponse struct {
	ID        uuid.UUID  `json:"id"`
	Seq       int        `json:"seq"`
	Name      string     `json:"name"`
	Lat       float64    `json:"lat"`
	Lng       float64    `json:"lng"`
	ArriveAt  *time.Time `json:"arrive_at"`
	Transport *string    `json:"transport"`
}

type participantResponse struct {
	UserID   uuid.UUID `json:"user_id"`
	Role     string    `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

// tripResponse is the body of every trip endpoint that returns a trip.
//
// Capacity is reported as three numbers rather than one: `capacity` is what the
// organizer set, `approved_count` is how many seats are taken, `spots_left` is
// the subtraction the SPA would otherwise do in three places and get wrong in
// one of them.
type tripResponse struct {
	ID            uuid.UUID `json:"id"`
	OrganizerID   uuid.UUID `json:"organizer_id"`
	Title         string    `json:"title"`
	Description   *string   `json:"description"`
	Category      string    `json:"category"`
	Status        string    `json:"status"`
	Capacity      int       `json:"capacity"`
	ApprovedCount int       `json:"approved_count"`
	SpotsLeft     int       `json:"spots_left"`
	StartAt       time.Time `json:"start_at"`
	EndAt         time.Time `json:"end_at"`

	// The denormalised endpoints, served alongside the route rather than
	// instead of it: a list card needs two coordinates, a detail map needs all
	// twenty, and this endpoint answers both.
	Departure   coordinatesResponse `json:"departure"`
	Destination coordinatesResponse `json:"destination"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Points       []pointResponse       `json:"points"`
	Participants []participantResponse `json:"participants"`

	// Viewer is the caller's own relationship to this trip. Present on GET
	// /api/trips/{id} and absent everywhere else, because the mutations that
	// return a trip are all acts the caller already knows the outcome of —
	// publishing a trip does not leave anyone wondering whether they organize
	// it. Absent rather than null so the key means something wherever it
	// appears. See domain.Viewer.
	Viewer *viewerResponse `json:"viewer,omitempty"`
}

// newTripResponse projects a domain detail onto the wire shape.
//
// Every timestamp is forced to UTC on the way out. Postgres hands back
// timestamptz in the session's time zone, so without this the same trip could
// serialise with a different offset depending on which container read it —
// identical instants that no client would compare as equal (CLAUDE.md: UTC,
// RFC 3339).
func newTripResponse(detail *domain.TripDetail) tripResponse {
	trip := detail.Trip

	points := make([]pointResponse, 0, len(detail.Points))
	for _, p := range detail.Points {
		points = append(points, pointResponse{
			ID:        p.ID,
			Seq:       p.Seq,
			Name:      p.Name,
			Lat:       p.Lat,
			Lng:       p.Lng,
			ArriveAt:  utcPtr(p.ArriveAt),
			Transport: p.Transport,
		})
	}

	participants := make([]participantResponse, 0, len(detail.Participants))
	for _, p := range detail.Participants {
		participants = append(participants, participantResponse{
			UserID:   p.UserID,
			Role:     p.Role,
			JoinedAt: p.JoinedAt.UTC(),
		})
	}

	return tripResponse{
		ID:            trip.ID,
		OrganizerID:   trip.OrganizerID,
		Title:         trip.Title,
		Description:   trip.Description,
		Category:      trip.Category,
		Status:        string(trip.Status),
		Capacity:      trip.Capacity,
		ApprovedCount: trip.ApprovedCount,
		SpotsLeft:     trip.SpotsLeft(),
		StartAt:       trip.StartAt.UTC(),
		EndAt:         trip.EndAt.UTC(),
		Departure:     coordinatesResponse{Lat: trip.Departure.Lat, Lng: trip.Departure.Lng},
		Destination:   coordinatesResponse{Lat: trip.Destination.Lat, Lng: trip.Destination.Lng},
		CreatedAt:     trip.CreatedAt.UTC(),
		UpdatedAt:     trip.UpdatedAt.UTC(),
		Points:        points,
		Participants:  participants,
	}
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}
