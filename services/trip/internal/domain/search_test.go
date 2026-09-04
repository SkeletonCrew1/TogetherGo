package domain_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
)

func TestCursorRoundTrips(t *testing.T) {
	original := domain.Cursor{
		// Microsecond precision, because that is what timestamptz stores and
		// what a cursor truncated to the second would lose.
		StartAt: time.Date(2026, 9, 14, 8, 0, 0, 123456000, time.UTC),
		ID:      uuid.MustParse("2f1e9f7a-9c47-4f3a-9a20-6b7d8e4c1a55"),
	}

	decoded, err := domain.DecodeCursor(original.Encode())
	require.NoError(t, err)
	require.NotNil(t, decoded)
	require.True(t, original.StartAt.Equal(decoded.StartAt),
		"want %s, got %s", original.StartAt, decoded.StartAt)
	require.Equal(t, original.ID, decoded.ID)
}

func TestCursorNormalisesToUTC(t *testing.T) {
	kyiv := time.FixedZone("EEST", 3*3600)
	cursor := domain.Cursor{
		StartAt: time.Date(2026, 9, 14, 11, 0, 0, 0, kyiv),
		ID:      uuid.New(),
	}

	decoded, err := domain.DecodeCursor(cursor.Encode())
	require.NoError(t, err)
	require.Equal(t, time.UTC, decoded.StartAt.Location())
	require.True(t, cursor.StartAt.Equal(decoded.StartAt), "the instant survives the round trip")
}

// TestDecodeCursorRejectsRubbish: every one of these arrives from a client, and
// none of them may panic. A cursor is opaque, which means a client will
// eventually send something that is not one.
func TestDecodeCursorRejectsRubbish(t *testing.T) {
	valid := domain.Cursor{StartAt: time.Now().UTC(), ID: uuid.New()}
	encode := func(raw string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(raw))
	}

	for _, tc := range []struct{ name, cursor string }{
		{"not base64", "!!!!not base64!!!!"},
		{"base64 of nothing useful", encode("hello")},
		{"no separator", encode("2026-09-14T08:00:00Z" + valid.ID.String())},
		{"empty halves", encode("|")},
		{"timestamp is not a timestamp", encode("yesterday|" + valid.ID.String())},
		{"id is not a uuid", encode("2026-09-14T08:00:00Z|not-a-uuid")},
		{"halves the wrong way round", encode(valid.ID.String() + "|2026-09-14T08:00:00Z")},
		{"trailing separator", encode("2026-09-14T08:00:00Z|" + valid.ID.String() + "|extra")},
		{"a whole json object", encode(`{"start_at":"2026-09-14T08:00:00Z"}`)},
		{"far too long", strings.Repeat("A", 4096)},
		{"a sql fragment", encode("2026-09-14T08:00:00Z|' OR 1=1 --")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := domain.DecodeCursor(tc.cursor)
			require.Nil(t, decoded)
			require.Error(t, err)

			var validation *domain.ValidationError
			require.ErrorAs(t, err, &validation, "a bad cursor is a 400, not a 500")
			require.Len(t, validation.Fields, 1)
			require.Equal(t, "cursor", validation.Fields[0].Field)
		})
	}
}

func TestDecodeCursorAcceptsAnEmptyString(t *testing.T) {
	// An absent cursor and an empty one are the same request: the first page.
	decoded, err := domain.DecodeCursor("")
	require.NoError(t, err)
	require.Nil(t, decoded)
}

// TestDecodeCursorAcceptsBothBase64Alphabets: we emit unpadded base64url, but
// cursors pass through client code and URL builders that may re-encode them.
func TestDecodeCursorAcceptsBothBase64Alphabets(t *testing.T) {
	cursor := domain.Cursor{StartAt: time.Date(2026, 9, 14, 8, 0, 0, 0, time.UTC), ID: uuid.New()}
	raw := cursor.StartAt.Format(time.RFC3339Nano) + "|" + cursor.ID.String()

	for name, encoded := range map[string]string{
		"raw url":  base64.RawURLEncoding.EncodeToString([]byte(raw)),
		"url":      base64.URLEncoding.EncodeToString([]byte(raw)),
		"raw std":  base64.RawStdEncoding.EncodeToString([]byte(raw)),
		"standard": base64.StdEncoding.EncodeToString([]byte(raw)),
	} {
		t.Run(name, func(t *testing.T) {
			decoded, err := domain.DecodeCursor(encoded)
			require.NoError(t, err)
			require.Equal(t, cursor.ID, decoded.ID)
		})
	}
}

// --- the filter -------------------------------------------------------------

func ptrInt(v int) *int { return &v }

func TestSearchFilterDefaultsTheLimit(t *testing.T) {
	filter := domain.SearchFilter{}.Normalize()
	require.NotNil(t, filter.Limit)
	require.Equal(t, domain.SearchLimitDefault, *filter.Limit)
	require.Equal(t, domain.SearchLimitDefault, filter.PageSize())
	require.NoError(t, filter.Validate())
}

// TestSearchFilterDistinguishesAnExplicitZero: `?limit=0` is a request this
// endpoint refuses, not a request it did not receive.
func TestSearchFilterDistinguishesAnExplicitZero(t *testing.T) {
	zero := 0
	require.Equal(t, []string{"limit"},
		fieldsOf(t, domain.SearchFilter{Limit: &zero}.Normalize().Validate()))
}

