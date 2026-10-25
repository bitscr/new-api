package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/require"
)

func TestAutoModelManualPrioritySurvivesExpiredFailureMemory(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	settings.ScoreDecayMinutes = 60
	manual := AutoModelRoute{Group: "g", ModelName: "manual", ChannelID: 1}
	backup := AutoModelRoute{Group: "g", ModelName: "backup", ChannelID: 2}
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream, Now: now, Settings: settings,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"manual": {{ChannelID: 1, Priority: 100, Weight: 1}},
			"backup": {{ChannelID: 2, Priority: 0, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "backup", 2): autoModelPlanTestOutcome(20, 0, 94272, 20, now),
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "manual", 1): {Until: now.Add(-time.Minute), Level: 4},
		},
	}
	for _, draw := range []float64{0, 0.5, 0.99} {
		routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"backup", "manual"}, snapshot, true, func() float64 { return draw })
		require.Equal(t, []AutoModelRoute{manual, backup}, routes,
			"expired failure memory may change selection within a tier, but cannot override the administrator's higher priority")
		require.Equal(t, "pending_recovery", meta.Categories[manual])
		require.False(t, meta.Explored)
	}
	// An active cooldown is an actual availability restriction, so the next
	// configured priority supplies the fallback until recovery is allowed.
	snapshot.Cooldowns[autoModelHealthKey("g", "manual", 1)] = autoModelCooldownState{
		Until: now.Add(time.Minute), Level: 5,
	}
	routes, _ := buildAutoModelRoutesFromSnapshot("g", []string{"backup", "manual"}, snapshot, true, func() float64 { return 0 })
	require.Equal(t, []AutoModelRoute{backup}, routes)
}

func TestAutoModelRecoveryMemoryIsScopedToRoutePair(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	settings.MinObservations = 5
	failed := AutoModelRoute{Group: "g", ModelName: "shared", ChannelID: 1}
	working := AutoModelRoute{Group: "g", ModelName: "shared", ChannelID: 2}
	sibling := AutoModelRoute{Group: "g", ModelName: "sibling", ChannelID: 1}
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream, Now: now, Settings: settings,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"shared":  {{ChannelID: 1, Weight: 1000}, {ChannelID: 2, Weight: 1}},
			"sibling": {{ChannelID: 1, Weight: 1000}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "shared", 2): autoModelPlanTestOutcome(1, 0, 74049, 1, now),
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "shared", 1): {Until: now.Add(-time.Minute), Level: 4},
		},
	}
	for _, draw := range []float64{0, 0.5, 0.99} {
		routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"shared", "sibling"}, snapshot, false, func() float64 { return draw })
		require.Equal(t, []AutoModelRoute{working, sibling, failed}, routes,
			"one upstream's failed model must not demote its working peer or the channel's other model")
		require.Equal(t, "successful_trial", meta.Categories[working])
		require.False(t, meta.Evidence[working].PendingRecovery)
		require.Equal(t, "unobserved", meta.Categories[sibling])
		require.False(t, meta.Evidence[sibling].PendingRecovery)
		require.True(t, meta.Evidence[failed].PendingRecovery)
	}
	otherRoutes, otherMeta := buildAutoModelRoutesFromSnapshot("other", []string{"shared", "sibling"}, snapshot, false, func() float64 { return 0 })
	require.Len(t, otherRoutes, 3)
	for _, route := range otherRoutes {
		require.False(t, otherMeta.Evidence[route].PendingRecovery, "failure memory also remains isolated by authorized group")
	}
}

func TestAutoModelOnlyRecoveryCandidatesRemainUsableWithoutExploration(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream, Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"a": {{ChannelID: 1, Weight: 1}},
			"b": {{ChannelID: 2, Weight: 1}},
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "a", 1): {Until: now.Add(-time.Minute), Level: 1},
			autoModelHealthKey("g", "b", 2): {Until: now.Add(-time.Minute), Level: 2},
		},
	}
	routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"a", "b"}, snapshot, true, func() float64 { return 0 })
	require.ElementsMatch(t, []AutoModelRoute{
		{Group: "g", ModelName: "a", ChannelID: 1},
		{Group: "g", ModelName: "b", ChannelID: 2},
	}, routes, "expired cooldowns retain a recovery path when no untouched or working route exists")
	require.True(t, meta.ColdStart)
	require.False(t, meta.Explored)
	for _, route := range routes {
		require.Equal(t, "pending_recovery", meta.Categories[route])
		require.True(t, meta.Trial[route], "recovery remains subject to the single-flight trial guard")
	}
}
