package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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

func TestAutoModelPlanReusesFirstSuccessUntilMainAdmission(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	settings.MinObservations = 5
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream, Now: now, Settings: settings,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"successful": {{ChannelID: 99, Weight: 1}},
		},
		ModelHealth: make(map[string]autoModelOutcome),
	}
	names := []string{"successful"}
	for i := 1; i <= 40; i++ {
		name := fmt.Sprintf("unknown-%02d", i)
		names = append(names, name)
		snapshot.Targets[name] = []model.AutoModelRoutingTarget{{ChannelID: i, Weight: 1}}
	}
	route := AutoModelRoute{Group: "g", ModelName: "successful", ChannelID: 99}
	key := autoModelHealthKey(route.Group, route.ModelName, route.ChannelID)
	outcome := autoModelPlanTestOutcome(1, 0, 74049, 1, now)
	for attempt := 0; attempt < 7; attempt++ {
		snapshot.ModelHealth[key] = outcome
		routes, meta := buildAutoModelRoutesFromSnapshot("g", names, snapshot, true, func() float64 { return 0.5 })
		require.Equal(t, route, routes[0], "reuse the known working route even when its successful answer was slow")
		require.False(t, meta.ColdStart)
		require.False(t, meta.Explored)
		if attempt == 0 {
			require.Equal(t, "successful_trial", meta.Categories[route])
			require.True(t, meta.Trial[route], "one success does not bypass the single-flight trial slot")
			require.False(t, meta.Evidence[route].ReliabilityKnown)
			require.False(t, meta.Evidence[route].LatencyKnown)
		}
		if attempt == 6 {
			require.Equal(t, "main", meta.Categories[route], "reused business requests must eventually satisfy admission")
			require.False(t, meta.Trial[route])
		}
		snapshot.Now = snapshot.Now.Add(time.Second)
		outcome = updateAutoModelOutcome(outcome, true, true, false, true, 74049, snapshot.Now)
	}
}

func TestAutoModelPlanSuccessfulTrialBoundsExploration(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	settings.MinObservations, settings.ExploreRatio, settings.ExploreMaxRatio = 5, 0.05, 0.10
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream, Now: now, Settings: settings,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"unknown": {{ChannelID: 1, Weight: 1}},
			"fast":    {{ChannelID: 2, Weight: 1}},
			"slow":    {{ChannelID: 3, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "fast", 2): autoModelPlanTestOutcome(1, 0, 500, 1, now),
			autoModelHealthKey("g", "slow", 3): autoModelPlanTestOutcome(1, 0, 74049, 1, now),
		},
	}
	fast := AutoModelRoute{Group: "g", ModelName: "fast", ChannelID: 2}
	explored := 0
	for i := 0; i < 1000; i++ {
		draw := (float64(i) + 0.5) / 1000
		routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"unknown", "fast", "slow"}, snapshot, true, func() float64 { return draw })
		require.Len(t, routes, 3)
		require.False(t, meta.ColdStart)
		require.InDelta(t, 0.10, meta.ExplorationRatio, 1e-9)
		if meta.Explored {
			explored++
			require.NotEqual(t, fast, routes[0])
			require.Equal(t, meta.ExplorationRoute, routes[0])
			require.Equal(t, fast, routes[1], "one exploration attempt keeps the working route as immediate fallback")
		} else {
			require.Equal(t, fast, routes[0])
			require.Equal(t, "slow", routes[1].ModelName, "slow success remains ahead of unverified fallback")
		}
	}
	require.Equal(t, 100, explored, "only the configured share may start outside the successful pool")
	settings.ExploreRatio, settings.ExploreMaxRatio = 0, 0
	snapshot.Settings = settings
	routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"unknown", "fast", "slow"}, snapshot, true, func() float64 { return 0 })
	require.False(t, meta.Explored)
	require.Equal(t, fast, routes[0])
}

func TestAutoModelPlanSuccessfulTrialNeedsFreshModeSpecificEvidence(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	settings.MinObservations, settings.ScoreDecayMinutes = 5, 60
	for _, scenario := range []struct {
		name    string
		outcome autoModelOutcome
	}{
		{"other request mode", autoModelOutcome{SuccessCount: 1, LastObservedAt: now,
			StreamLatencyMS: 500, StreamLatencySamples: 1, StreamLatencyUpdatedAt: now}},
		{"stale success", autoModelPlanTestOutcome(1, 0, 500, 1, now.Add(-16 * time.Minute))},
		{"stale latency", func() autoModelOutcome {
			outcome := autoModelPlanTestOutcome(1, 0, 500, 1, now)
			outcome.NonStreamLatencyUpdatedAt = now.Add(-16 * time.Minute)
			return outcome
		}()},
		{"recent failure", func() autoModelOutcome {
			outcome := autoModelPlanTestOutcome(1, 0, 500, 1, now)
			outcome.PendingRecovery = true
			return outcome
		}()},
		{"unreliable", autoModelPlanTestOutcome(1, 1, 500, 1, now)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			snapshot := autoModelRouteSnapshot{
				Mode: autoModelRequestNonStream, Now: now, Settings: settings,
				Targets: map[string][]model.AutoModelRoutingTarget{"observed": {{ChannelID: 1, Weight: 1}}},
				ModelHealth: map[string]autoModelOutcome{autoModelHealthKey("g", "observed", 1): scenario.outcome},
			}
			routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"observed"}, snapshot, true, func() float64 { return 0.5 })
			require.Len(t, routes, 1)
			require.True(t, meta.ColdStart)
			require.NotEqual(t, "successful_trial", meta.Categories[routes[0]])
		})
	}
}

func TestAutoModelPlanDiagnosticsSeparateExclusions(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream,
		Now:  now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"cooling":  {{ChannelID: 1, Weight: 1}},
			"excluded": {{ChannelID: 2, Weight: 1}},
			"valid":    {{ChannelID: 3, Weight: 1}},
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "cooling", 1): {Until: now.Add(time.Minute)},
		},
		ExcludedRoutes: map[AutoModelRoute]bool{
			{Group: "g", ModelName: "excluded", ChannelID: 2}: true,
		},
		Diagnostics: &AutoModelRouteDiagnostics{},
	}
	routes, meta := buildAutoModelRoutesFromSnapshot("g",
		[]string{"cooling", "excluded", "valid"}, snapshot, false, func() float64 { return 0.9 })
	require.Len(t, routes, 1)
	require.Equal(t, AutoModelRoute{Group: "g", ModelName: "valid", ChannelID: 3}, routes[0])
	require.Equal(t, 1, meta.Diagnostics.Cooling)
	require.Equal(t, 1, meta.Diagnostics.RouteExcluded)
	require.Equal(t, 1, meta.Diagnostics.Trial)
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
