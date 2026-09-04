package domain_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/togethergo/chat/internal/domain"
)

func TestCleanBody(t *testing.T) {
	t.Run("trims before measuring", func(t *testing.T) {
		body, err := domain.CleanBody("  we leave at six  ")
		require.NoError(t, err)
		require.Equal(t, "we leave at six", body)
	})

	t.Run("rejects empty and whitespace-only", func(t *testing.T) {
		for _, raw := range []string{"", "   ", "\t\n", " "} {
			_, err := domain.CleanBody(raw)
			require.Error(t, err, "expected %q to be rejected", raw)
		}
	})

	t.Run("counts characters, not bytes", func(t *testing.T) {
		// Two thousand Cyrillic characters is four thousand bytes. A byte limit
		// would let an English speaker write twice as much as a Ukrainian one,
		// which is not a rule anybody would write down on purpose.
		body := strings.Repeat("ї", domain.MaxBodyLength)
		cleaned, err := domain.CleanBody(body)
		require.NoError(t, err)
		require.Equal(t, body, cleaned)

		_, err = domain.CleanBody(strings.Repeat("ї", domain.MaxBodyLength+1))
		require.Error(t, err)
	})
}

func TestCleanLimit(t *testing.T) {
	// Absent and zero mean the same thing — "give me a page" — because every
	// client that omits the parameter and every client that sends an empty one
	// mean that, and answering one of them with an empty page would be a puzzle.
	limit, err := domain.CleanLimit(0)
	require.NoError(t, err)
	require.Equal(t, domain.DefaultPageLimit, limit)

	limit, err = domain.CleanLimit(25)
	require.NoError(t, err)
	require.Equal(t, 25, limit)

	_, err = domain.CleanLimit(-1)
	require.Error(t, err)

	_, err = domain.CleanLimit(domain.MaxPageLimit + 1)
	require.Error(t, err)
}

func TestRoomIsOpen(t *testing.T) {
	require.True(t, domain.Room{Status: domain.StatusRecruiting}.IsOpen())
	require.False(t, domain.Room{Status: domain.StatusCancelled}.IsOpen())
}
