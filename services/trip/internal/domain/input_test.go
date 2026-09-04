package domain_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
)

var now = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// validInput is a trip that passes every rule. Each test below breaks exactly
// one thing about it, so a failure names the rule that broke rather than
// leaving the reader to diff two literals.
func validInput() domain.TripInput {
	return domain.TripInput{
		Title:       "Carpathians in autumn",
		Description: ptr("Three days on the Chornohora ridge."),
		Category:    "hiking",
		Capacity:    8,
		StartAt:     now.Add(14 * 24 * time.Hour),
		EndAt:       now.Add(17 * 24 * time.Hour),
		Points: []domain.PointInput{
			{Name: "Lviv", Lat: 49.8397, Lng: 24.0297},
			{Name: "Vorokhta", Lat: 48.2833, Lng: 24.5667, Transport: ptr("train")},
			{Name: "Hoverla", Lat: 48.1600, Lng: 24.5000},
		},
	}
}

// fieldsOf returns the field paths a validation failure reported.
func fieldsOf(t *testing.T, err error) []string {
	t.Helper()
	require.Error(t, err)

	var validation *domain.ValidationError
	require.ErrorAs(t, err, &validation)
	require.ErrorIs(t, err, domain.ErrValidation)

	paths := make([]string, 0, len(validation.Fields))
	for _, f := range validation.Fields {
		require.NotEmpty(t, f.Message, "every field error must carry a message")
		paths = append(paths, f.Field)
	}
	return paths
}

func TestValidInputPasses(t *testing.T) {
	require.NoError(t, validInput().Normalize().Validate(now, true))
}

func TestValidationRejectsOneFieldAtATime(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*domain.TripInput)
		field  string
	}{
		{"title too short", func(in *domain.TripInput) { in.Title = "ab" }, "title"},
		{"title only whitespace", func(in *domain.TripInput) { in.Title = "     " }, "title"},
		{"title too long", func(in *domain.TripInput) { in.Title = string(make([]rune, 121)) }, "title"},
		{"unknown category", func(in *domain.TripInput) { in.Category = "spelunking" }, "category"},
		{"empty category", func(in *domain.TripInput) { in.Category = "" }, "category"},
		{"capacity below two", func(in *domain.TripInput) { in.Capacity = 1 }, "capacity"},
		{"capacity above fifty", func(in *domain.TripInput) { in.Capacity = 51 }, "capacity"},
		{"capacity missing", func(in *domain.TripInput) { in.Capacity = 0 }, "capacity"},
		{"start in the past", func(in *domain.TripInput) { in.StartAt = now.Add(-time.Hour) }, "start_at"},
		{"start exactly now", func(in *domain.TripInput) { in.StartAt = now }, "start_at"},
		{"start missing", func(in *domain.TripInput) { in.StartAt = time.Time{} }, "start_at"},
		{"end before start", func(in *domain.TripInput) { in.EndAt = in.StartAt.Add(-time.Hour) }, "end_at"},
		{"end missing", func(in *domain.TripInput) { in.EndAt = time.Time{} }, "end_at"},
		{
			"longer than sixty days",
			func(in *domain.TripInput) { in.EndAt = in.StartAt.Add(61 * 24 * time.Hour) },
			"end_at",
		},
		{
			"description too long",
			func(in *domain.TripInput) { in.Description = ptr(string(make([]rune, 4001))) },
			"description",
		},
		{
			"latitude out of range",
			func(in *domain.TripInput) { in.Points[1].Lat = 90.1 },
			"points[1].lat",
		},
		{
			"longitude out of range",
			func(in *domain.TripInput) { in.Points[2].Lng = -180.5 },
			"points[2].lng",
		},
		{
			"point without a name",
			func(in *domain.TripInput) { in.Points[0].Name = "  " },
			"points[0].name",
		},
		{
			"transport too long",
			func(in *domain.TripInput) { in.Points[1].Transport = ptr(string(make([]rune, 41))) },
			"points[1].transport",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.break_(&in)

			require.Contains(t, fieldsOf(t, in.Normalize().Validate(now, true)), tc.field)
		})
	}
}

