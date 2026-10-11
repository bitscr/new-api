package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/require"
)

func autoModelTestReset() {
	autoModelHealth.Clear()
	autoModelChannelHealth.Clear()
	autoModelCooldowns.Clear()
	_ = operation_setting.SetAutoModelCandidates("{}")
	_ = operation_setting.SetAutoModelWeights("{}")
}

func TestAutoModelIsAutoModelName(t *testing.T) {
	require.True(t, operation_setting.IsAutoModelName("auto"))
	require.True(t, operation_setting.IsAutoModelName("  auto  "))
	require.False(t, operation_setting.IsAutoModelName("auto-v1"))
	require.False(t, operation_setting.IsAutoModelName("gpt-4o"))
	require.False(t, operation_setting.IsAutoModelName(""))
	require.False(t, operation_setting.IsAutoModelName("Auto"))
}

func TestAutoModelSetCandidatesParsing(t *testing.T) {
	autoModelTestReset()
	require.NoError(t, operation_setting.SetAutoModelCandidates(`{"default":["gpt-4o"," gpt-4o ","", "claude-3-5-sonnet"],"vip":["gpt-4o"]}`))
	candidates := operation_setting.GetAutoModelCandidates()
	defaults, ok := candidates["default"]
	require.True(t, ok)
	require.Equal(t, 2, len(defaults))
	require.Equal(t, "gpt-4o", defaults[0])
	require.Equal(t, "claude-3-5-sonnet", defaults[1])
	vip, ok := candidates["vip"]
	require.True(t, ok)
	require.Equal(t, 1, len(vip))
	// 空串清空
	require.NoError(t, operation_setting.SetAutoModelCandidates(""))
	require.Zero(t, len(operation_setting.GetAutoModelCandidates()))
	// 非 JSON 报错
	require.Error(t, operation_setting.SetAutoModelCandidates("not-json"))
}

func TestAutoModelHealthRanking(t *testing.T) {
	autoModelTestReset()
	group := "default"
	// 无观测：分数全部相等（中性 0.5），档内随机，而不是按输入/数据库顺序。
	// 输入顺序与质量无关，谁排前面纯看数据库返回顺序——这正是"第一次总是渠道 1
	// 那个排队上游"的来源。
	input := []string{"model-b", "model-a", "model-c"}
	firstSeen := map[string]bool{}
	for i := 0; i < 40; i++ {
		ranked := rankAutoModelTestCandidates(group, input)
		require.ElementsMatch(t, input, ranked)
		firstSeen[ranked[0]] = true
	}
	require.Greater(t, len(firstSeen), 1, "并列分数应随机挑，不能固定输入顺序的第一个")

	// model-a 成功 3 次；model-b 失败 3 次 → model-a 应排前
	// 使用同一渠道 ID（1）来模拟单渠道场景
	RecordAutoModelOutcome(group, "model-a", 1, true, 800, 800)
	RecordAutoModelOutcome(group, "model-a", 1, true, 1200, 1200)
	RecordAutoModelOutcome(group, "model-a", 1, true, 1000, 1000)
	RecordAutoModelOutcome(group, "model-b", 1, false, 100, 100)
	RecordAutoModelOutcome(group, "model-b", 1, false, 100, 100)
	RecordAutoModelOutcome(group, "model-b", 1, false, 100, 100)
	ranked := rankAutoModelTestCandidates(group, []string{"model-a", "model-b"})
	require.Equal(t, "model-a", ranked[0])
	require.Equal(t, "model-b", ranked[1])

	// 成功率相同时（都是 3 次成功），高延迟模型应靠后
	RecordAutoModelOutcome(group, "model-c", 1, true, 20000, 20000)
	RecordAutoModelOutcome(group, "model-c", 1, true, 20000, 20000)
	RecordAutoModelOutcome(group, "model-c", 1, true, 20000, 20000)
	ranked = rankAutoModelTestCandidates(group, []string{"model-a", "model-c"})
	require.Equal(t, "model-a", ranked[0])
	require.Equal(t, "model-c", ranked[1])
}

// TestAutoModelSlowSuccessWithoutLatencyOutranksColdCandidate 锁定故障成因：
// 一次"成功但很慢"的请求如果没有延迟数据，它的分数会高于从未观测过的候选，
// 于是排在最前面的慢渠道（例如排队 60 秒才服务的上游）会一直压住新候选，
// auto 每次请求都再选它，客户端等不到响应就报错。
// 修复点在调用侧（controller 用总耗时兜底），这里把推论钉住，
// 防止有人再把 latencyMs 传 0。
func TestAutoModelSlowSuccessWithoutLatencyOutranksColdCandidate(t *testing.T) {
	autoModelTestReset()
	group := "default"

	RecordAutoModelOutcome(group, "slow-model", 1, true, 0, 0) // 无延迟数据的成功观测
	ranked := rankAutoModelTestCandidates(group, []string{"slow-model", "fresh-model"})
	require.Equal(t, "slow-model", ranked[0], "无延迟的成功观测会压过冷启动候选（这就是故障成因）")

	autoModelTestReset()
	RecordAutoModelOutcome(group, "slow-model", 1, true, 75000, 75000) // 记到了 75 秒
	ranked = rankAutoModelTestCandidates(group, []string{"slow-model", "fresh-model"})
	require.Equal(t, "fresh-model", ranked[0], "记录到真实延迟后应让位给未观测候选")
}

