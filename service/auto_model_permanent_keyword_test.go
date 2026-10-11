package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// 管理员补的关键词必须能把"内置判据认不出的确定性失败"升级为长冷却。
// 日志里的真实样本:`tools / function calling is not supported by the upstream anonymous backend`。
func TestPermanentKeywordUpgradesUnknownFailure(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("function calling is not supported")

	err := types.NewErrorWithStatusCode(
		errors.New("tools / function calling is not supported by the upstream anonymous backend; remove `tools`"),
		types.ErrorCode("invalid_request_error"), http.StatusBadRequest)

	require.True(t, AutoModelPermanentFailure(err),
		"关键词命中就该升级为永久级,否则这种失败只会拿 15 分钟冷却并被反复重试")
}

// 关键词不区分大小写,并且要能在 error code 上命中(很多上游只在结构化字段给原因)。
func TestPermanentKeywordIsCaseInsensitiveAndMatchesCode(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("BULK_PROBE_DETECTED")

	err := types.NewErrorWithStatusCode(errors.New("请稍后再试"),
		types.ErrorCode("bulk_probe_detected"), http.StatusTooManyRequests)
	require.True(t, AutoModelPermanentFailure(err))
}

// 保护项优先于关键词:上下文超长是"这次请求太长",不是"模型不存在"。
// 管理员加错关键词也不能把整个模型封 24 小时。
func TestKeywordCannotOverrideProtectedTransient(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("maximum context length")

	err := types.NewErrorWithStatusCode(errors.New("Maximum context length exceeded"),
		types.ErrorCode("context_length_exceeded"), http.StatusBadRequest)

	require.False(t, AutoModelPermanentFailure(err),
		"上下文超长必须永远按抖动处理,即使关键词命中")
}

// 没有命中关键词、也没有内置判据时,仍按抖动:不能因为配了关键词就扩大打击面。
func TestUnmatchedFailureStaysTransientWithKeywordsConfigured(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("some specific upstream string")

	err := types.NewErrorWithStatusCode(errors.New("Upstream unavailable"),
		types.ErrorCode("server_error"), http.StatusServiceUnavailable)
	require.False(t, AutoModelPermanentFailure(err))
}

// 关键词解析:按行切、去空白、去重、转小写,忽略空行。
func TestParsePermanentKeywordsNormalizesInput(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("  Function Calling  \n\nfunction calling\n  模型不存在  \r\n")

	got := operation_setting.AutoModelPermanentKeywords()
	require.Equal(t, []string{"function calling", "模型不存在"}, got)
}

// 面板数值必须能被读回,并且拒绝 0/负数(否则 auto 直接不可用)。
func TestAutoModelNumericSettingsRejectNonPositive(t *testing.T) {
	t.Cleanup(func() { operation_setting.InitAutoModelSettingsFromEnv() })
	operation_setting.InitAutoModelSettingsFromEnv()

	operation_setting.SetAutoModelMaxAttempts(0)
	require.Equal(t, 1, operation_setting.AutoModelMaxAttempts(), "0 要收敛到 1,不能变成不尝试")

	operation_setting.SetAutoModelMaxAttempts(7)
	require.Equal(t, 7, operation_setting.AutoModelMaxAttempts())

	operation_setting.SetAutoModelPermanentCooldownHours(-5)
	require.Equal(t, time.Hour, operation_setting.AutoModelPermanentCooldownBase())

	operation_setting.SetAutoModelScoreDecayMinutes(0)
	require.Equal(t, 1, operation_setting.AutoModelScoreDecayMinutes())
}

// 冷却封顶必须始终 >= 首次窗口,否则阶梯首犯就超过封顶。
func TestPermanentCooldownMaxNeverBelowBase(t *testing.T) {
	t.Cleanup(func() { operation_setting.InitAutoModelSettingsFromEnv() })
	operation_setting.InitAutoModelSettingsFromEnv()

	operation_setting.SetAutoModelPermanentCooldownHours(72)
	operation_setting.SetAutoModelPermanentCooldownMaxDays(1)
	require.Equal(t, 72*time.Hour, operation_setting.AutoModelPermanentCooldownMax(),
		"封顶低于首次窗口时按首次窗口兜底")
}

// 评分衰减窗口必须比抖动冷却窗口长,否则冷却到期时评分已归零回到中立,
// 惩罚与教训脱节,评分持久化也失去意义。这是这次修正的核心不变式。
func TestScoreDecayOutlastsTransientCooldown(t *testing.T) {
	t.Cleanup(func() { operation_setting.InitAutoModelSettingsFromEnv() })
	operation_setting.InitAutoModelSettingsFromEnv()

	decay := time.Duration(autoModelHalfLifeMs()) * time.Millisecond
	require.Greater(t, decay, autoModelCooldownBase,
		"评分衰减窗口必须长于抖动冷却窗口(15 分钟),否则冷却一到期评分就没记忆了")

	// 冷却到期那一刻,记忆必须还剩一部分。
	ageFactor := 1.0 - float64(autoModelCooldownBase.Milliseconds())/float64(autoModelHalfLifeMs())
	require.Greater(t, ageFactor, 0.0, "冷却到期时评分不该已经归零")
}