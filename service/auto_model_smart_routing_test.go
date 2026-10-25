package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAutoModelManualPriorityRemainsHardBoundary(t *testing.T) {
	now := time.Now()
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream, Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"manual": {{ChannelID: 1, Priority: 100, Weight: 1}},
			"slow":   {{ChannelID: 2, Priority: 0, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "slow", 2): autoModelPlanTestOutcome(20, 0, 94272, 20, now),
		},
	}
	for _, draw := range []float64{0, 0.5, 0.99} {
		routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"slow", "manual"}, snapshot, true, func() float64 { return draw })
		require.Equal(t, "manual", routes[0].ModelName, "business evidence and exploration must not override manual priority")
		require.False(t, meta.Explored)
	}
	// An actual failure isolates only the failed pair, allowing the configured backup.
	snapshot.Cooldowns = map[string]autoModelCooldownState{
		autoModelHealthKey("g", "manual", 1): {Until: now.Add(time.Minute), Level: 1},
	}
	routes, _ := buildAutoModelRoutesFromSnapshot("g", []string{"slow", "manual"}, snapshot, false, nil)
	require.Len(t, routes, 1)
	require.Equal(t, "slow", routes[0].ModelName)
}

func TestAutoModelExpiredFailuresNeverWinExploration(t *testing.T) {
	now := time.Now()
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	settings.MinObservations, settings.ScoreDecayMinutes = 5, 60
	settings.ExploreRatio, settings.ExploreMaxRatio = 0.10, 0.10
	failed := autoModelPlanTestOutcome(0, 10, 0, 0, now.Add(-2*time.Hour))
	failed.PendingRecovery = true
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream, Now: now, Settings: settings,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"failed":  {{ChannelID: 1, Weight: 1000}},
			"unknown": {{ChannelID: 2, Weight: 1}},
			"working": {{ChannelID: 3, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "failed", 1):  failed,
			autoModelHealthKey("g", "working", 3): autoModelPlanTestOutcome(1, 0, 74049, 1, now),
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "failed", 1): {Until: now.Add(-time.Minute), Level: 4},
		},
	}
	for i := 0; i < 1000; i++ {
		draw := (float64(i) + 0.5) / 1000
		routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"failed", "unknown", "working"}, snapshot, true, func() float64 { return draw })
		require.Len(t, routes, 3)
		require.Equal(t, "failed", routes[2].ModelName)
		require.Equal(t, "pending_recovery", meta.Categories[routes[2]])
		require.Zero(t, meta.Evidence[routes[2]].Observations)
		if meta.Explored {
			require.Equal(t, "unknown", routes[0].ModelName)
			require.Equal(t, "working", routes[1].ModelName, "slow success is the immediate exploration fallback")
		} else {
			require.Equal(t, "working", routes[0].ModelName)
		}
	}
	// Score pruning/restart must not erase the cooldown row's failure memory.
	delete(snapshot.ModelHealth, autoModelHealthKey("g", "failed", 1))
	routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"failed", "unknown", "working"}, snapshot, true, func() float64 { return 0 })
	require.Equal(t, "failed", routes[2].ModelName)
	require.True(t, meta.Evidence[routes[2]].PendingRecovery)
	// All-cold selection tries untouched routes before known failures, without probes.
	delete(snapshot.Targets, "working")
	routes, meta = buildAutoModelRoutesFromSnapshot("g", []string{"failed", "unknown"}, snapshot, true, func() float64 { return 0 })
	require.Equal(t, "unknown", routes[0].ModelName)
	require.True(t, meta.ColdStart)
	require.False(t, meta.Explored)
}

func TestAutoModelExplorationTimeoutProtectsManualAndSlowRoutes(t *testing.T) {
	unknown := AutoModelRoute{Group: "g", ModelName: "unknown", ChannelID: 1}
	working := AutoModelRoute{Group: "g", ModelName: "slow", ChannelID: 2}
	for _, category := range []string{"main", "backup", "successful_trial"} {
		c, _ := gin.CreateTestContext(nil)
		SetAutoModelRoutePlan(c, []AutoModelRoute{unknown, working})
		meta := AutoModelRoutePlanMeta{
			Categories: map[AutoModelRoute]string{unknown: "unobserved", working: category},
			Explored:   true, ExplorationRoute: unknown,
		}
		SetAutoModelRoutePlanMeta(c, meta)
		require.Equal(t, 20*time.Second, AutoModelExplorationAttemptTimeout(c))
		meta.Explored = false
		SetAutoModelRoutePlanMeta(c, meta)
		require.Zero(t, AutoModelExplorationAttemptTimeout(c), "normal priority selection must retain its original timeout")
		meta.Explored = true
		meta.Categories[unknown] = category
		SetAutoModelRoutePlanMeta(c, meta)
		require.Zero(t, AutoModelExplorationAttemptTimeout(c), "proven slow exploration must not be cut off")
	}
}
