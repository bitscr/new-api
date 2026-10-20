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
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func autoModelRouteTestOutcome(score float64, now time.Time) autoModelOutcome {
	return autoModelOutcome{Score: score, Observations: 20, UpdatedAt: now}
}

// autoModelRouteChannelOrder 把计划里的渠道 ID 按首次出现顺序抽出来。
func autoModelRouteChannelOrder(routes []AutoModelRoute) []int {
	var order []int
	seen := make(map[int]bool, len(routes))
	for _, route := range routes {
		if seen[route.ChannelID] {
			continue
		}
		seen[route.ChannelID] = true
		order = append(order, route.ChannelID)
	}
	return order
}

// setAutoModelScoreBandWidthForTest 显式设定档宽并在结束时还原,测试不依赖
// 环境配置的巧合值。
func setAutoModelScoreBandWidthForTest(t *testing.T, width float64) {
	t.Helper()
	old := operation_setting.AutoModelScoreBandWidth()
	t.Cleanup(func() { operation_setting.SetAutoModelScoreBandWidth(old) })
	operation_setting.SetAutoModelScoreBandWidth(width)
}

// priority 是硬边界:高 priority 的 0.4 分候选排在低 priority 的 0.99 前面,
// 低优先级层的全部路线都排在仍有候选的高优先级层之后。
func TestAutoModelPriorityIsHardBoundary(t *testing.T) {
	setAutoModelScoreBandWidthForTest(t, 0.01)

	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"m":     {{ChannelID: 1, Priority: 10, Weight: 1}, {ChannelID: 2, Weight: 1}},
			"other": {{ChannelID: 1, Priority: 10, Weight: 1}, {ChannelID: 2, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "m", 1):     autoModelRouteTestOutcome(0.4, now),
			autoModelHealthKey("g", "m", 2):     autoModelRouteTestOutcome(0.99, now),
			autoModelHealthKey("g", "other", 1): autoModelRouteTestOutcome(0.98, now),
			autoModelHealthKey("g", "other", 2): autoModelRouteTestOutcome(0.97, now),
		},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"m", "other"}, snapshot, func() float64 { return 0.99 })
	// priority 10 层(other@1 0.98、m@1 0.4)整层排在 priority 0 层(m@2 0.99、
	// other@2 0.97)之前——0.99 的高分也越不过优先级硬边界。层内按模型分档,
	// 各档单条路线,顺序确定。
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "other", ChannelID: 1},
		{Group: "g", ModelName: "m", ChannelID: 1},
		{Group: "g", ModelName: "m", ChannelID: 2},
		{Group: "g", ModelName: "other", ChannelID: 2},
	}, got, "the whole priority-10 tier precedes the priority-0 tier even with a 0.99 model there")
}

// 同 priority 的模型分跨渠道比较:B 渠道上显著更高的模型必须领先;渠道健康分
// 不参与排序(快照里根本没有渠道层数据也能排)。
func TestAutoModelRoutesInterleaveAcrossChannelsByModelScore(t *testing.T) {
	setAutoModelScoreBandWidthForTest(t, 0.01)

	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"a-high": {{ChannelID: 1, Weight: 1}},
			"a-low":  {{ChannelID: 1, Weight: 1}},
			"b-high": {{ChannelID: 2, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "a-high", 1): autoModelRouteTestOutcome(0.894, now),
			autoModelHealthKey("g", "a-low", 1):  autoModelRouteTestOutcome(0.850, now),
			autoModelHealthKey("g", "b-high", 2): autoModelRouteTestOutcome(0.897, now),
		},
	}
	// 0.897 与 0.894 差 0.003 < 档宽:同档。同档内两级抽样,渠道等权;
	// 固定随机源下按稳定枚举顺序可复现:draw 0.99 选中渠道 2,先输出 b-high,
	// 再回到渠道 1 的 a-high(0.894),最后是更低一档的 a-low。
	got := buildAutoModelRoutesFromSnapshot("g", []string{"b-high", "a-low", "a-high"}, snapshot, func() float64 { return 0.99 })
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "b-high", ChannelID: 2},
		{Group: "g", ModelName: "a-high", ChannelID: 1},
		{Group: "g", ModelName: "a-low", ChannelID: 1},
	}, got, "equal-priority routes interleave across channels by model score")
}

