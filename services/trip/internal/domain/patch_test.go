package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
)

func TestPatchLeavesOmittedFieldsAlone(t *testing.T) {
	current := validInput()

	merged := domain.TripPatch{Title: ptr("A better title")}.Apply(current)

	require.Equal(t, "A better title", merged.Title)
	require.Equal(t, current.Description, merged.Description)
	require.Equal(t, current.Category, merged.Category)
	require.Equal(t, current.Capacity, merged.Capacity)
	require.Equal(t, current.StartAt, merged.StartAt)
	require.Equal(t, current.EndAt, merged.EndAt)
	require.Equal(t, current.Points, merged.Points)
}

// TestPatchDistinguishesNullFromAbsent is why TripPatch carries both
// DescriptionSet and Description: `{"description": null}` clears the field and
// `{}` leaves it, and one nil pointer cannot say which arrived.
func TestPatchDistinguishesNullFromAbsent(t *testing.T) {
	current := validInput()
	require.NotNil(t, current.Description)

	absent := domain.TripPatch{}.Apply(current)
	require.Equal(t, current.Description, absent.Description, "an absent key must not clear the description")

	cleared := domain.TripPatch{DescriptionSet: true, Description: nil}.Apply(current)
	require.Nil(t, cleared.Description, "an explicit null must clear the description")

	replaced := domain.TripPatch{DescriptionSet: true, Description: ptr("new text")}.Apply(current)
	require.Equal(t, "new text", *replaced.Description)
}

func TestPatchReplacesTheWholeRoute(t *testing.T) {
	current := validInput()
	route := []domain.PointInput{
		{Name: "Kyiv", Lat: 50.4501, Lng: 30.5234},
		{Name: "Odesa", Lat: 46.4825, Lng: 30.7233},
	}

	merged := domain.TripPatch{Points: route}.Apply(current)

	require.Equal(t, route, merged.Points, "points are replaced wholesale, never merged element by element")
}

// TestPatchWithAnEmptyRouteIsRejected: a non-nil empty slice is an explicit
// "replace the route with nothing", which is different from omitting the key.
func TestPatchWithAnEmptyRouteIsRejected(t *testing.T) {
	merged := domain.TripPatch{Points: []domain.PointInput{}}.Apply(validInput())

	require.Contains(t, fieldsOf(t, merged.Normalize().Validate(now, false)), "points")
}

// TestPatchIsValidatedAgainstTheMergedResult: moving only end_at can still
// break "end_at must not be before start_at", which is a statement about the
// trip and not about the patch.
func TestPatchIsValidatedAgainstTheMergedResult(t *testing.T) {
	current := validInput()

	merged := domain.TripPatch{EndAt: ptr(current.StartAt.Add(-time.Hour))}.Apply(current)

	require.Contains(t, fieldsOf(t, merged.Normalize().Validate(now, false)), "end_at")
}

func TestPatchDoesNotMutateTheCurrentInput(t *testing.T) {
	current := validInput()
	title := current.Title

	_ = domain.TripPatch{Title: ptr("something else")}.Apply(current)

	require.Equal(t, title, current.Title)
}

func TestPatchIsEmpty(t *testing.T) {
	require.True(t, domain.TripPatch{}.IsEmpty())
	require.False(t, domain.TripPatch{Title: ptr("x")}.IsEmpty())
	require.False(t, domain.TripPatch{DescriptionSet: true}.IsEmpty(),
		"clearing the description is a change, even though the value is nil")
	require.False(t, domain.TripPatch{Points: []domain.PointInput{}}.IsEmpty())
}

func TestTransitionErrorCarriesBothEnds(t *testing.T) {
	err := &domain.TransitionError{From: domain.StatusCancelled, To: domain.StatusRecruiting}

	require.ErrorIs(t, err, domain.ErrInvalidTransition, "the mapper matches on the sentinel")
	require.Contains(t, err.Error(), "cancelled")
	require.Contains(t, err.Error(), "recruiting")
	require.Empty(t, err.Allowed(), "nothing is allowed out of a terminal state")

	fromDraft := &domain.TransitionError{From: domain.StatusDraft, To: domain.StatusCompleted}
	require.ElementsMatch(t,
		[]domain.Status{domain.StatusRecruiting, domain.StatusCancelled},
		fromDraft.Allowed())
}

func TestTripVisibility(t *testing.T) {
	organizer := uuidFromString(t, "11111111-1111-4111-8111-111111111111")
	stranger := uuidFromString(t, "22222222-2222-4222-8222-222222222222")

	for _, status := range domain.AllStatuses() {
		trip := domain.Trip{OrganizerID: organizer, Status: status}

		require.True(t, trip.IsVisibleTo(organizer), "the organizer always sees their own trip")

		if status == domain.StatusDraft {
			require.False(t, trip.IsVisibleTo(stranger), "a draft is invisible to everybody else")
			continue
		}
		require.True(t, trip.IsVisibleTo(stranger), "%s is public", status)
	}
}

func TestSpotsLeft(t *testing.T) {
	require.Equal(t, 7, domain.Trip{Capacity: 8, ApprovedCount: 1}.SpotsLeft())
	require.Equal(t, 0, domain.Trip{Capacity: 8, ApprovedCount: 8}.SpotsLeft())
	// Cannot happen — the trips_approved_fits constraint forbids it — but a
	// negative "spots left" rendered in a UI is worse than a clamped zero.
	require.Equal(t, 0, domain.Trip{Capacity: 8, ApprovedCount: 9}.SpotsLeft())
}

func TestIsCategory(t *testing.T) {
	for _, c := range domain.Categories {
		require.True(t, domain.IsCategory(c))
	}
	require.False(t, domain.IsCategory("Nature"), "the allowlist is case-sensitive")
	require.False(t, domain.IsCategory(""))
	require.False(t, domain.IsCategory("spelunking"))
}
