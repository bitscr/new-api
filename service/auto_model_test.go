package service

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/require"
)

func autoModelTestReset() {
	autoModelHealth.Clear()
	require.NoError(nil, operation_setting.SetAutoModelCandidates("{}"))
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
	// 无观测：冷启动保持输入顺序，评分相等
	ranked := rankAutoModelCandidates(group, []string{"model-b", "model-a", "model-c"})
	require.Equal(t, "model-b", ranked[0])
	require.Equal(t, "model-a", ranked[1])
	require.Equal(t, "model-c", ranked[2])

	// model-a 成功 3 次；model-b 失败 3 次 → model-a 应排前
	// 使用同一渠道 ID（1）来模拟单渠道场景
	RecordAutoModelOutcome(group, "model-a", 1, true, 800)
	RecordAutoModelOutcome(group, "model-a", 1, true, 1200)
	RecordAutoModelOutcome(group, "model-a", 1, true, 1000)
	RecordAutoModelOutcome(group, "model-b", 1, false, 100)
	RecordAutoModelOutcome(group, "model-b", 1, false, 100)
	RecordAutoModelOutcome(group, "model-b", 1, false, 100)
	ranked = rankAutoModelCandidates(group, []string{"model-a", "model-b"})
	require.Equal(t, "model-a", ranked[0])
	require.Equal(t, "model-b", ranked[1])

	// 成功率相同时（都是 3 次成功），高延迟模型应靠后
	RecordAutoModelOutcome(group, "model-c", 1, true, 20000)
	RecordAutoModelOutcome(group, "model-c", 1, true, 20000)
	RecordAutoModelOutcome(group, "model-c", 1, true, 20000)
	ranked = rankAutoModelCandidates(group, []string{"model-a", "model-c"})
	require.Equal(t, "model-a", ranked[0])
	require.Equal(t, "model-c", ranked[1])
}

func TestAutoModelHealthIsGroupScoped(t *testing.T) {
	autoModelTestReset()
	RecordAutoModelOutcome("default", "model-a", 1, true, 100)
	RecordAutoModelOutcome("vip", "model-a", 1, false, 100)
	RecordAutoModelOutcome("vip", "model-a", 1, false, 100)
	// default 组 model-a 高分行列前；vip 组 model-a 低分行列后
	rankedDefault := rankAutoModelCandidates("default", []string{"model-a", "model-b"})
	require.Equal(t, "model-a", rankedDefault[0])
	rankedVip := rankAutoModelCandidates("vip", []string{"model-a", "model-b"})
	require.Equal(t, "model-b", rankedVip[0])
}

func TestAutoModelChannelGranularity(t *testing.T) {
	autoModelTestReset()
	group := "default"

	// 同一模型在不同渠道上的表现不同：
	// - channel-1: 快速成功（期望被优先选择）
	// - channel-2: 慢速成功或失败（期望排名靠后）
	RecordAutoModelOutcome(group, "test-model", 1, true, 100)
	RecordAutoModelOutcome(group, "test-model", 1, true, 150)
	RecordAutoModelOutcome(group, "test-model", 1, true, 120)

	// 同模型另一个渠道表现差
	RecordAutoModelOutcome(group, "test-model", 2, false, 100)
	RecordAutoModelOutcome(group, "test-model", 2, false, 100)
	RecordAutoModelOutcome(group, "test-model", 2, false, 100)

	// 验证渠道 1 的健康分更高
	outcome1, ok1 := autoModelHealth.Get(autoModelHealthKey(group, "test-model", 1))
	require.True(t, ok1)
	require.Greater(t, outcome1.Score, 0.8) // EWMA 应该显著高于 0.5

	outcome2, ok2 := autoModelHealth.Get(autoModelHealthKey(group, "test-model", 2))
	require.True(t, ok2)
	require.Less(t, outcome2.Score, 0.3) // 全失败应该显著低于 0.5

	// 延迟同样是 EWMA（0.8/0.2）：100 -> 100*0.8+150*0.2=110 -> 110*0.8+120*0.2=112
	require.InDelta(t, 112, outcome1.LatencyMS, 0.0001)
}

func TestAutoModelScoreAlphaEWMA(t *testing.T) {
	autoModelTestReset()
	group := "default"

	// 初始 0.5
	RecordAutoModelOutcome(group, "m", 1, true, 500) // alpha=0.3 => 0.5 + 0.3*(1-0.5) = 0.65
	outcome, ok := autoModelHealth.Get(autoModelHealthKey(group, "m", 1))
	require.True(t, ok)
	require.InDelta(t, 0.65, outcome.Score, 0.0001)

	// 再来一次失败 => 0.65 + 0.3*(0-0.65) = 0.455
	RecordAutoModelOutcome(group, "m", 1, false, 0)
	outcome, ok = autoModelHealth.Get(autoModelHealthKey(group, "m", 1))
	require.True(t, ok)
	require.InDelta(t, 0.455, outcome.Score, 0.0001)
}

func TestAutoModelRecordIgnoresVirtualName(t *testing.T) {
	autoModelTestReset()
	RecordAutoModelOutcome("default", "auto", 1, true, 5)
	require.Zero(t, autoModelHealth.Len())
}
