package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutoModelScoreSaveAcceptedAllRequiresMySQLReadback(t *testing.T) {
	require.False(t, autoModelScoreSaveAcceptedAll("mysql", 2, 2),
		"one updated row (2) plus one rejected row (0) also totals the batch size")
	require.False(t, autoModelScoreSaveAcceptedAll("mysql", 3, 2))
	require.True(t, autoModelScoreSaveAcceptedAll("sqlite", 2, 2))
	require.False(t, autoModelScoreSaveAcceptedAll("sqlite", 1, 2))
	require.True(t, autoModelScoreSaveAcceptedAll("postgres", 2, 2))
}
