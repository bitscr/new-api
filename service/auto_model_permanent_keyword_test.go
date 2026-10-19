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

// 管理员配置的关键词必须能把日志里出现的确定性失败判出来——这是唯一的正向判据。
func TestPermanentKeywordUpgradesUnknownFailure(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("anonymous backend retired")

	err := types.NewErrorWithStatusCode(
		errors.New("the anonymous backend retired this route for your key"),
		types.ErrorCode("invalid_request_error"), http.StatusBadRequest)

	require.True(t, AutoModelPermanentFailure(err),
		"关键词命中就应判为确定性失败,否则这种组合会被反复选中、反复失败")
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
// 管理员加错关键词也不能把整个模型摘掉。
func TestKeywordCannotOverrideProtectedTransient(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("maximum context length")

	err := types.NewErrorWithStatusCode(errors.New("Maximum context length exceeded"),
		types.ErrorCode("context_length_exceeded"), http.StatusBadRequest)

	require.False(t, AutoModelPermanentFailure(err),
		"上下文超长必须永远按抖动处理,即使关键词命中")
}

// 没有命中关键词时,仍按抖动:不能因为配了关键词就扩大打击面。
func TestUnmatchedFailureStaysTransientWithKeywordsConfigured(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("some specific upstream string")

	err := types.NewErrorWithStatusCode(errors.New("Upstream hiccup"),
		types.ErrorCode("server_error"), http.StatusBadRequest)
	require.False(t, AutoModelPermanentFailure(err), "关键词不命中就不能扩大打击面")
}

// 关键词解析:按行切、去空白、去重、转小写,忽略空行。
func TestParsePermanentKeywordsNormalizesInput(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("  Function Calling  \n\nfunction calling\n  模型不存在  \r\n")

	got := operation_setting.AutoModelPermanentKeywords()
	require.Equal(t, []string{"function calling", "模型不存在"}, got)
}

// 未配置关键词时,两个判定集合都是空的:源码不再内置任何条目,
// 数据库(面板)里有什么,运行时就用什么。
func TestUnconfiguredKeywordSetsAreEmpty(t *testing.T) {
	operation_setting.SetAutoModelPermanentKeywords("")
	operation_setting.SetAutoModelTransientKeywords("")
	require.Empty(t, operation_setting.AutoModelPermanentKeywords())
	require.Empty(t, operation_setting.AutoModelTransientKeywords())
}

// 面板数值必须能被读回,并且拒绝 0/负数(否则 auto 直接不可用)。
func TestAutoModelNumericSettingsRejectNonPositive(t *testing.T) {
	t.Cleanup(func() { operation_setting.InitAutoModelSettingsFromEnv() })
	operation_setting.InitAutoModelSettingsFromEnv()

	operation_setting.SetAutoModelMaxAttempts(0)
	require.Equal(t, 1, operation_setting.AutoModelMaxAttempts(), "0 要收敛到 1,不能变成不尝试")

	operation_setting.SetAutoModelMaxAttempts(7)
	require.Equal(t, 7, operation_setting.AutoModelMaxAttempts())

	// 衰减窗口的下限只有 1:封顶与衰减窗口之间**没有**联动改写。
	// 不合逻辑的组合(衰减 < 封顶)允许保存,面板会附带建议值提示,
	// 而不是悄悄替管理员改值——改掉会让人以为自己填的生效了,反而更迷惑。
	operation_setting.SetAutoModelScoreDecayMinutes(0)
	require.Equal(t, 1, operation_setting.AutoModelScoreDecayMinutes(),
		"低于下限时收敛到 1,而不是被抬到封顶所换算的分钟数")
}

// 封顶与衰减窗口之间没有联动改写:两个 setter 都只存值(各自收敛到下限 1),
// 无论以什么顺序写入,读回的都是写入值本身。
//
// 不合逻辑的组合由面板给建议值提示(controller/option.go 的 suggestion 机制),
// 不在 setter 里静默改值——前端把两个键放在同一批并发 PUT 里,静默改值会造成
// "一个被改、另一个没改"的半应用,而且管理员不知道自己填的没生效。
func TestCooldownMaxAndScoreDecayAreStoredVerbatim(t *testing.T) {
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
				require.Equal(t, 12, operation_setting.AutoModelCooldownMaxHours(),
					"写入顺序 %v:封顶按写入值保存", writes)
			case "decay":
				operation_setting.SetAutoModelScoreDecayMinutes(240)
				require.Equal(t, 240, operation_setting.AutoModelScoreDecayMinutes(),
					"写入顺序 %v:衰减窗口按写入值保存", writes)
			}
		}
		// 两个键互不联动:两个都写完之后,读回的仍是各自写入的值。
		require.Equal(t, 12, operation_setting.AutoModelCooldownMaxHours())
		require.Equal(t, 240, operation_setting.AutoModelScoreDecayMinutes())
	}

	// 不合逻辑的组合也照样保存,只是面板会给出建议值。
	operation_setting.InitAutoModelSettingsFromEnv()
	operation_setting.SetAutoModelCooldownMaxHours(12)
	operation_setting.SetAutoModelScoreDecayMinutes(60)
	require.Equal(t, 60, operation_setting.AutoModelScoreDecayMinutes(),
		"衰减窗口小于封顶时只给建议,不替管理员改值")
	require.Equal(t, 720, operation_setting.AutoModelRecommendedScoreDecayMinutes(),
		"建议值 = 封顶换算的分钟数(12 小时 = 720 分钟)")
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
