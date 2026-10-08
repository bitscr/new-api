package service

import (
	"math"
	"math/rand"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func autoModelRouteTestOutcome(score float64, now time.Time) autoModelOutcome {
	return autoModelOutcome{Score: score, Observations: 20, UpdatedAt: now}
}

func TestAutoModelRoutePlanIsTrulyChannelFirst(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"a-high": {{ChannelID: 1, Priority: 7, Weight: 1}},
			"a-low":  {{ChannelID: 1, Priority: 7, Weight: 1}},
			"b-high": {{ChannelID: 2, Priority: 7, Weight: 10000}},
		},
		ChannelHealth: map[string]autoModelOutcome{
			autoModelChannelHealthKey("g", 1): autoModelRouteTestOutcome(0.9, now),
			autoModelChannelHealthKey("g", 2): autoModelRouteTestOutcome(0.7, now),
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "a-high", 1): autoModelRouteTestOutcome(0.9, now),
			autoModelHealthKey("g", "a-low", 1):  autoModelRouteTestOutcome(0.1, now),
			autoModelHealthKey("g", "b-high", 2): autoModelRouteTestOutcome(1, now),
		},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"b-high", "a-low", "a-high"}, snapshot, func() float64 { return 0.99 })
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "a-high", ChannelID: 1},
		{Group: "g", ModelName: "a-low", ChannelID: 1},
		{Group: "g", ModelName: "b-high", ChannelID: 2},
	}, got, "finish channel A's models before B even when B's model and weight are better")

	// A model cannot promote a channel: only the independent channel summary
	// changes the selected channel. No maximum/expected model score is involved.
	snapshot.ChannelHealth[autoModelChannelHealthKey("g", 1)] = autoModelRouteTestOutcome(0.1, now)
	got = buildAutoModelRoutesFromSnapshot("g", []string{"a-high", "a-low", "b-high"}, snapshot, nil)
	require.Equal(t, 2, got[0].ChannelID)
	require.Equal(t, "a-high", got[1].ModelName)
	require.Equal(t, "a-low", got[2].ModelName)
}

func TestAutoModelRoutesNeverMixChannelPriorities(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"m": {
				{ChannelID: 1, Priority: 5, Weight: 1},
				{ChannelID: 2, Priority: 5, Weight: 1e100},
				{ChannelID: 3, Priority: -1, Weight: 1e200},
			},
		},
		ChannelHealth: map[string]autoModelOutcome{
			autoModelChannelHealthKey("g", 1): autoModelRouteTestOutcome(0.4, now),
			autoModelChannelHealthKey("g", 2): autoModelRouteTestOutcome(0.1, now),
			autoModelChannelHealthKey("g", 3): autoModelRouteTestOutcome(1, now),
		},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, func() float64 { return 0.99 })
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "m", ChannelID: 1},
		{Group: "g", ModelName: "m", ChannelID: 2},
		{Group: "g", ModelName: "m", ChannelID: 3},
	}, got, "priority dominates score, and score dominates weight within a priority")
}

func TestAutoModelRouteWeightsAreLayerLocal(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"a": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 9}},
			"b": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 9}},
		},
		ModelWeights: map[string]map[string]float64{"g": {"a":1, "b":9}},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"a", "b"}, snapshot, func() float64 { return 0.5 })
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "b", ChannelID: 2},
		{Group: "g", ModelName: "a", ChannelID: 2},
		{Group: "g", ModelName: "b", ChannelID: 1},
		{Group: "g", ModelName: "a", ChannelID: 1},
	}, got)
	// Changing only model weights changes model order, never the chosen channel.
	snapshot.ModelWeights["g"] = map[string]float64{"a":9, "b":1}
	got = buildAutoModelRoutesFromSnapshot("g", []string{"a", "b"}, snapshot, func() float64 { return 0.5 })
	require.Equal(t, 2, got[0].ChannelID)
	require.Equal(t, "a", got[0].ModelName)
	require.Equal(t, "b", got[1].ModelName)
	// Changing a channel weight cannot change model scores/order in that channel.
	snapshot.Targets["a"][0].Weight, snapshot.Targets["b"][0].Weight = 100, 100
	got = buildAutoModelRoutesFromSnapshot("g", []string{"a", "b"}, snapshot, func() float64 { return 0.5 })
	require.Equal(t, 1, got[0].ChannelID)
	require.Equal(t, "a", got[0].ModelName)
}