// TestExactlyAtTheBoundsIsAccepted: the limits are inclusive, and an off-by-one
// in either direction would be invisible to the tests above.
func TestExactlyAtTheBoundsIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tweak func(*domain.TripInput)
	}{
		{"title of exactly three characters", func(in *domain.TripInput) { in.Title = "abc" }},
		{"title of exactly 120 characters", func(in *domain.TripInput) { in.Title = string(make([]rune, 120)) }},
		{"capacity of exactly two", func(in *domain.TripInput) { in.Capacity = 2 }},
		{"capacity of exactly fifty", func(in *domain.TripInput) { in.Capacity = 50 }},
		{"exactly sixty days long", func(in *domain.TripInput) { in.EndAt = in.StartAt.Add(60 * 24 * time.Hour) }},
		{"a same-day trip", func(in *domain.TripInput) { in.EndAt = in.StartAt }},
		{"the poles and the antimeridian", func(in *domain.TripInput) {
			in.Points[0].Lat, in.Points[0].Lng = -90, -180
			in.Points[1].Lat, in.Points[1].Lng = 90, 180
		}},
		{"null island is a real coordinate", func(in *domain.TripInput) {
			in.Points[0].Lat, in.Points[0].Lng = 0, 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.tweak(&in)
			require.NoError(t, in.Normalize().Validate(now, true))
		})
	}
}

// TestTitleLengthCountsRunesNotBytes: "Подорож" is 7 characters and 13 bytes; a
// byte-counting implementation would let a two-character Cyrillic title through.
func TestTitleLengthCountsRunesNotBytes(t *testing.T) {
	in := validInput()
	in.Title = "Ль" // two runes, four bytes

	require.Contains(t, fieldsOf(t, in.Normalize().Validate(now, true)), "title")

	in.Title = "Львів"
	require.NoError(t, in.Normalize().Validate(now, true))
}

func TestPointCountBounds(t *testing.T) {
	t.Run("one point is rejected with a field path", func(t *testing.T) {
		in := validInput()
		in.Points = in.Points[:1]

		// The acceptance criterion: a one-point trip fails validation, and the
		// failure names `points`.
		require.Contains(t, fieldsOf(t, in.Normalize().Validate(now, true)), "points")
	})

	t.Run("no points at all is rejected", func(t *testing.T) {
		in := validInput()
		in.Points = nil
		require.Contains(t, fieldsOf(t, in.Normalize().Validate(now, true)), "points")
	})

	t.Run("two points is the minimum and is accepted", func(t *testing.T) {
		in := validInput()
		in.Points = in.Points[:2]
		require.NoError(t, in.Normalize().Validate(now, true))
	})

	t.Run("twenty points is accepted, twenty-one is not", func(t *testing.T) {
		in := validInput()
		in.Points = make([]domain.PointInput, 20)
		for i := range in.Points {
			in.Points[i] = domain.PointInput{Name: "stop", Lat: float64(i), Lng: float64(i)}
		}
		require.NoError(t, in.Normalize().Validate(now, true))

		in.Points = append(in.Points, domain.PointInput{Name: "one too many", Lat: 21, Lng: 21})
		require.Contains(t, fieldsOf(t, in.Normalize().Validate(now, true)), "points")
	})
}

