package service

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// Existing model-score tests explicitly model one enabled shared channel. They
// exercise model ordering inside that channel without DB or production cooldowns.
func rankAutoModelTestCandidates(group string, candidates []string) []string {
	targets := make(map[string][]model.AutoModelRoutingTarget, len(candidates))
	for _, name := range candidates {
		targets[name] = []model.AutoModelRoutingTarget{{ChannelID: 1, Weight: 1}}
	}
	snapshot := autoModelRouteSnapshot{Targets: targets, ModelHealth: autoModelHealth.ReadAll(), Now: time.Now()}
	return autoModelRouteModelNames(buildAutoModelRoutesFromSnapshot(group, candidates, snapshot, nil))
}

func TestAutoModelEvidenceExpiresAndDoesNotRevive(t *testing.T) {
	now := time.Unix(1700000000, 0)
	// 衰减窗口是可配置的,所以断言必须跟着窗口走,不能写死 10 分钟:
	// 不变式是"超过窗口即完全中立、窗口一半处保留一半"。
	window := time.Duration(autoModelHalfLifeMs()) * time.Millisecond
	old := autoModelOutcome{Score: 1, Observations: 100000, LatencyMS: 75000, UpdatedAt: now.Add(-window)}
	require.Equal(t, 0.5, effectiveScore(old, now))
	fresh := updateAutoModelOutcome(old, true, true, 100, now)
	require.InDelta(t, 0.65, fresh.Score, 0.000001)
	require.Equal(t, 1.0, fresh.Observations)
	require.Equal(t, 100.0, fresh.LatencyMS)
	half := old
	half.UpdatedAt = now.Add(-window / 2)
	decayed := decayAutoModelOutcome(half, now)
	require.Equal(t, 10.0, decayed.Observations)
	require.Equal(t, 0.75, decayed.Score)
	require.Equal(t, 37500.0, decayed.LatencyMS)
	for i := 0; i < 100; i++ {
		fresh = updateAutoModelOutcome(fresh, true, true, 100, now)
	}
	require.Equal(t, autoModelMaxObservations, fresh.Observations)
}

func TestAutoModelAtomicEvidenceUpdates(t *testing.T) {
	health := types.NewRWMap[string, autoModelOutcome]()
	now := time.Unix(1700000000, 0)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			health.Update("key", func(current autoModelOutcome, exists bool) autoModelOutcome {
				return updateAutoModelOutcome(current, exists, true, 100, now)
			})
		}()
	}
	wg.Wait()
	outcome, ok := health.Get("key")
	require.True(t, ok)
	require.Equal(t, 12.0, outcome.Observations)
}

func TestAutoModelRankingExcludesCoolingRoutesWithoutFallback(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"m":     {{ChannelID: 1, Priority: 5, Weight: 1}, {ChannelID: 2, Priority: 1, Weight: 1}},
			"fresh": {{ChannelID: 3, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "m", 1): {Score: 1, Observations: 20, UpdatedAt: now},
			autoModelHealthKey("g", "m", 2): {Score: 0, Observations: 20, UpdatedAt: now},
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "m", 1): {Until: now.Add(time.Minute)},
		},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"m", "fresh"}, snapshot, nil)
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "m", ChannelID: 2},
		{Group: "g", ModelName: "fresh", ChannelID: 3},
	}, got, "only the actually available channel supplies its priority")
	snapshot.Cooldowns[autoModelHealthKey("g", "m", 2)] = autoModelCooldownState{Until: now.Add(time.Minute)}
	got = buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, nil)
	require.Empty(t, got, "a sole authorized but fully cooling model must not fail open")
	got = buildAutoModelRoutesFromSnapshot("g", []string{"missing"}, snapshot, nil)
	require.Empty(t, got, "historical evidence and missing target snapshots cannot create routes")
}