func TestAutoModelRouteLowerModelScoreCannotWinWithMoreWeight(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"higher": {{ChannelID: 1, Weight: 1}},
			"lower": {{ChannelID: 1, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			// observations=3 yields effective scores exactly .60 and .59.
			autoModelHealthKey("g", "higher", 1): {Score: 0.70, Observations: 3, UpdatedAt: now},
			autoModelHealthKey("g", "lower", 1): {Score: 0.68, Observations: 3, UpdatedAt: now},
		},
		ModelWeights: map[string]map[string]float64{"g": {"higher":1, "lower":math.MaxFloat64}},
	}
	for _, draw := range []float64{0, 0.5, 0.99} {
		got := buildAutoModelRoutesFromSnapshot("g", []string{"lower", "higher"}, snapshot, func() float64 { return draw })
		require.Equal(t, []string{"higher", "lower"}, autoModelRouteModelNames(got))
	}
}

func TestAutoModelRouteLowerChannelScoreCannotWinWithMoreWeight(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"m": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: math.MaxFloat64}},
		},
		ChannelHealth: map[string]autoModelOutcome{
			autoModelChannelHealthKey("g", 1): {Score: 0.70, Observations: 3, UpdatedAt: now},
			autoModelChannelHealthKey("g", 2): {Score: 0.68, Observations: 3, UpdatedAt: now},
		},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, func() float64 { return 0.99 })
	require.Equal(t, 1, got[0].ChannelID, "channel score 0.60 beats 0.59 regardless of weight")
	require.Equal(t, 2, got[1].ChannelID)
}

func TestAutoModelRouteWeightsDefaultToOneAndZeroDisables(t *testing.T) {
	snapshot := autoModelRouteSnapshot{
		Now: time.Unix(1700000000, 0),
		Targets: map[string][]model.AutoModelRoutingTarget{
			"default": {{ChannelID: 1, Weight: 1}},
			"disabled": {{ChannelID: 2, Priority: 100, Weight: 100}},
		},
		ModelWeights: map[string]map[string]float64{"g": {"disabled":0}},
	}
	require.Equal(t, 1.0, autoModelRouteWeight("g", "default", snapshot.ModelWeights))
	require.Equal(t, 1.0, autoModelRouteWeight("other", "disabled", snapshot.ModelWeights))
	got := buildAutoModelRoutesFromSnapshot("g", []string{"default", "disabled"}, snapshot, nil)
	require.Equal(t, []AutoModelRoute{{Group: "g", ModelName: "default", ChannelID: 1}}, got)
	snapshot.ModelWeights["g"]["default"] = 0
	require.Empty(t, buildAutoModelRoutesFromSnapshot("g", []string{"default", "disabled"}, snapshot, nil))
	require.Len(t, buildAutoModelRoutesFromSnapshot("other", []string{"default", "disabled"}, snapshot, nil), 2)
}

func TestAutoModelRouteChannelZeroWeightSemantics(t *testing.T) {
	snapshot := autoModelRouteSnapshot{
		Now: time.Unix(1700000000, 0),
		Targets: map[string][]model.AutoModelRoutingTarget{"m": {{ChannelID: 1}, {ChannelID: 2}}},
	}
	for _, tc := range []struct{draw float64; want int}{{0,1},{0.99,2}} {
		got := buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, func() float64 { return tc.draw })
		require.Len(t, got, 2)
		require.Equal(t, tc.want, got[0].ChannelID, "all-zero channel weights sample uniformly")
	}
	snapshot.Targets["m"][1].Weight = 1
	got := buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, func() float64 { return 0 })
	require.Equal(t, 2, got[0].ChannelID, "zero weight cannot beat a positive weight in the same score band")
	require.Equal(t, 1, got[1].ChannelID, "the remaining all-zero channel band still forms a fallback tier")
}

func TestAutoModelRouteCooldownIsStrictAndTupleScoped(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"m": {{ChannelID: 1, Priority: 10, Weight: 1}, {ChannelID: 2, Weight: 1}},
			"other": {{ChannelID: 1, Priority: 10, Weight: 1}},
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "m", 1): {Until: now.Add(time.Minute)},
			autoModelHealthKey("vip", "m", 2): {Until: now.Add(time.Minute)},
			autoModelHealthKey("g", "other", 1): {Until: now},
		},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"m", "other"}, snapshot, nil)
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "other", ChannelID: 1},
		{Group: "g", ModelName: "m", ChannelID: 2},
	}, got, "a model cooldown does not cool sibling models or another group")
	snapshot.Cooldowns[autoModelHealthKey("g", "other", 1)] = autoModelCooldownState{Until: now.Add(time.Minute)}
	snapshot.Cooldowns[autoModelHealthKey("g", "m", 2)] = autoModelCooldownState{Until: now.Add(time.Minute)}
	require.Empty(t, buildAutoModelRoutesFromSnapshot("g", []string{"m", "other"}, snapshot, nil))
	snapshot.Cooldowns = nil
	snapshot.ExcludedChannels = map[int]bool{1:true, 2:true}
	require.Empty(t, buildAutoModelRoutesFromSnapshot("g", []string{"m", "other"}, snapshot, nil), "hard exclusions never fail open either")
}

