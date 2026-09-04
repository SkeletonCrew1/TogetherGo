package domain_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func uuidFromString(t *testing.T, raw string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(raw)
	require.NoError(t, err)
	return id
}