// TestSearchFilterRadiusIsAllOrNone is the second acceptance criterion.
func TestSearchFilterRadiusIsAllOrNone(t *testing.T) {
	lat, lng, radius := 48.9226, 24.7111, 50.0

	t.Run("all three", func(t *testing.T) {
		filter := domain.SearchFilter{
			Near: domain.RadiusFilter{Lat: &lat, Lng: &lng, RadiusKm: &radius},
		}.Normalize()
		require.NoError(t, filter.Validate())
	})

	t.Run("none", func(t *testing.T) {
		require.NoError(t, domain.SearchFilter{}.Normalize().Validate())
	})

	t.Run("lat without lng", func(t *testing.T) {
		filter := domain.SearchFilter{
			Near: domain.RadiusFilter{Lat: &lat, RadiusKm: &radius},
		}.Normalize()

		fields := fieldsOf(t, filter.Validate())
		require.Equal(t, []string{"near_lng"}, fields)
	})

	t.Run("coordinates without a radius", func(t *testing.T) {
		filter := domain.SearchFilter{
			Near: domain.RadiusFilter{Lat: &lat, Lng: &lng},
		}.Normalize()
		require.Equal(t, []string{"radius_km"}, fieldsOf(t, filter.Validate()))
	})

	t.Run("the destination group is checked independently", func(t *testing.T) {
		filter := domain.SearchFilter{
			Near:        domain.RadiusFilter{Lat: &lat, Lng: &lng, RadiusKm: &radius},
			Destination: domain.RadiusFilter{Lat: &lat},
		}.Normalize()
		require.ElementsMatch(t, []string{"dest_lng", "dest_radius_km"}, fieldsOf(t, filter.Validate()))
	})
}

func TestSearchFilterChecksRanges(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }

	for _, tc := range []struct {
		name   string
		filter domain.SearchFilter
		fields []string
	}{
		{
			"radius below the minimum",
			domain.SearchFilter{Near: domain.RadiusFilter{Lat: f(48), Lng: f(24), RadiusKm: f(0.5)}},
			[]string{"radius_km"},
		},
		{
			"radius above the maximum",
			domain.SearchFilter{Near: domain.RadiusFilter{Lat: f(48), Lng: f(24), RadiusKm: f(501)}},
			[]string{"radius_km"},
		},
		{
			"latitude off the planet",
			domain.SearchFilter{Near: domain.RadiusFilter{Lat: f(91), Lng: f(24), RadiusKm: f(50)}},
			[]string{"near_lat"},
		},
		{"no limit at all takes the default", domain.SearchFilter{}, nil},
		{"an explicit limit of zero", domain.SearchFilter{Limit: i(0)}, []string{"limit"}},
		{"limit above the maximum", domain.SearchFilter{Limit: i(51)}, []string{"limit"}},
		{"negative limit", domain.SearchFilter{Limit: i(-1)}, []string{"limit"}},
		{"zero days", domain.SearchFilter{MinDays: i(0)}, []string{"min_days"}},
		{"max below min", domain.SearchFilter{MinDays: i(5), MaxDays: i(2)}, []string{"max_days"}},
		{"negative free slots", domain.SearchFilter{MinFreeSlots: i(-1)}, []string{"min_free_slots"}},
		{"a category outside the allowlist", domain.SearchFilter{Categories: []string{"spelunking"}}, []string{"categories"}},
		{"a query that is a novel", domain.SearchFilter{Query: strings.Repeat("x", 201)}, []string{"q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.filter.Normalize().Validate()
			if tc.fields == nil {
				require.NoError(t, err)
				return
			}
			require.ElementsMatch(t, tc.fields, fieldsOf(t, err))
		})
	}
}

func TestSearchFilterReportsEveryProblemAtOnce(t *testing.T) {
	lat := 48.9226
	err := domain.SearchFilter{
		Near:       domain.RadiusFilter{Lat: &lat},
		Limit:      ptrInt(900),
		Categories: []string{"spelunking"},
	}.Normalize().Validate()

	require.ElementsMatch(t,
		[]string{"near_lng", "radius_km", "limit", "categories"},
		fieldsOf(t, err),
		"a client that gets one error per round trip needs as many round trips as it has mistakes")
}

func TestSearchFilterNormalizesCategories(t *testing.T) {
	filter := domain.SearchFilter{
		Categories: []string{" hiking ", "hiking", "", "food"},
	}.Normalize()

	require.Equal(t, []string{"hiking", "food"}, filter.Categories,
		"deduplicated, trimmed, and in the order the client sent them")
	require.NoError(t, filter.Validate())
}

func TestSearchFilterRejectsAnInvertedDateWindow(t *testing.T) {
	from := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, -3)

	err := domain.SearchFilter{DateFrom: &from, DateTo: &to}.Normalize().Validate()
	require.Equal(t, []string{"date_to"}, fieldsOf(t, err))
}

// --- route summary ----------------------------------------------------------

func TestRouteSummaryTruncation(t *testing.T) {
	full := domain.RouteSummary{Points: []string{"a", "b", "c"}, TotalPoints: 3}
	require.False(t, full.Truncated())

	capped := domain.RouteSummary{
		Points:      []string{"a", "b", "c", "d", "e", "f"},
		TotalPoints: 9,
	}
	require.True(t, capped.Truncated())
	require.Len(t, capped.Points, domain.RouteSummaryMax)
}