// 分档锚定在"尚未分档路线里的最高分":有效分相近的三条同档,明显更低的另起一档;
// 非链式:不能因为相邻差值都小于档宽就串成一档。模型排列的输入顺序不得改变档位。
// 分数为原始观测分:有效分 = 0.5+(score-0.5)*置信度(observations 封顶 20),
// 边界值按缩放后的有效分选取(0.900/0.895/0.890 的有效分距锚点 <= 0.0087 同档,
// 0.850 的有效分距锚点 0.0435 另档)。
func TestAutoModelScoreBandsAnchorAtBandMaximum(t *testing.T) {
	setAutoModelScoreBandWidthForTest(t, 0.01)

	now := time.Unix(1700000000, 0)
	newSnapshot := func(names []string, scores map[string]float64) autoModelRouteSnapshot {
		targets := map[string][]model.AutoModelRoutingTarget{}
		health := map[string]autoModelOutcome{}
		for _, name := range names {
			targets[name] = []model.AutoModelRoutingTarget{{ChannelID: 1, Weight: 1}}
			health[autoModelHealthKey("g", name, 1)] = autoModelRouteTestOutcome(scores[name], now)
		}
		return autoModelRouteSnapshot{Now: now, Targets: targets, ModelHealth: health}
	}
	// 0.900/0.895/0.890 的有效分(0.848/0.843/0.839)同档,0.850 的有效分(0.804)
	// 距锚点 > 档宽:另起一档。单渠道同权重,档内顺序随机;能确定的是 m4 必须垫底。
	got := buildAutoModelRoutesFromSnapshot("g", []string{"m1", "m2", "m3", "m4"}, newSnapshot([]string{"m1", "m2", "m3", "m4"}, map[string]float64{"m1": 0.900, "m2": 0.895, "m3": 0.890, "m4": 0.850}), func() float64 { return 0.5 })
	require.Len(t, got, 4)
	require.Equal(t, "m4", got[3].ModelName, "0.850 is a separate band behind 0.900/0.895/0.890")
	require.ElementsMatch(t, []string{"m1", "m2", "m3"},
		[]string{got[0].ModelName, got[1].ModelName, got[2].ModelName})

	// 非链式:0.888 的有效分(0.837)距锚点 0.0104 > 档宽另起一档,但距相邻项
	// 0.895 的有效分(0.843)只差 0.006。若错误地锚定相邻项,三条会串成一档,
	// m3 不会垫底。
	got = buildAutoModelRoutesFromSnapshot("g", []string{"m1", "m2", "m3"},
		newSnapshot([]string{"m1", "m2", "m3"}, map[string]float64{"m1": 0.900, "m2": 0.895, "m3": 0.888}), func() float64 { return 0.5 })
	require.Len(t, got, 3)
	require.Equal(t, "m3", got[2].ModelName, "0.888 starts a new band; banding must not chain via the previous item")
	require.ElementsMatch(t, []string{"m1", "m2"}, []string{got[0].ModelName, got[1].ModelName})

	// 输入顺序不影响档位:候选倒序重排,结论一致。
	got = buildAutoModelRoutesFromSnapshot("g", []string{"m3", "m2", "m1", "m4"},
		newSnapshot([]string{"m4", "m3", "m2", "m1"}, map[string]float64{"m1": 0.900, "m2": 0.895, "m3": 0.890, "m4": 0.850}), func() float64 { return 0.5 })
	require.Len(t, got, 4)
	require.Equal(t, "m4", got[3].ModelName)
}