func TestAutoModelRouteColdStartUsesWeightedDrawsWithoutReplacement(t *testing.T) {
	snapshot := autoModelRouteSnapshot{
		Now: time.Unix(1700000000, 0),
		Targets: map[string][]model.AutoModelRoutingTarget{
			"a": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 3}},
			"b": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 3}},
		},
		ModelWeights: map[string]map[string]float64{"g": {"a":1, "b":3}},
	}
	random := rand.New(rand.NewSource(7))
	channelHeads, modelHeads := map[int]int{}, map[string]int{}
	for i := 0; i < 1000; i++ {
		routes := buildAutoModelRoutesFromSnapshot("g", []string{"a", "b", "a"}, snapshot, random.Float64)
		require.Len(t, routes, 4)
		unique := make(map[AutoModelRoute]bool)
		for _, route := range routes { unique[route] = true }
		require.Len(t, unique, 4)
		require.Equal(t, routes[0].ChannelID, routes[1].ChannelID)
		require.Equal(t, routes[2].ChannelID, routes[3].ChannelID)
		channelHeads[routes[0].ChannelID]++
		modelHeads[routes[0].ModelName]++
	}
	require.InDelta(t, 750, channelHeads[2], 70)
	require.InDelta(t, 750, modelHeads["b"], 70)
}

func TestAutoModelWeightedDrawHandlesLargeFiniteWeights(t *testing.T) {
	items := []autoModelScoreItem{
		{Index:1, Score:0.5, Weight:math.MaxFloat64},
		{Index:2, Score:0.5, Weight:math.MaxFloat64},
	}
	for _, tc := range []struct{draw float64; want int}{{0.25,1},{0.75,2}} {
		got := orderAutoModelScoreBands(items, func() float64 { return tc.draw })
		require.Equal(t, tc.want, got[0].Index, "normalizing before summation prevents overflow")
	}
}

func TestAutoModelRouteContextCompatibilityAndOwnership(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "auto")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "auto")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "unchanged")
	common.SetContextKey(c, constant.ContextKeyChannelId, 99)
	require.Equal(t, -1, GetAutoModelRouteIndex(c))
	_, ok := GetCurrentAutoModelRoute(c)
	require.False(t, ok)
	input := []AutoModelRoute{
		{Group:"g", ModelName:"a", ChannelID:1},
		{Group:"g", ModelName:"b", ChannelID:1},
		{Group:"vip", ModelName:"a", ChannelID:2},
	}
	SetAutoModelRoutePlan(c, input)
	input[0].ChannelID = 900
	copyPlan := GetAutoModelRoutePlan(c)
	copyPlan[0].ModelName = "mutated"
	current, ok := GetCurrentAutoModelRoute(c)
	require.True(t, ok)
	require.Equal(t, AutoModelRoute{Group:"g", ModelName:"a", ChannelID:1}, current)
	require.Equal(t, []string{"a","b","a"}, common.GetContextKeyStringSlice(c, constant.ContextKeyAutoModelCandidates))
	require.Equal(t, 0, GetAutoModelRouteIndex(c))
	require.Equal(t, "g", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
	require.Equal(t, "g", common.GetContextKeyString(c, constant.ContextKeyAutoGroup))
	require.True(t, ActivateAutoModelRoute(c, 2))
	require.Equal(t, 2, common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex))
	require.Equal(t, "vip", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
	require.Equal(t, "vip", common.GetContextKeyString(c, constant.ContextKeyAutoGroup))
	require.Equal(t, "auto", common.GetContextKeyString(c, constant.ContextKeyTokenGroup))
	require.Equal(t, "unchanged", common.GetContextKeyString(c, constant.ContextKeyOriginalModel))
	require.Equal(t, 99, common.GetContextKeyInt(c, constant.ContextKeyChannelId))
	require.False(t, ActivateAutoModelRoute(c, -1))
	require.False(t, ActivateAutoModelRoute(c, 3))
	require.Equal(t, 2, GetAutoModelRouteIndex(c))
	SetAutoModelRoutePlan(c, nil)
	require.Equal(t, -1, GetAutoModelRouteIndex(c))
	require.Empty(t, common.GetContextKeyStringSlice(c, constant.ContextKeyAutoModelCandidates))
	require.Equal(t, "vip", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))
	_, ok = GetCurrentAutoModelRoute(c)
	require.False(t, ok)
	common.SetContextKey(c, constant.ContextKeyAutoModelIndex, 0)
	_, ok = GetCurrentAutoModelRoute(c)
	require.False(t, ok, "a stale legacy index must not reactivate an empty plan")
	SetAutoModelRoutePlan(nil, input)
	require.Nil(t, GetAutoModelRoutePlan(nil))
	require.Equal(t, -1, GetAutoModelRouteIndex(nil))
	require.False(t, ActivateAutoModelRoute(nil, 0))
}
