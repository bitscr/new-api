package service

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func rankAutoModelTestCandidates(group string, candidates []string) []string {
	targets := make(map[string][]model.AutoModelRoutingTarget, len(candidates))
	for _, name := range candidates {
		targets[name] = []model.AutoModelRoutingTarget{{ChannelID: 1, Weight: 1}}
	}
	snapshot := autoModelRouteSnapshot{Targets: targets, ModelHealth: autoModelHealth.ReadAll(), Now: time.Now()}
	plan, _ := buildAutoModelRoutesFromSnapshot(group, candidates, snapshot, false, nil)
	return autoModelRouteModelNames(plan)
}

func TestAutoModelEvidenceDecaysCountsAndSamplesButNotLatencyValue(t *testing.T) {
	now := time.Unix(1700000000, 0)
	window := time.Duration(autoModelHalfLifeMs()) * time.Millisecond
	outcome := autoModelOutcome{
		SuccessCount: 20, FailureCount: 4,
		LastObservedAt:            now.Add(-window / 2),
		NonStreamLatencyMS:        75000,
		NonStreamLatencySamples:   20,
		NonStreamLatencyUpdatedAt: now.Add(-window / 2),
	}
	decayed := decayAutoModelOutcome(outcome, now)
	require.InDelta(t, 10, decayed.SuccessCount, 0.0001)
	require.InDelta(t, 2, decayed.FailureCount, 0.0001)
	require.InDelta(t, 10, decayed.NonStreamLatencySamples, 0.0001)
	require.Equal(t, 75000.0, decayed.NonStreamLatencyMS, "latency value must not decay to zero")
}

func TestAutoModelEvidenceViewRequiresFreshSamples(t *testing.T) {
	now := time.Unix(1700000000, 0)
	outcome := autoModelOutcome{
		SuccessCount: 10, LastObservedAt: now,
		NonStreamLatencyMS: 500, NonStreamLatencySamples: 10, NonStreamLatencyUpdatedAt: now,
	}
	view := autoModelEvidenceViewFor(outcome, autoModelRequestNonStream, now)
	require.True(t, view.ReliabilityKnown)
	require.InDelta(t, 1.0, view.Reliability, 0.0001)
	require.True(t, view.LatencyKnown)
	require.InDelta(t, 500, view.LatencyMS, 0.0001)

	freshness := time.Duration(autoModelHalfLifeMs()) * time.Millisecond / 4
	stale := autoModelEvidenceViewFor(outcome, autoModelRequestNonStream, now.Add(freshness+time.Minute))
	require.False(t, stale.ReliabilityKnown)
	require.False(t, stale.LatencyKnown)
}

func TestAutoModelNonStreamFeedbackDoesNotReDecayStreamSamples(t *testing.T) {
	start := time.Unix(1700000000, 0)
	outcome := autoModelOutcome{
		SuccessCount: 10, LastObservedAt: start,
		StreamLatencyMS: 500, StreamLatencySamples: 10, StreamLatencyUpdatedAt: start,
	}
	// Seven non-stream observations over the next 60 minutes must leave the
	// stream evidence untouched; only the read-time view folds its age once.
	at := start
	for i := 0; i < 7; i++ {
		at = at.Add(60 * time.Minute / 7)
		outcome = updateAutoModelOutcome(outcome, true, true, false, true, 300, at)
	}
	require.InDelta(t, 10, outcome.StreamLatencySamples, 1e-9,
		"non-stream feedback must not repeatedly decay stream samples")
	require.Equal(t, start, outcome.StreamLatencyUpdatedAt)
	require.Greater(t, outcome.NonStreamLatencySamples, 6.0,
		"the active mode still folds its own history on each sample")

	view := autoModelEvidenceViewFor(outcome, autoModelRequestStream, at)
	factor := 1 - float64(at.Sub(start).Milliseconds())/float64(autoModelHalfLifeMs())
	require.InDelta(t, 10*factor, view.LatencySamples, 1e-6,
		"read-time decay must apply the elapsed age exactly once")
}

func TestAutoModelOutOfOrderObservationDoesNotRewindEvidenceTime(t *testing.T) {
	latest := time.Unix(1700000000, 0)
	outcome := autoModelOutcome{
		SuccessCount: 5, LastObservedAt: latest,
		NonStreamLatencyMS: 400, NonStreamLatencySamples: 3, NonStreamLatencyUpdatedAt: latest,
	}
	older := latest.Add(-10 * time.Minute)
	outcome = updateAutoModelOutcome(outcome, true, true, false, true, 900, older)
	require.Equal(t, latest, outcome.LastObservedAt, "a delayed older observation must not move time backwards")
	require.Equal(t, latest, outcome.NonStreamLatencyUpdatedAt)
	require.InDelta(t, 500, outcome.NonStreamLatencyMS, 1e-9,
		"the sample still folds into the EWMA without claiming a newer timestamp")
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
				return updateAutoModelOutcome(current, exists, true, false, true, 100, now)
			})
		}()
	}
	wg.Wait()
	outcome, ok := health.Get("key")
	require.True(t, ok)
	require.Equal(t, 12.0, outcome.SuccessCount)
}

func TestAutoModelRankingExcludesCoolingRoutesWithoutFallback(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Mode: autoModelRequestNonStream,
		Now:  now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"m":     {{ChannelID: 1, Priority: 5, Weight: 1}, {ChannelID: 2, Priority: 1, Weight: 1}},
			"fresh": {{ChannelID: 3, Weight: 1}},
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "m", 1): {Until: now.Add(time.Minute)},
		},
	}
	got, _ := buildAutoModelRoutesFromSnapshot("g", []string{"m", "fresh"}, snapshot, false, nil)
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "m", ChannelID: 2},
		{Group: "g", ModelName: "fresh", ChannelID: 3},
	}, got, "only the actually available channel supplies its priority")
	snapshot.Cooldowns[autoModelHealthKey("g", "m", 2)] = autoModelCooldownState{Until: now.Add(time.Minute)}
	got, _ = buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, false, nil)
	require.Empty(t, got, "a sole authorized but fully cooling model must not fail open")
	got, _ = buildAutoModelRoutesFromSnapshot("g", []string{"missing"}, snapshot, false, nil)
	require.Empty(t, got, "historical evidence and missing target snapshots cannot create routes")
}