// 档宽端点:0 = 仅同分随机(浮点容差 1e-9);>= 1 = 同优先级全部同档;
// 非法负数在运行时回退默认值。
func TestAutoModelScoreBandWidthEndpoints(t *testing.T) {
	now := time.Unix(1700000000, 0)
	newSnapshot := func() autoModelRouteSnapshot {
		return autoModelRouteSnapshot{
			Now: now,
			Targets: map[string][]model.AutoModelRoutingTarget{
				"high": {{ChannelID: 1, Weight: 1}},
				"low":  {{ChannelID: 1, Weight: 1}},
			},
			ModelHealth: map[string]autoModelOutcome{
				autoModelHealthKey("g", "high", 1): autoModelRouteTestOutcome(0.99, now),
				autoModelHealthKey("g", "low", 1):  autoModelRouteTestOutcome(0.10, now),
			},
		}
	}

	// 档宽 0:不同分各自成档,低分也不能越档。
	setAutoModelScoreBandWidthForTest(t, 0)
	got := buildAutoModelRoutesFromSnapshot("g", []string{"high", "low"}, newSnapshot(), func() float64 { return 0.99 })
	require.Equal(t, []string{"high", "low"}, autoModelRouteModelNames(got),
		"width 0 keeps strict score order")

	// 档宽 >= 1:同优先级内全部同档,低分候选可以被随机抽到队首。
	setAutoModelScoreBandWidthForTest(t, 1)
	got = buildAutoModelRoutesFromSnapshot("g", []string{"high", "low"}, newSnapshot(), func() float64 { return 0.99 })
	require.Equal(t, []string{"low", "high"}, autoModelRouteModelNames(got),
		"width >= 1 puts everything into one band where weights decide")

	// 非法负数:运行时回退默认 0.01。
	setAutoModelScoreBandWidthForTest(t, -1)
	require.Equal(t, 0.01, operation_setting.AutoModelScoreBandWidth(), "a negative width falls back to the default")
	got = buildAutoModelRoutesFromSnapshot("g", []string{"high", "low"}, newSnapshot(), nil)
	require.Equal(t, "high", got[0].ModelName, "with the default width the 0.99 candidate leads its own band")
}

// 冷启动没有"未观测渠道强制进最好档"的例外:没有观测的路线按中立 0.5 参与分档。
// 全中性时 priority 决定顺序;同优先级里被证明的 0.9 领先中性的 0.5。
func TestAutoModelColdStartUsesNeutralScoreWithoutException(t *testing.T) {
	setAutoModelScoreBandWidthForTest(t, 0.01)

	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"fresh":  {{ChannelID: 1, Priority: 5, Weight: 1}},
			"proven": {{ChannelID: 2, Priority: 5, Weight: 1}},
			"idle":   {{ChannelID: 3, Priority: 9, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{
			autoModelHealthKey("g", "proven", 2): autoModelRouteTestOutcome(0.9, now),
		},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"fresh", "proven", "idle"}, snapshot, func() float64 { return 0.99 })
	// idle(p9) 按优先级领先;p5 层里 proven(0.9) 领先中性的 fresh(0.5)。
	require.Equal(t, []int{3, 2, 1}, autoModelRouteChannelOrder(got),
		"an all-neutral cold start is ordered by priority; a proven 0.9 beats a neutral 0.5 at equal priority")
	require.Equal(t, "proven", got[1].ModelName)
	require.Equal(t, "fresh", got[2].ModelName)
}

