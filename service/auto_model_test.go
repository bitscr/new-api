package service

import (
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/require"
)

func autoModelTestReset() {
	autoModelHealth.Clear()
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
