package operation_setting

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutoModelScoreBandWidthDefaultsAndSanitize(t *testing.T) {
	t.Cleanup(func() { InitAutoModelRoutingSettingsFromEnv() })
	InitAutoModelRoutingSettingsFromEnv()
	require.Equal(t, 0.01, AutoModelScoreBandWidth(), "默认档宽 0.01")

	SetAutoModelScoreBandWidth(0)
	require.Equal(t, 0.0, AutoModelScoreBandWidth(), "显式 0 是合法配置,按原值保留")
	SetAutoModelScoreBandWidth(1)
	require.Equal(t, 1.0, AutoModelScoreBandWidth(), ">= 1 表示同优先级全部同档")
	SetAutoModelScoreBandWidth(-0.5)
	require.Equal(t, 0.01, AutoModelScoreBandWidth(), "负数回退默认值")
	require.NotEmpty(t, ValidateAutoModelScoreBandWidth(-0.5))
	require.Empty(t, ValidateAutoModelScoreBandWidth(0))
}

// 环境变量只接受新键 AUTO_MODEL_SCORE_BAND_WIDTH;旧键
// AUTO_MODEL_PRIORITY_OVERRIDE_MARGIN 不再作为环境变量回退来源,
// 历史数据库值由 model/option.go 的一次性迁移处理。
func TestAutoModelScoreBandWidthEnvOnlyNewKey(t *testing.T) {
	t.Setenv("AUTO_MODEL_PRIORITY_OVERRIDE_MARGIN", "0.9")
	InitAutoModelRoutingSettingsFromEnv()
	require.Equal(t, 0.01, AutoModelScoreBandWidth(),
		"旧环境变量不再是回退来源,部署配置应改用新键")

	t.Setenv("AUTO_MODEL_SCORE_BAND_WIDTH", "0.05")
	InitAutoModelRoutingSettingsFromEnv()
	require.Equal(t, 0.05, AutoModelScoreBandWidth(), "新环境变量生效")

	t.Setenv("AUTO_MODEL_SCORE_BAND_WIDTH", "not-a-number")
	InitAutoModelRoutingSettingsFromEnv()
	require.Equal(t, 0.01, AutoModelScoreBandWidth(), "非法环境变量回退默认值")
}
