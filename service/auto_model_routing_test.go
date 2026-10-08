package service

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// Existing score tests explicitly model one enabled channel per candidate;
// they exercise ranking without querying DB or consulting production cooldowns.
func rankAutoModelTestCandidates(group string, candidates []string) []string {
	targets := make(map[string][]model.AutoModelRoutingTarget, len(candidates))
	for _, name := range candidates {
		targets[name] = []model.AutoModelRoutingTarget{{ChannelID: 1, Weight: 1}}
	}
	return rankAutoModelCandidatesFromSnapshot(group, candidates, targets, autoModelHealth.ReadAll(), nil, time.Now())
}

func TestAutoModelEvidenceExpiresAndDoesNotRevive(t *testing.T) {
	now := time.Unix(1700000000, 0)
	old := autoModelOutcome{Score: 1, Observations: 100000, LatencyMS: 75000, UpdatedAt: now.Add(-10 * time.Minute)}
	require.Equal(t, 0.5, effectiveScore(old, now))
	fresh := updateAutoModelOutcome(old, true, true, 100, now)
	require.InDelta(t, 0.65, fresh.Score, 0.000001)
	require.Equal(t, 1.0, fresh.Observations)
	require.Equal(t, 100.0, fresh.LatencyMS)
	half := old
	half.UpdatedAt = now.Add(-5 * time.Minute)
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

func TestAutoModelRoutingExpectationUsesCurrentTopTierAndWeights(t *testing.T) {
	now := time.Unix(1700000000, 0)
	health := map[string]autoModelOutcome{}
	// Effective scores are .60, .61 and .75 with observations=3.
	for id, score := range map[int]float64{1: 0.7, 2: 0.72, 3: 1, 99: 1} {
		health[autoModelHealthKey("g", "m", id)] = autoModelOutcome{Score: score, Observations: 3, UpdatedAt: now}
	}
	targets := []model.AutoModelRoutingTarget{
		{ChannelID: 1, Priority: 5, Weight: 1},
		{ChannelID: 2, Priority: 5, Weight: 3},
		{ChannelID: 3, Priority: 1, Weight: 1000},
	}
	// Disabled historical channel 99 and the lower priority channel are ignored.
	require.InDelta(t, 0.6075, autoModelRoutingExpectedScore("g", "m", targets, health, now), 0.000001)
	targets[0].Weight, targets[1].Weight = 0, 0
	require.InDelta(t, 0.605, autoModelRoutingExpectedScore("g", "m", targets, health, now), 0.000001)
	// A score outside the top band receives no weight, however large its weight.
	health[autoModelHealthKey("g", "m", 1)] = autoModelOutcome{Score: 0.1, Observations: 3, UpdatedAt: now}
	require.InDelta(t, 0.61, autoModelRoutingExpectedScore("g", "m", targets, health, now), 0.000001)
}

func TestAutoModelRankingExcludesCoolingRouteEvidence(t *testing.T) {
	now := time.Unix(1700000000, 0)
	targets := map[string][]model.AutoModelRoutingTarget{
		"m": {{ChannelID: 1, Priority: 5, Weight: 1}, {ChannelID: 2, Priority: 1, Weight: 1}},
		"fresh": {{ChannelID: 3, Weight: 1}},
	}
	health := map[string]autoModelOutcome{
		autoModelHealthKey("g", "m", 1): {Score: 1, Observations: 20, UpdatedAt: now},
		autoModelHealthKey("g", "m", 2): {Score: 0, Observations: 20, UpdatedAt: now},
	}
	cooldowns := map[string]autoModelCooldownState{
		autoModelHealthKey("g", "m", 1): {Until: now.Add(time.Minute)},
	}
	got := rankAutoModelCandidatesFromSnapshot("g", []string{"m", "fresh"}, targets, health, cooldowns, now)
	// 先挑渠道再挑模型：渠道优先级是第一关键字。m 的 ch1（优先级 5）整体冷却被跳过，
	// 但它剩下的可用渠道 ch2 优先级 1 仍高于 fresh 的 ch3（0），所以 m 排前。
	// 冷却证据依然被排除（ch1 的满分不进 m 的期望值），只是排序口径跟着渠道优先级走。
	require.Equal(t, []string{"m", "fresh"}, got)
	// A sole authorized candidate remains eligible under all-cooling fallback.
	cooldowns[autoModelHealthKey("g", "m", 2)] = autoModelCooldownState{Until: now.Add(time.Minute)}
	got = rankAutoModelCandidatesFromSnapshot("g", []string{"m"}, targets, health, cooldowns, now)
	require.Equal(t, []string{"m"}, got)
}
