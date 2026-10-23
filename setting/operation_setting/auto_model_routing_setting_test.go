package operation_setting

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutoModelRoutingDefaultsAndValidation(t *testing.T) {
	require.InDelta(t, 0.95, AutoModelReliabilityThreshold(), 1e-9)
	require.Equal(t, 5, AutoModelMinObservations())
	require.InDelta(t, 0.20, AutoModelLatencyToleranceRatio(), 1e-9)
	require.Equal(t, int64(250), AutoModelLatencyToleranceMs())
	require.InDelta(t, 0.05, AutoModelExploreRatio(), 1e-9)
	require.InDelta(t, 0.10, AutoModelExploreMaxRatio(), 1e-9)

	require.Empty(t, ValidateAutoModelReliabilityThreshold(0.9))
	require.NotEmpty(t, ValidateAutoModelReliabilityThreshold(1.1))
	require.NotEmpty(t, ValidateAutoModelMinObservations(0))
	require.Empty(t, ValidateAutoModelMinObservations(3))
	require.NotEmpty(t, ValidateAutoModelLatencyToleranceRatio(-0.1))
	require.NotEmpty(t, ValidateAutoModelLatencyToleranceMs(-1))
	require.Empty(t, ValidateAutoModelExploreRatios(0.05, 0.10))
	require.NotEmpty(t, ValidateAutoModelExploreRatios(0.10, 0.05))
	require.NotEmpty(t, ValidateAutoModelExploreRatios(0.05, 0.11))
}

func TestAutoModelMinObservationsUpperBoundIsReachable(t *testing.T) {
	require.Empty(t, ValidateAutoModelMinObservations(AutoModelMaxMinObservations))
	require.NotEmpty(t, ValidateAutoModelMinObservations(AutoModelMaxMinObservations+1))
	require.LessOrEqual(t, AutoModelMaxMinObservations, 70,
		"the accepted threshold must stay below the evidence cap after freshness decay")
}

func TestAutoModelExploreRatiosPublishAsOnePair(t *testing.T) {
	previous := AutoModelRoutingSettingsSnapshot()
	t.Cleanup(func() {
		require.Empty(t, SetAutoModelExploreRatios(previous.ExploreRatio, previous.ExploreMaxRatio))
	})

	require.Empty(t, SetAutoModelExploreRatios(0.02, 0.03))
	snapshot := AutoModelRoutingSettingsSnapshot()
	require.InDelta(t, 0.02, snapshot.ExploreRatio, 1e-9)
	require.InDelta(t, 0.03, snapshot.ExploreMaxRatio, 1e-9)

	require.NotEmpty(t, SetAutoModelExploreRatios(0.04, 0.01),
		"an invalid pair must be rejected as a whole")
	snapshot = AutoModelRoutingSettingsSnapshot()
	require.InDelta(t, 0.02, snapshot.ExploreRatio, 1e-9,
		"a rejected pair must not publish either half")
	require.InDelta(t, 0.03, snapshot.ExploreMaxRatio, 1e-9)
}

func TestAutoModelRoutingSettingsSnapshotIsComplete(t *testing.T) {
	snapshot := AutoModelRoutingSettingsSnapshot()
	require.Greater(t, snapshot.ScoreDecayMinutes, 0)
	require.NotZero(t, snapshot.MinObservations)
	require.NotZero(t, snapshot.LatencyToleranceMs)
}