func TestAutoModelHealthIsGroupScoped(t *testing.T) {
	autoModelTestReset()
	RecordAutoModelOutcome("default", "model-a", 1, true, 100, 100)
	RecordAutoModelOutcome("vip", "model-a", 1, false, 100, 100)
	RecordAutoModelOutcome("vip", "model-a", 1, false, 100, 100)
	// default 组 model-a 高分行列前；vip 组 model-a 低分行列后
	rankedDefault := rankAutoModelTestCandidates("default", []string{"model-a", "model-b"})
	require.Equal(t, "model-a", rankedDefault[0])
	rankedVip := rankAutoModelTestCandidates("vip", []string{"model-a", "model-b"})
	require.Equal(t, "model-b", rankedVip[0])
}

func TestAutoModelChannelGranularity(t *testing.T) {
	autoModelTestReset()
	group := "default"

	// 同一模型在不同渠道上的表现不同：
	// - channel-1: 快速成功（期望被优先选择）
	// - channel-2: 慢速成功或失败（期望排名靠后）
	RecordAutoModelOutcome(group, "test-model", 1, true, 100, 100)
	RecordAutoModelOutcome(group, "test-model", 1, true, 150, 150)
	RecordAutoModelOutcome(group, "test-model", 1, true, 120, 120)

	// 同模型另一个渠道表现差
	RecordAutoModelOutcome(group, "test-model", 2, false, 100, 100)
	RecordAutoModelOutcome(group, "test-model", 2, false, 100, 100)
	RecordAutoModelOutcome(group, "test-model", 2, false, 100, 100)

	// 验证渠道 1 的健康分更高
	outcome1, ok1 := autoModelHealth.Get(autoModelHealthKey(group, "test-model", 1))
	require.True(t, ok1)
	require.Greater(t, outcome1.Score, 0.8) // EWMA 应该显著高于 0.5

	outcome2, ok2 := autoModelHealth.Get(autoModelHealthKey(group, "test-model", 2))
	require.True(t, ok2)
	require.Less(t, outcome2.Score, 0.3) // 全失败应该显著低于 0.5
}

func TestAutoModelLatencyEWMA(t *testing.T) {
	// 固定时间只验证 EWMA，避免真实调度及冷却持久化耗时触发证据衰减。
	now := time.Unix(1700000000, 0)
	outcome := updateAutoModelOutcome(autoModelOutcome{}, false, true, 100, now)
	outcome = updateAutoModelOutcome(outcome, true, true, 150, now)
	outcome = updateAutoModelOutcome(outcome, true, true, 120, now)
	// 100 -> 100*0.8+150*0.2=110 -> 110*0.8+120*0.2=112
	require.InDelta(t, 112, outcome.LatencyMS, 0.0001)
}

func TestAutoModelScoreAlphaEWMA(t *testing.T) {
	now := time.Unix(1700000000, 0)
	// alpha=0.3 => 0.5 + 0.3*(1-0.5) = 0.65
	outcome := updateAutoModelOutcome(autoModelOutcome{}, false, true, 500, now)
	require.InDelta(t, 0.65, outcome.Score, 0.0001)

	// 再来一次失败 => 0.65 + 0.3*(0-0.65) = 0.455
	outcome = updateAutoModelOutcome(outcome, true, false, 0, now)
	require.InDelta(t, 0.455, outcome.Score, 0.0001)
}

func TestAutoModelRecordIgnoresVirtualName(t *testing.T) {
	autoModelTestReset()
	RecordAutoModelOutcome("default", "auto", 1, true, 5, 5)
	require.Zero(t, autoModelHealth.Len())
	require.Zero(t, autoModelChannelHealth.Len())
}

// Only floating-point-equivalent scores tie. Actual score differences must
// retain their order even when the lower-scoring item has a much larger weight.
func TestAutoModelTieBandsOrdering(t *testing.T) {
	items := []autoModelScoreItem{
		{Index: 0, Score: 0.30, Weight: 1},
		{Index: 1, Score: 0.60, Weight: 1},
		{Index: 2, Score: 0.60 + 5e-10, Weight: 1},
		{Index: 3, Score: 0.59, Weight: 1e100},
	}
	heads := map[int]bool{}
	for _, draw := range []float64{0, 0.99} {
		ordered := orderAutoModelScoreBands(items, func() float64 { return draw })
		require.Len(t, ordered, 4)
		require.Contains(t, []int{1, 2}, ordered[0].Index)
		require.Contains(t, []int{1, 2}, ordered[1].Index)
		require.Equal(t, 3, ordered[2].Index, "0.59 is below 0.60, not a random tie")
		require.Equal(t, 0, ordered[3].Index)
		heads[ordered[0].Index] = true
	}
	require.Len(t, heads, 2, "floating-point-equivalent top scores use weighted randomness")
}
