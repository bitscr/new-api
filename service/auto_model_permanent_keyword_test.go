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
func TestPermanentKeywordUpgradesUnknownFailure(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("anonymous backend retired")

	err := types.NewErrorWithStatusCode(
		errors.New("the anonymous backend retired this route for your key"),
		types.ErrorCode("invalid_request_error"), http.StatusBadRequest)

	require.True(t, AutoModelPermanentFailure(err),
		"关键词命中就该升级为永久级,否则这种失败只会拿 15 分钟冷却并被反复重试")
}

// 能力不匹配必须按确定性失败处理,内置关键词表里那条 "function calling is not
// supported" 就是干这个的:本网关的 auto 主要用于工具调用,一个给不了 tools 的
// 模型对这个用途就是废的,留着只会被反复选中、每次白费一次尝试。
//
// 日志里的真实样本:`tools / function calling is not supported by the upstream
// anonymous backend`。
func TestCapabilityFailureBecomesPermanent(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })

	err := types.NewOpenAIError(
		errors.New("tools / function calling is not supported by the upstream anonymous backend; remove `tools`"),
		types.ErrorCode("invalid_request_error"), http.StatusBadRequest)

	require.Equal(t, "tools", AutoModelCapabilityFailure(err))
	require.True(t, AutoModelPermanentFailure(err),
		"内置判据就该认出来:首犯冷却 24 小时,再犯摘除这个组合")
	require.Equal(t, AutoModelRetryNextModel, ClassifyAutoModelRetry(err),
		"这次请求仍然要换个模型继续跑,不能直接甩一个 400 给调用方")
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

	// 用一个既不在内置判据、也不在关键词表里的 400。
	// 不能拿 5xx 当例子:502/503/504 与 CF 的 520-527 现在由状态码直接判永久,
	// 用它们就测不到"关键词不命中"这件事了。
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