// 档内抽样保留渠道份额:渠道权重相同时,A 渠道 10 个候选模型与 B 渠道 1 个
// 候选模型的总流量约各占 50%,模型数量不放大渠道份额;1:3 的渠道权重对应
// 25%/75%。
func TestAutoModelBandSamplingPreservesChannelShare(t *testing.T) {
	setAutoModelScoreBandWidthForTest(t, 1)

	now := time.Unix(1700000000, 0)
	newSnapshot := func(channelWeights [2]float64) autoModelRouteSnapshot {
		targets := map[string][]model.AutoModelRoutingTarget{}
		for i := 0; i < 10; i++ {
			name := "a" + string(rune('0'+i))
			targets[name] = []model.AutoModelRoutingTarget{{ChannelID: 1, Weight: channelWeights[0]}}
		}
		targets["solo"] = []model.AutoModelRoutingTarget{{ChannelID: 2, Weight: channelWeights[1]}}
		return autoModelRouteSnapshot{Now: now, Targets: targets}
	}

	random := rand.New(rand.NewSource(42))
	channelHeads, modelHeads := map[int]int{}, map[string]int{}
	const runs = 4000
	for i := 0; i < runs; i++ {
		routes := buildAutoModelRoutesFromSnapshot("g",
			[]string{"solo", "a0", "a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9"},
			newSnapshot([2]float64{1, 1}), random.Float64)
		require.Len(t, routes, 11)
		unique := make(map[AutoModelRoute]bool, len(routes))
		for _, route := range routes {
			unique[route] = true
		}
		require.Len(t, unique, 11, "sampling is without replacement")
		channelHeads[routes[0].ChannelID]++
		modelHeads[routes[0].ModelName]++
	}
	require.InDelta(t, runs/2, channelHeads[1], runs*0.04, "equal channel weights share the band ~50/50 regardless of model count")
	require.InDelta(t, runs/2, channelHeads[2], runs*0.04)
	require.InDelta(t, runs/2, modelHeads["solo"], runs*0.04, "the single model on its channel gets ~50%")
	for i := 0; i < 10; i++ {
		require.InDelta(t, runs*0.05, modelHeads["a"+string(rune('0'+i))], runs*0.03,
			"each of the ten equal models gets ~5%")
	}

	// 渠道权重 1:3 → 25%/75%。
	random = rand.New(rand.NewSource(43))
	channelHeads = map[int]int{}
	for i := 0; i < runs; i++ {
		routes := buildAutoModelRoutesFromSnapshot("g", []string{"a0", "solo"},
			autoModelRouteSnapshot{Now: now, Targets: map[string][]model.AutoModelRoutingTarget{
				"a0":   {{ChannelID: 1, Weight: 1}},
				"solo": {{ChannelID: 2, Weight: 3}},
			}}, random.Float64)
		channelHeads[routes[0].ChannelID]++
	}
	require.InDelta(t, runs*0.25, channelHeads[1], runs*0.04, "channel weight 1:3 means 25%/75% heads")
	require.InDelta(t, runs*0.75, channelHeads[2], runs*0.04)
}

// 零权重与大权重的边界:渠道零权重在正权重渠道耗尽前不参与,剩余全零等概率;
// 极大有限权重归一后求和,不会溢出。
func TestAutoModelBandSamplingWeightEdges(t *testing.T) {
	setAutoModelScoreBandWidthForTest(t, 1)

	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"zero": {{ChannelID: 1, Weight: 0}},
			"solo": {{ChannelID: 2, Weight: 1}},
		},
	}
	got := buildAutoModelRoutesFromSnapshot("g", []string{"zero", "solo"}, snapshot, func() float64 { return 0 })
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "solo", ChannelID: 2},
		{Group: "g", ModelName: "zero", ChannelID: 1},
	}, got, "a zero-weight channel stays as the in-band fallback behind positive ones")

	items := []autoModelWeightedItem{
		{Index: 0, Weight: math.MaxFloat64},
		{Index: 1, Weight: math.MaxFloat64},
	}
	require.Equal(t, 0, chooseAutoModelWeightedItem(items, func() float64 { return 0.25 }))
	require.Equal(t, 1, chooseAutoModelWeightedItem(items, func() float64 { return 0.75 }))
}

