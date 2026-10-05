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
	RecordAutoModelOutcome(group, "model-a", true, 800)
	RecordAutoModelOutcome(group, "model-a", true, 1200)
	RecordAutoModelOutcome(group, "model-a", true, 1000)
	RecordAutoModelOutcome(group, "model-b", false, 100)
	RecordAutoModelOutcome(group, "model-b", false, 100)
	RecordAutoModelOutcome(group, "model-b", false, 100)
	ranked = rankAutoModelCandidates(group, []string{"model-a", "model-b"})
	require.Equal(t, "model-a", ranked[0])
	require.Equal(t, "model-b", ranked[1])

	// 成功率相同时（都是 3 次成功），高延迟模型应靠后
	RecordAutoModelOutcome(group, "model-c", true, 20000)
	RecordAutoModelOutcome(group, "model-c", true, 20000)
	RecordAutoModelOutcome(group, "model-c", true, 20000)
	ranked = rankAutoModelCandidates(group, []string{"model-a", "model-c"})
	require.Equal(t, "model-a", ranked[0])
	require.Equal(t, "model-c", ranked[1])
}

func TestAutoModelHealthIsGroupScoped(t *testing.T) {
	autoModelTestReset()
	RecordAutoModelOutcome("default", "model-a", true, 100)
	RecordAutoModelOutcome("vip", "model-a", false, 100)
	RecordAutoModelOutcome("vip", "model-a", false, 100)
	// default 组 model-a 高分行列前；vip 组 model-a 低分行列后
	rankedDefault := rankAutoModelCandidates("default", []string{"model-a", "model-b"})
	require.Equal(t, "model-a", rankedDefault[0])
	rankedVip := rankAutoModelCandidates("vip", []string{"model-a", "model-b"})
	require.Equal(t, "model-b", rankedVip[0])
}

func TestAutoModelRecordIgnoresVirtualName(t *testing.T) {
	autoModelTestReset()
	RecordAutoModelOutcome("default", "auto", true, 5)
	require.Zero(t, autoModelHealth.Len())
}