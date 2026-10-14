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

	// 衰减窗口的下限不是 1,而是"封顶小时数换算的分钟数":低于它时最高几档
	// 解冻时扣分已归零,冷却只剩"等时间"。写 0 会被抬到封顶,而不是变成 1。
	operation_setting.SetAutoModelScoreDecayMinutes(0)
	require.Equal(t, operation_setting.AutoModelCooldownMaxHours()*60,
		operation_setting.AutoModelScoreDecayMinutes(),
		"衰减窗口不能低于封顶所换算的分钟数")
}

// 封顶与衰减窗口的联动:无论两个键以什么顺序写入,不变式"衰减 >= 封顶*60"
// 都必须成立。前端把改动项放在同一批并发 PUT 里,到达顺序不确定,所以两种
// 顺序都要收敛到同一结果。
func TestCooldownMaxAndScoreDecayStayConsistent(t *testing.T) {
	t.Cleanup(func() { operation_setting.InitAutoModelSettingsFromEnv() })

	for _, writes := range [][2]string{
		{"max", "decay"},
		{"decay", "max"},
	} {
		operation_setting.InitAutoModelSettingsFromEnv()
		for _, key := range writes {
			switch key {
			case "max":
				operation_setting.SetAutoModelCooldownMaxHours(12)
			case "decay":
				operation_setting.SetAutoModelScoreDecayMinutes(240)
			}
			require.GreaterOrEqual(t,
				operation_setting.AutoModelScoreDecayMinutes(),
				operation_setting.AutoModelCooldownMaxHours()*60,
				"写入顺序 %v:衰减窗口必须不低于封顶", writes)
		}
	}

	// 未显式设过衰减时,只改封顶应把衰减一起带过去。
	operation_setting.InitAutoModelSettingsFromEnv()
	operation_setting.SetAutoModelCooldownMaxHours(3)
	require.Equal(t, 180, operation_setting.AutoModelScoreDecayMinutes(),
		"未显式设置时,衰减随封顶联动(3 小时 = 180 分钟)")
}

// 确定性失败已改为两档:首犯冷却 = 首次窗口,再犯直接摘除模型。
// 所以封顶恒等于首次窗口,不再有"封顶不能低于首次窗口"的配置矛盾。
func TestPermanentCooldownMaxEqualsBase(t *testing.T) {
	t.Cleanup(func() { operation_setting.InitAutoModelSettingsFromEnv() })
	operation_setting.InitAutoModelSettingsFromEnv()

	operation_setting.SetAutoModelPermanentCooldownHours(72)
	require.Equal(t, 72*time.Hour, operation_setting.AutoModelPermanentCooldownMax(),
		"不再逐级翻倍:封顶恒等于首次窗口")
}

// 评分衰减窗口必须覆盖住抖动冷却阶梯的靠前几档,不能只看首档。
//
// 关键反例:窗口 = 30 分钟时,第 2 次失败的冷却窗口恰好也是 30 分钟,
// 于是解冻那一刻评分精确回到中立 0.5,惩罚"刚好"作废。所以不变式是
// "窗口 > 第二档",这也是把默认值从 30 改成 180 的原因。
func TestScoreDecayOutlastsTransientCooldown(t *testing.T) {
	t.Cleanup(func() { operation_setting.InitAutoModelSettingsFromEnv() })
	operation_setting.InitAutoModelSettingsFromEnv()

	decay := time.Duration(autoModelHalfLifeMs()) * time.Millisecond
	require.Greater(t, decay, autoModelCooldownSecondTier,
		"评分衰减窗口必须大于冷却第二档(30 分钟),否则第 2 次失败解冻时扣分已归零")

	// 逐档验证:前几档解冻时都必须还剩记忆。
	for _, window := range []time.Duration{
		autoModelCooldownBase,
		autoModelCooldownBase * 2,
		autoModelCooldownBase * 4,
	} {
		ageFactor := 1.0 - float64(window.Milliseconds())/float64(autoModelHalfLifeMs())
		require.Greater(t, ageFactor, 0.0, "冷却 %s 到期时评分不该已经归零", window)
	}
}