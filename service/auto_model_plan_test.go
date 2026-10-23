package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

func autoModelPlanTestOutcome(success, failure float64, latency float64, samples float64, now time.Time) autoModelOutcome {
	outcome := autoModelOutcome{
		SuccessCount: success, FailureCount: failure,
		LastObservedAt:     now,
		NonStreamLatencyMS: latency, NonStreamLatencySamples: samples,
		NonStreamLatencyUpdatedAt: now,
	}
	return outcome
}

func TestAutoModelPlanReliabilityGateAndLatencyPools(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream,
		Now:  now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"fast":    {{ChannelID: 1, Weight: 1}},
			"close":   {{ChannelID: 2, Weight: 1}},
			"slow":    {{ChannelID: 3, Weight: 1}},
			"flaky":   {{ChannelID: 4, Weight: 1}},
			"pending": {{ChannelID: 5, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "fast", 1):  autoModelPlanTestOutcome(20, 0, 500, 20, now),
			autoModelHealthKey("g", "close", 2): autoModelPlanTestOutcome(20, 0, 600, 20, now),
			autoModelHealthKey("g", "slow", 3):  autoModelPlanTestOutcome(20, 0, 1500, 20, now),
			autoModelHealthKey("g", "flaky", 4): autoModelPlanTestOutcome(16, 4, 400, 20, now),
			autoModelHealthKey("g", "pending", 5): func() autoModelOutcome {
				outcome := autoModelPlanTestOutcome(20, 0, 450, 20, now)
				outcome.PendingRecovery = true
				return outcome
			}(),
		},
	}
	// 500ms 锚点 + max(250ms, 20%*500=100ms) => 900ms 内同池。1.5s 和不可靠/待恢复
	// 路线都不能进入主流量池。
	routes, meta := buildAutoModelRoutesFromSnapshot("g",
		[]string{"fast", "close", "slow", "flaky", "pending"}, snapshot, false, func() float64 { return 0.99 })
	require.ElementsMatch(t, []string{"fast", "close"}, autoModelRouteModelNames(routes[:2]))
	require.Equal(t, "main", meta.Categories[AutoModelRoute{Group: "g", ModelName: "fast", ChannelID: 1}])
	require.Equal(t, "main", meta.Categories[AutoModelRoute{Group: "g", ModelName: "close", ChannelID: 2}])
	require.Equal(t, "backup", meta.Categories[AutoModelRoute{Group: "g", ModelName: "slow", ChannelID: 3}])
	require.True(t, meta.Trial[AutoModelRoute{Group: "g", ModelName: "flaky", ChannelID: 4}])
	require.True(t, meta.Trial[AutoModelRoute{Group: "g", ModelName: "pending", ChannelID: 5}])
	require.False(t, meta.Explored)
}

func TestAutoModelPlanExplorationPrependsOnlyOneRoute(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream,
		Now:  now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"main":   {{ChannelID: 1, Weight: 1}},
			"trial1": {{ChannelID: 2, Weight: 1}},
			"trial2": {{ChannelID: 3, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "main", 1): autoModelPlanTestOutcome(20, 0, 500, 20, now),
		},
	}
	// random()=0 forces the exploration draw to hit and the weighted sampler to
	// choose the first stable candidate.
	routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"main", "trial1", "trial2"}, snapshot, true, func() float64 { return 0 })
	require.True(t, meta.ExplorationDecided)
	require.True(t, meta.Explored)
	require.True(t, meta.ExplorationRatio > 0)
	require.Len(t, routes, 3)
	require.Equal(t, meta.ExplorationRoute, routes[0])
	require.True(t, meta.Trial[routes[0]], "exploration route is outside the main pool")
	require.Equal(t, "main", meta.Categories[routes[1]])
}

func TestAutoModelTrialSlotSingleInFlight(t *testing.T) {
	route := AutoModelRoute{Group: "g", ModelName: "m", ChannelID: 9}
	release, ok := AcquireAutoModelTrialSlot(route)
	require.True(t, ok)
	_, second := AcquireAutoModelTrialSlot(route)
	require.False(t, second, "only one trial request per route per process")
	release()
	_, third := AcquireAutoModelTrialSlot(route)
	require.True(t, third)
}
