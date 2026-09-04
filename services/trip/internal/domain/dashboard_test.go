package domain_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/togethergo/trip/internal/domain"
)

func TestMyTripsFilterRequiresAKnownRole(t *testing.T) {
	for _, tc := range []struct {
		name  string
		role  domain.DashboardRole
		valid bool
	}{
		{"organizer", domain.DashboardOrganizer, true},
		{"participant", domain.DashboardParticipant, true},
		{"absent", "", false},
		{"wrong case", "Organizer", false},
		// 'member' is the participants table's role in some other schemas and a
		// plausible guess here. It is not this endpoint's vocabulary.
		{"member", "member", false},
		{"both", "organizer,participant", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := domain.MyTripsFilter{Role: tc.role}.Normalize().Validate()
			if tc.valid {
				require.NoError(t, err)
				return
			}
			require.Equal(t, []string{"role"}, fieldsOf(t, err))
		})
	}
}

func TestMyTripsFilterChecksTheStatusVocabulary(t *testing.T) {
	filter := domain.MyTripsFilter{
		Role:     domain.DashboardOrganizer,
		Statuses: []domain.Status{domain.StatusDraft, "archived"},
	}
	require.Equal(t, []string{"status"}, fieldsOf(t, filter.Normalize().Validate()))

	// Every status in the vocabulary is accepted, including the two that appear
	// nowhere else in the API.
	all := domain.MyTripsFilter{Role: domain.DashboardOrganizer, Statuses: domain.AllStatuses()}
	require.NoError(t, all.Normalize().Validate())
}

func TestMyTripsFilterBoundsThePageSize(t *testing.T) {
	role := domain.DashboardOrganizer

	// Absent means the default, and `limit=0` is a request to refuse rather
	// than a request that was not made.
	require.Equal(t, domain.MyTripsLimitDefault,
		domain.MyTripsFilter{Role: role}.Normalize().PageSize())

	for _, limit := range []int{0, -1, domain.MyTripsLimitMax + 1} {
		filter := domain.MyTripsFilter{Role: role, Limit: &limit}
		require.Equalf(t, []string{"limit"}, fieldsOf(t, filter.Normalize().Validate()), "limit=%d", limit)
	}

	for _, limit := range []int{1, domain.MyTripsLimitMax} {
		filter := domain.MyTripsFilter{Role: role, Limit: &limit}
		require.NoErrorf(t, filter.Normalize().Validate(), "limit=%d", limit)
		require.Equal(t, limit, filter.Normalize().PageSize())
	}
}

func TestMyTripsFilterNormalizeDeduplicatesStatuses(t *testing.T) {
	filter := domain.MyTripsFilter{
		Role: domain.DashboardOrganizer,
		// A repeated status is the same question asked twice and must not
		// widen the `= ANY(...)` array; an empty member comes from a client
		// that built `status=draft,,cancelled` out of a form.
		Statuses: []domain.Status{domain.StatusDraft, " ", domain.StatusDraft, domain.StatusCancelled},
	}.Normalize()

	require.Equal(t, []domain.Status{domain.StatusDraft, domain.StatusCancelled}, filter.Statuses)
	require.NoError(t, filter.Validate())
}

func TestMyTripsFilterReportsEveryFailureAtOnce(t *testing.T) {
	limit := 900
	err := domain.MyTripsFilter{
		Role:     "nobody",
		Statuses: []domain.Status{"archived"},
		Limit:    &limit,
	}.Normalize().Validate()

	require.ElementsMatch(t, []string{"role", "status", "limit"}, fieldsOf(t, err))
}

// --- the viewer block -------------------------------------------------------

func TestNewViewer(t *testing.T) {
	organizer := uuid.New()
	participant := uuid.New()
	outsider := uuid.New()

	detail := &domain.TripDetail{
		Trip: domain.Trip{OrganizerID: organizer},
		Participants: []domain.Participant{
			{UserID: organizer, Role: domain.RoleOrganizer},
			{UserID: participant, Role: domain.RoleParticipant},
		},
	}

	status := func(s domain.JoinRequestStatus) *domain.JoinRequestStatus { return &s }

	for _, tc := range []struct {
		name   string
		viewer uuid.UUID
		latest *domain.JoinRequestStatus
		want   domain.Viewer
	}{
		{
			// The organizer holds a seat and therefore a participants row. That
			// is the honest answer; a client reads is_organizer first.
			name: "organizer", viewer: organizer,
			want: domain.Viewer{IsOrganizer: true, IsParticipant: true},
		},
		{
			// An approved request has become the participants row. Reporting it
			// as well would give the detail page two answers to one question.
			name: "approved participant", viewer: participant, latest: status(domain.JoinApproved),
			want: domain.Viewer{IsParticipant: true},
		},
		{
			name: "pending requester", viewer: outsider, latest: status(domain.JoinPending),
			want: domain.Viewer{JoinRequestStatus: status(domain.JoinPending)},
		},
		{
			name: "rejected requester", viewer: outsider, latest: status(domain.JoinRejected),
			want: domain.Viewer{JoinRequestStatus: status(domain.JoinRejected)},
		},
		{
			// Withdrawing puts a caller back where they started: they may ask
			// again, which is the same position as never having asked.
			name: "withdrawn requester", viewer: outsider, latest: status(domain.JoinCancelled),
			want: domain.Viewer{},
		},
		{
			name: "stranger", viewer: outsider,
			want: domain.Viewer{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, domain.NewViewer(detail, tc.viewer, tc.latest))
		})
	}
}
