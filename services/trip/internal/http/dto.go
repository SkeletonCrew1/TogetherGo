package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/togethergo/trip/internal/domain"
)

// maxBodyBytes caps a request body. Twenty route points with long names is a
// few kilobytes; 64 KiB is generous and still small enough that a client
// streaming megabytes at the endpoint is cut off rather than buffered.
const maxBodyBytes = 64 << 10

// nullable distinguishes an absent JSON key from one explicitly set to null.
//
// PATCH needs the difference: `{"description": null}` clears the description
// and `{}` leaves it alone, and a plain *string cannot say which arrived. Set
// is true whenever the key was present at all; Value is nil only for an
// explicit null.
type nullable[T any] struct {
	Set   bool
	Value *T
}

func (n *nullable[T]) UnmarshalJSON(data []byte) error {
	n.Set = true
	if string(data) == "null" {
		n.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	n.Value = &value
	return nil
}

// pointRequest is one route stop as it arrives.
//
// Lat and Lng are pointers because 0 is a real coordinate. Every other field's
// zero value is already out of range and can be left to the domain validator,
// but a point that omits `lat` would otherwise be stored at Null Island without
// a word of complaint.
type pointRequest struct {
	Name      string     `json:"name"`
	Lat       *float64   `json:"lat"`
	Lng       *float64   `json:"lng"`
	ArriveAt  *time.Time `json:"arrive_at"`
	Transport *string    `json:"transport"`
}

type createTripRequest struct {
	Title       string         `json:"title"`
	Description *string        `json:"description"`
	Category    string         `json:"category"`
	Capacity    int            `json:"capacity"`
	StartAt     time.Time      `json:"start_at"`
	EndAt       time.Time      `json:"end_at"`
	Points      []pointRequest `json:"points"`
}

type updateTripRequest struct {
	Title       *string          `json:"title"`
	Description nullable[string] `json:"description"`
	Category    *string          `json:"category"`
	Capacity    *int             `json:"capacity"`
	StartAt     *time.Time       `json:"start_at"`
	EndAt       *time.Time       `json:"end_at"`
	// nil means "leave the route alone". An explicit `[]` is a request to
	// replace it with nothing, which the domain validator then rejects.
	Points []pointRequest `json:"points"`
}

// toInput converts a decoded create body into the domain type, reporting any
// coordinate the client left out.
//
// Presence is checked here and ranges are checked in the domain: "did the
// client send this key" is a fact about the encoding, "is -200 a longitude" is
// a rule about the world.
func (b createTripRequest) toInput() (domain.TripInput, error) {
	points, err := toPointInputs(b.Points)
	if err != nil {
		return domain.TripInput{}, err
	}
	return domain.TripInput{
		Title:       b.Title,
		Description: b.Description,
		Category:    b.Category,
		Capacity:    b.Capacity,
		StartAt:     b.StartAt,
		EndAt:       b.EndAt,
		Points:      points,
	}, nil
}

func (b updateTripRequest) toPatch() (domain.TripPatch, error) {
	patch := domain.TripPatch{
		Title:          b.Title,
		DescriptionSet: b.Description.Set,
		Description:    b.Description.Value,
		Category:       b.Category,
		Capacity:       b.Capacity,
		StartAt:        b.StartAt,
		EndAt:          b.EndAt,
	}
	if b.Points != nil {
		points, err := toPointInputs(b.Points)
		if err != nil {
			return domain.TripPatch{}, err
		}
		patch.Points = points
	}
	return patch, nil
}

func toPointInputs(points []pointRequest) ([]domain.PointInput, error) {
	missing := &domain.ValidationError{}
	out := make([]domain.PointInput, len(points))

	for i, p := range points {
		path := fmt.Sprintf("points[%d]", i)
		if p.Lat == nil {
			missing.Fields = append(missing.Fields, domain.FieldError{Field: path + ".lat", Message: "is required"})
		}
		if p.Lng == nil {
			missing.Fields = append(missing.Fields, domain.FieldError{Field: path + ".lng", Message: "is required"})
		}
		out[i] = domain.PointInput{
			Name:      p.Name,
			Lat:       derefOr(p.Lat, 0),
			Lng:       derefOr(p.Lng, 0),
			ArriveAt:  p.ArriveAt,
			Transport: p.Transport,
		}
	}

	if len(missing.Fields) > 0 {
		return nil, missing
	}
	return out, nil
}

func derefOr[T any](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

// decodeBody reads a JSON request body into target.
//
// DisallowUnknownFields is on. A client that sends `titel` has made a mistake,
// and silently ignoring the key would store an empty title and report success —
// the failure would surface much later, as a trip nobody can find.
func decodeBody(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		return bodyError(err)
	}
	// Exactly one JSON value per request. A body of `{} {}` is a confused
	// client, and taking the first object and ignoring the rest hides that.
	if err := decoder.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errBadBody("The request body must contain exactly one JSON object.")
	}
	return nil
}

// errBadBody is a body that could not be decoded at all. It is deliberately not
// a *domain.ValidationError: nothing about the domain was reached.
type errBadBody string

func (e errBadBody) Error() string { return string(e) }

func bodyError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		// A well-formed body with a value of the wrong type is close enough to
		// a rejected field to be reported as one, with the same field path a
		// range violation would carry.
		field := typeErr.Field
		if field == "" {
			field = "body"
		}
		return &domain.ValidationError{Fields: []domain.FieldError{{
			Field:   field,
			Message: fmt.Sprintf("must be a %s", typeErr.Type.String()),
		}}}
	}

	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return errBadBody(fmt.Sprintf("The request body must be at most %d bytes.", maxBodyBytes))
	}

	if errors.Is(err, io.EOF) {
		return errBadBody("The request body is empty.")
	}
	return errBadBody("The request body is not valid JSON.")
}

// tripIDFrom parses the {tripID} path parameter.
func tripIDFrom(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, &domain.ValidationError{Fields: []domain.FieldError{{
			Field:   "id",
			Message: "must be a uuid",
		}}}
	}
	return id, nil
}