// 档宽 0 时分数严格决定档位,任何权重都不能跨档;档宽把两条路线并入同档后,
// 权重才决定先后。
func TestAutoModelBandEdgeDecidesWhetherWeightMatters(t *testing.T) {
	now := time.Unix(1700000000, 0)
	newSnapshot := func() autoModelRouteSnapshot {
		return autoModelRouteSnapshot{
			Now: now,
			Targets: map[string][]model.AutoModelRoutingTarget{
				"higher": {{ChannelID: 1, Weight: 1}},
				"lower":  {{ChannelID: 1, Weight: 1}},
			},
			ModelHealth: map[string]autoModelOutcome{
				// observations=3 yields effective scores exactly .60 and .59.
				autoModelHealthKey("g", "higher", 1): {Score: 0.70, Observations: 3, UpdatedAt: now},
				autoModelHealthKey("g", "lower", 1):  {Score: 0.68, Observations: 3, UpdatedAt: now},
			},
			ModelWeights: map[string]map[string]float64{"g": {"higher": 1, "lower": math.MaxFloat64}},
		}
	}

	setAutoModelScoreBandWidthForTest(t, 0)
	for _, draw := range []float64{0, 0.5, 0.99} {
		got := buildAutoModelRoutesFromSnapshot("g", []string{"lower", "higher"}, newSnapshot(), func() float64 { return draw })
		require.Equal(t, []string{"higher", "lower"}, autoModelRouteModelNames(got),
			"width 0: a 0.60/0.59 difference is a real band edge, weight cannot cross it")
	}

	setAutoModelScoreBandWidthForTest(t, 0.01)
	got := buildAutoModelRoutesFromSnapshot("g", []string{"lower", "higher"}, newSnapshot(), func() float64 { return 0.99 })
	require.Equal(t, []string{"lower", "higher"}, autoModelRouteModelNames(got),
		"width 0.01 puts 0.60/0.59 into one band where the huge weight wins the draw")
}

func TestAutoModelRouteWeightsDefaultToOneAndZeroDisables(t *testing.T) {
	snapshot := autoModelRouteSnapshot{
		Now: time.Unix(1700000000, 0),
		Targets: map[string][]model.AutoModelRoutingTarget{
			"default":  {{ChannelID: 1, Weight: 1}},
			"disabled": {{ChannelID: 2, Priority: 100, Weight: 100}},
		},
		ModelWeights: map[string]map[string]float64{"g": {"disabled": 0}},
	}
	require.Equal(t, 1.0, autoModelRouteWeight("g", "default", snapshot.ModelWeights))
	require.Equal(t, 1.0, autoModelRouteWeight("other", "disabled", snapshot.ModelWeights))
	got := buildAutoModelRoutesFromSnapshot("g", []string{"default", "disabled"}, snapshot, nil)
	require.Equal(t, []AutoModelRoute{{Group: "g", ModelName: "default", ChannelID: 1}}, got)
	snapshot.ModelWeights["g"]["default"] = 0
	require.Empty(t, buildAutoModelRoutesFromSnapshot("g", []string{"default", "disabled"}, snapshot, nil))
	require.Len(t, buildAutoModelRoutesFromSnapshot("other", []string{"default", "disabled"}, snapshot, nil), 2)
}

// 全部渠道零权重时等概率;一旦有正权重渠道,零权重渠道只能做后备。
func TestAutoModelRouteChannelZeroWeightSemantics(t *testing.T) {
	snapshot := autoModelRouteSnapshot{
		Now:     time.Unix(1700000000, 0),
		Targets: map[string][]model.AutoModelRoutingTarget{"m": {{ChannelID: 1}, {ChannelID: 2}}},
	}
	for _, tc := range []struct {
		draw float64
		want int
	}{{0, 1}, {0.99, 2}} {
		got := buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, func() float64 { return tc.draw })
		require.Len(t, got, 2)
		require.Equal(t, tc.want, got[0].ChannelID, "all-zero channel weights sample uniformly")
	}
	snapshot.Targets["m"][1].Weight = 1
	got := buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, func() float64 { return 0 })
	require.Equal(t, 2, got[0].ChannelID, "zero weight cannot beat a positive weight in the same band")
	require.Equal(t, 1, got[1].ChannelID, "the remaining all-zero channel still forms a fallback tier")
}

