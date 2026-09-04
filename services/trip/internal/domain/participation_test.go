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

func TestValidateJoinMessage(t *testing.T) {
	require.NoError(t, domain.ValidateJoinMessage(nil))
	require.NoError(t, domain.ValidateJoinMessage(ptr("")))
	require.NoError(t, domain.ValidateJoinMessage(ptr(strings.Repeat("a", domain.JoinMessageMaxLen))))

	err := domain.ValidateJoinMessage(ptr(strings.Repeat("a", domain.JoinMessageMaxLen+1)))
	require.ErrorIs(t, err, domain.ErrValidation)

	// Runes, not bytes. Five hundred Cyrillic characters is a thousand bytes
	// and is still five hundred characters, which is what the API documents and
	// what a textarea counts.
	require.NoError(t, domain.ValidateJoinMessage(ptr(strings.Repeat("я", domain.JoinMessageMaxLen))))
	require.Error(t, domain.ValidateJoinMessage(ptr(strings.Repeat("я", domain.JoinMessageMaxLen+1))))
}

func TestNormalizeInviteIDs(t *testing.T) {
	first, second := uuid.New(), uuid.New()

	t.Run("keeps order and drops repeats", func(t *testing.T) {
		got, err := domain.NormalizeInviteIDs([]uuid.UUID{first, second, first})
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{first, second}, got)
	})

	t.Run("counts the limit after deduplication", func(t *testing.T) {
		// One person named far too many times is one person, not an oversized
		// request.
		ids := make([]uuid.UUID, domain.InviteBatchMax+10)
		for i := range ids {
			ids[i] = first
		}
		got, err := domain.NormalizeInviteIDs(ids)
		require.NoError(t, err)
		require.Len(t, got, 1)
	})

	t.Run("rejects too many distinct users", func(t *testing.T) {
		ids := make([]uuid.UUID, domain.InviteBatchMax+1)
		for i := range ids {
			ids[i] = uuid.New()
		}
		_, err := domain.NormalizeInviteIDs(ids)
		require.ErrorIs(t, err, domain.ErrValidation)
	})

	t.Run("rejects an empty list", func(t *testing.T) {
		_, err := domain.NormalizeInviteIDs(nil)
		require.ErrorIs(t, err, domain.ErrValidation)
	})

	t.Run("rejects the nil uuid, naming the position", func(t *testing.T) {
		_, err := domain.NormalizeInviteIDs([]uuid.UUID{first, uuid.Nil})
		var validation *domain.ValidationError
		require.ErrorAs(t, err, &validation)
		require.Equal(t, "user_ids[1]", validation.Fields[0].Field)
	})
}

func TestJoinRequestCursorRoundTrips(t *testing.T) {
	for _, decided := range []bool{false, true} {
		original := domain.JoinRequestCursor{
			Decided: decided,
			// Microseconds, because timestamptz keeps them and a cursor
			// truncated to the second would re-read every row sharing it.
			CreatedAt: time.Date(2026, 8, 28, 12, 0, 0, 123456000, time.UTC),
			ID:        uuid.New(),
		}

		decoded, err := domain.DecodeJoinRequestCursor(original.Encode())
		require.NoError(t, err)
		require.NotNil(t, decoded)
		require.Equal(t, original.Decided, decoded.Decided)
		require.True(t, original.CreatedAt.Equal(decoded.CreatedAt))
		require.Equal(t, original.ID, decoded.ID)
	}
}

// A cursor travels through client code, URL builders and copy-paste. Every one
// of these is a failure the client controls, and none of them may be a panic.
func TestDecodeJoinRequestCursorRejectsRubbish(t *testing.T) {
	empty, err := domain.DecodeJoinRequestCursor("")
	require.NoError(t, err)
	require.Nil(t, empty, "no cursor means the first page, not an error")

	for name, raw := range map[string]string{
		"not base64":            "!!!!",
		"no separators":         encode("nonsense"),
		"one separator":         encode("0|2026-08-28T12:00:00Z"),
		"an unknown rank":       encode("2|2026-08-28T12:00:00Z|" + uuid.NewString()),
		"a bad timestamp":       encode("0|yesterday|" + uuid.NewString()),
		"a bad id":              encode("0|2026-08-28T12:00:00Z|not-a-uuid"),
		"longer than the limit": strings.Repeat("A", 300),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := domain.DecodeJoinRequestCursor(raw)
			require.ErrorIs(t, err, domain.ErrValidation)

			var validation *domain.ValidationError
			require.ErrorAs(t, err, &validation)
			require.Equal(t, "cursor", validation.Fields[0].Field)
		})
	}
}

// A discovery cursor and a queue cursor are different shapes, and neither may
// be decoded as the other: they order by different columns, so accepting one
// where the other belongs would page through the wrong sequence in silence.
func TestTheTwoCursorTypesAreNotInterchangeable(t *testing.T) {
	discovery := domain.Cursor{StartAt: time.Now().UTC(), ID: uuid.New()}
	_, err := domain.DecodeJoinRequestCursor(discovery.Encode())
	require.Error(t, err)

	queue := domain.JoinRequestCursor{CreatedAt: time.Now().UTC(), ID: uuid.New()}
	_, err = domain.DecodeCursor(queue.Encode())
	require.Error(t, err)
}

func TestInviteExpiresAtIsDerived(t *testing.T) {
	created := time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC)
	invite := domain.Invite{CreatedAt: created}
	require.Equal(t, created.Add(domain.InviteTTL), invite.ExpiresAt())
	require.Equal(t, 7*24*time.Hour, domain.InviteTTL)
}

func TestJoinRequestQueryNormalize(t *testing.T) {
	require.Equal(t, domain.JoinRequestLimitDefault,
		domain.JoinRequestQuery{}.Normalize().Limit)
	require.Equal(t, domain.JoinRequestLimitMax,
		domain.JoinRequestQuery{Limit: domain.JoinRequestLimitMax + 500}.Normalize().Limit)
	require.Equal(t, 5, domain.JoinRequestQuery{Limit: 5}.Normalize().Limit)
}

// encode builds a cursor payload directly, so a malformed one can be
// constructed without going through Encode — which by construction cannot
// produce any of them.
func encode(raw string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}