func TestConsecutiveDuplicatePoints(t *testing.T) {
	t.Run("two identical neighbours are rejected", func(t *testing.T) {
		in := validInput()
		in.Points[2].Lat, in.Points[2].Lng = in.Points[1].Lat, in.Points[1].Lng

		require.Contains(t, fieldsOf(t, in.Normalize().Validate(now, true)), "points[2]")
	})

	t.Run("a different name does not make them different places", func(t *testing.T) {
		in := validInput()
		in.Points[2] = domain.PointInput{Name: "Vorokhta again", Lat: in.Points[1].Lat, Lng: in.Points[1].Lng}

		require.Contains(t, fieldsOf(t, in.Normalize().Validate(now, true)), "points[2]")
	})

	t.Run("a round trip returning to its start is fine", func(t *testing.T) {
		// Only *consecutive* repeats are rejected. Ending where you started is
		// the single most common shape of route there is.
		in := validInput()
		in.Points = append(in.Points, domain.PointInput{Name: "Lviv", Lat: 49.8397, Lng: 24.0297})

		require.NoError(t, in.Normalize().Validate(now, true))
	})
}

// TestValidationReportsEveryProblemAtOnce: a form that gets one error per round
// trip takes as many round trips as it has mistakes.
func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	in := domain.TripInput{
		Title:    "x",
		Category: "teleportation",
		Capacity: 99,
		StartAt:  now.Add(-time.Hour),
		EndAt:    now.Add(-2 * time.Hour),
		Points:   []domain.PointInput{{Name: "", Lat: 200, Lng: 400}},
	}

	fields := fieldsOf(t, in.Normalize().Validate(now, true))
	require.Subset(t, fields, []string{
		"title", "category", "capacity", "start_at", "end_at",
		"points", "points[0].name", "points[0].lat", "points[0].lng",
	})
}

func TestNormalize(t *testing.T) {
	in := domain.TripInput{
		Title:       "  Carpathians  ",
		Description: ptr("   "),
		Category:    " hiking ",
		Points: []domain.PointInput{
			{Name: "  Lviv ", Transport: ptr("  ")},
			{Name: "Hoverla", Transport: ptr("  bus  ")},
		},
		StartAt: now.In(time.FixedZone("EEST", 3*60*60)),
		EndAt:   now.In(time.FixedZone("EEST", 3*60*60)),
	}

	out := in.Normalize()

	require.Equal(t, "Carpathians", out.Title)
	require.Equal(t, "hiking", out.Category)
	require.Nil(t, out.Description, "a description of only whitespace is no description")
	require.Equal(t, "Lviv", out.Points[0].Name)
	require.Nil(t, out.Points[0].Transport)
	require.Equal(t, "bus", *out.Points[1].Transport)
	require.Equal(t, time.UTC, out.StartAt.Location(), "timestamps are normalised to UTC")
	require.Equal(t, time.UTC, out.EndAt.Location())
	require.True(t, out.StartAt.Equal(now))
}

// TestNormalizeDoesNotMutateItsInput: Normalize returns a copy, so a caller that
// keeps the original — as the update path does, to compare against the patch —
// still has it.
func TestNormalizeDoesNotMutateItsInput(t *testing.T) {
	in := validInput()
	in.Title = "  spaced  "
	in.Points[0].Name = "  Lviv  "

	_ = in.Normalize()

	require.Equal(t, "  spaced  ", in.Title)
	require.Equal(t, "  Lviv  ", in.Points[0].Name)
}

func TestDepartureAndDestination(t *testing.T) {
	in := validInput()

	require.Equal(t, domain.Coordinates{Lat: 49.8397, Lng: 24.0297}, in.Departure())
	require.Equal(t, domain.Coordinates{Lat: 48.16, Lng: 24.5}, in.Destination())
}

func TestFutureStartIsOptionalForEdits(t *testing.T) {
	in := validInput()
	in.StartAt = now.Add(-24 * time.Hour)
	in.EndAt = now.Add(-12 * time.Hour)

	// An edit that does not touch start_at must not be refused because the trip
	// has meanwhile begun.
	require.NoError(t, in.Normalize().Validate(now, false))

	// An edit that does touch it is held to the rule.
	require.Contains(t, fieldsOf(t, in.Normalize().Validate(now, true)), "start_at")
}