func TestAutoModelRouteCooldownIsStrictAndTupleScoped(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"m":     {{ChannelID: 1, Priority: 10, Weight: 1}, {ChannelID: 2, Weight: 1}},
			"other": {{ChannelID: 1, Priority: 10, Weight: 1}},
		},
		Cooldowns: map[string]autoModelCooldownState{
			autoModelHealthKey("g", "m", 1):     {Until: now.Add(time.Minute)},
			autoModelHealthKey("vip", "m", 2):   {Until: now.Add(time.Minute)},
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
	snapshot.ExcludedChannels = map[int]bool{1: true, 2: true}
	require.Empty(t, buildAutoModelRoutesFromSnapshot("g", []string{"m", "other"}, snapshot, nil), "hard exclusions never fail open either")
}

// 冷启动全中性时,无放回的两级抽样决定顺序:渠道 1:3 权重对应约 25%/75% 的
// 队首份额,模型 1:3 亦然;计划仍覆盖全部路线且不重复。
func TestAutoModelRouteColdStartUsesWeightedDrawsWithoutReplacement(t *testing.T) {
	setAutoModelScoreBandWidthForTest(t, 1)

	snapshot := autoModelRouteSnapshot{
		Now: time.Unix(1700000000, 0),
		Targets: map[string][]model.AutoModelRoutingTarget{
			"a": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 3}},
			"b": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 3}},
		},
		ModelWeights: map[string]map[string]float64{"g": {"a": 1, "b": 3}},
	}
	random := rand.New(rand.NewSource(7))
	channelHeads, modelHeads := map[int]int{}, map[string]int{}
	for i := 0; i < 1000; i++ {
		routes := buildAutoModelRoutesFromSnapshot("g", []string{"a", "b", "a"}, snapshot, random.Float64)
		require.Len(t, routes, 4)
		unique := make(map[AutoModelRoute]bool)
		for _, route := range routes {
			unique[route] = true
		}
		require.Len(t, unique, 4)
		channelHeads[routes[0].ChannelID]++
		modelHeads[routes[0].ModelName]++
	}
	// 队首渠道 2 的概率 = 3/4;队首模型 b 的概率 = P(ch1)*3/4 + P(ch2)*3/4 = 3/4。
	require.InDelta(t, 750, channelHeads[2], 70)
	require.InDelta(t, 750, modelHeads["b"], 70)
}

func TestAutoModelWeightedDrawHandlesLargeFiniteWeights(t *testing.T) {
	items := []autoModelWeightedItem{
		{Index: 0, Weight: math.MaxFloat64},
		{Index: 1, Weight: math.MaxFloat64},
	}
	for _, tc := range []struct {
		draw float64
		want int
	}{{0.25, 0}, {0.75, 1}} {
		got := chooseAutoModelWeightedItem(items, func() float64 { return tc.draw })
		require.Equal(t, tc.want, got, "normalizing before summation prevents overflow")
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
		{Group: "g", ModelName: "a", ChannelID: 1},
		{Group: "g", ModelName: "b", ChannelID: 1},
		{Group: "vip", ModelName: "a", ChannelID: 2},
	}
	SetAutoModelRoutePlan(c, input)
	input[0].ChannelID = 900
	copyPlan := GetAutoModelRoutePlan(c)
	copyPlan[0].ModelName = "mutated"
	current, ok := GetCurrentAutoModelRoute(c)
	require.True(t, ok)
	require.Equal(t, AutoModelRoute{Group: "g", ModelName: "a", ChannelID: 1}, current)
	require.Equal(t, []string{"a", "b", "a"}, common.GetContextKeyStringSlice(c, constant.ContextKeyAutoModelCandidates))
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
