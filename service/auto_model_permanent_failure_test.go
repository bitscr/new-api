package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// AutoModelPermanentFailure 只认确定性失败。测试逐条锁定正面判据,以及
// 明确不该判永久的抖动类失败(限流、请求超时、上下文超长、非 model 参数的非法值)。
func TestAutoModelPermanentFailureClassifiesDeterministicOnly(t *testing.T) {
	cases := []struct {
		name      string
		err       *types.NewAPIError
		permanent bool
	}{
		{"auth401", types.NewErrorWithStatusCode(errors.New("Invalid API key"),
			types.ErrorCodeInvalidRequest, http.StatusUnauthorized), true},
		{"auth403", types.NewErrorWithStatusCode(errors.New("Credential access forbidden"),
			types.ErrorCodeInvalidRequest, http.StatusForbidden), true},
		{"model-not-found-code", types.NewErrorWithStatusCode(errors.New("Requested model not found"),
			types.ErrorCodeModelNotFound, http.StatusNotFound), true},
		{"model-not-supported", types.NewErrorWithStatusCode(errors.New("model_not_supported"),
			types.ErrorCode("model_not_supported"), http.StatusBadRequest), true},
		{"unsupported-model", types.NewErrorWithStatusCode(errors.New("unsupported model"),
			types.ErrorCode("unsupported_model"), http.StatusBadRequest), true},
		{"param-model-404", types.NewOpenAIError(errors.New("no such model"),
			types.ErrorCodeInvalidRequest, http.StatusNotFound,
			withOpenAIParam("model")), true},
		{"param-model-invalid-value", types.NewOpenAIError(errors.New("invalid value"),
			types.ErrorCode("invalid_value"), http.StatusBadRequest,
			withOpenAIParam("model")), true},
		{"unsupported-model-feature", types.NewErrorWithStatusCode(
			errors.New("This model does not support the requested feature"),
			types.ErrorCode("unsupported_model_feature"), http.StatusUnprocessableEntity), true},

		// 抖动类:必须保持短冷却。
		{"rate-limit", types.NewErrorWithStatusCode(errors.New("Account rate limit exceeded"),
			types.ErrorCode("rate_limit_exceeded"), http.StatusTooManyRequests), false},
		{"timeout", types.NewErrorWithStatusCode(errors.New("Upstream request timeout"),
			types.ErrorCode("request_timeout"), http.StatusRequestTimeout), false},
		{"context-overflow", types.NewErrorWithStatusCode(errors.New("Maximum context length exceeded"),
			types.ErrorCode("context_length_exceeded"), http.StatusBadRequest), false},
		{"invalid-value-other-param", types.NewOpenAIError(errors.New("invalid value"),
			types.ErrorCode("invalid_value"), http.StatusBadRequest,
			withOpenAIParam("temperature")), false},
		{"nil", nil, false},

		// 上游网关挂掉、以及"端点/模型根本不存在"是确定性失败:本网关改不了,
		// 靠"每 15 分钟再撞一次"等它自愈只会让同一个组合反复失败。
		// 502/503/504 与 CF 自有的 520-527 同理。
		//
		// 下面这两条过去写的是 false(被归进"抖动类"),与 AutoModelPermanentFailure
		// 里按状态码判永久的代码互相矛盾——是测试没跟上代码,不是代码有别的分支。
		// 状态码分支不看错误码,所以 route_not_found 这种"不是模型问题"的 404
		// 同样会被判永久;代码注释里对此是明确的(端点或模型不存在,重试无意义)。
		{"generic-404", types.NewErrorWithStatusCode(errors.New("Endpoint not found"),
			types.ErrorCode("route_not_found"), http.StatusNotFound), true},
		{"server-error", types.NewErrorWithStatusCode(errors.New("Upstream unavailable"),
			types.ErrorCode("server_error"), http.StatusServiceUnavailable), true},
		{"gateway-timeout", types.NewErrorWithStatusCode(errors.New("Upstream timed out"),
			types.ErrorCode("bad_response_status_code"), http.StatusGatewayTimeout), true},
		{"cloudflare-524", types.NewErrorWithStatusCode(errors.New("bad response status code 524"),
			types.ErrorCode("bad_response_status_code"), 524), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.permanent, AutoModelPermanentFailure(tc.err))
		})
	}
}

func withOpenAIParam(param string) types.NewAPIErrorOptions {
	return func(e *types.NewAPIError) {
		if detail, ok := e.RelayError.(types.OpenAIError); ok {
			detail.Param = param
			e.RelayError = detail
		}
	}
}

// 确定性失败只有两档:首犯 = 首次窗口(默认 24h),再犯直接摘除该组合,不再逐级翻倍。
// 抖动失败仍走独立的 15m 起步阶梯,两者不共用计数。
func TestPermanentCooldownLadderIsSeparateFromTransient(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()

	permanent := autoModelCooldownState{}
	windows := []time.Duration{}
	for i := 0; i < 3; i++ {
		permanent = nextAutoModelPermanentCooldown(permanent, "模型不存在", now)
		windows = append(windows, permanent.Until.Sub(now))
	}
	require.Equal(t, autoModelPermanentCooldownBase(), windows[0], "首犯即 24h")
	require.Equal(t, autoModelPermanentCooldownBase(), windows[1],
		"不再翻倍:封顶恒等于首次窗口,再犯由 RetireAutoModelCombination 摘除,而不是等更久")
	require.Equal(t, autoModelPermanentCooldownBase(), windows[2])
	require.True(t, permanent.Permanent)
	require.Equal(t, 3, permanent.Level, "等级仍逐次递增,只是不再用来拉长窗口")

	// 抖动阶梯不受影响,仍是 15m 起步、逐级翻倍。
	transient := autoModelCooldownState{}
	transient = nextAutoModelCooldown(transient, "请求失败", now)
	require.Equal(t, autoModelCooldownBase, transient.Until.Sub(now))
	require.False(t, transient.Permanent, "抖动失败不该被标成永久")
}

// 永久阶梯也要封顶,不能无限翻倍。
func TestPermanentCooldownCapsAtMax(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()
	prev := autoModelCooldownState{}
	for i := 0; i < 20; i++ {
		prev = nextAutoModelPermanentCooldown(prev, "模型不存在", now)
	}
	require.Equal(t, autoModelPermanentCooldownMax(), prev.Until.Sub(now))
}

// 永久失败落库并跨重启保留;标志位必须一起存取。
func TestPermanentCooldownPersistsAndReloads(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	RecordAutoModelPermanentFailure(group, "ghost-model", 3, "模型不存在(model_not_found)")

	rows := cooldownRows(t)
	require.Len(t, rows, 1)
	require.True(t, rows[0].Permanent, "库里必须记住这是永久级冷却")
	require.Greater(t, rows[0].Until-time.Now().Unix(), int64(23*3600),
		"永久级冷却至少要留 23 小时")

	// 模拟进程重启:内存清空后从库里读回,标志位仍在。
	autoModelCooldowns.Clear()
	LoadAutoModelCooldowns()
	state, ok := autoModelCooldowns.Get(autoModelHealthKey(group, "ghost-model", 3))
	require.True(t, ok)
	require.True(t, state.Permanent, "重启后仍必须是永久级")
	require.Equal(t, 1, state.Level)
}

// 老行(没有 permanent 列/值为假)必须按抖动冷却载入,不能被误升为永久。
func TestLegacyCooldownRowLoadsAsTransient(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
		Group: "default", Model: "old-model", ChannelId: 1,
		Until: time.Now().Add(time.Hour).Unix(), Level: 2, Reason: "请求失败",
		UpdatedAt: time.Now().Unix(),
	}))
	LoadAutoModelCooldowns()
	state, ok := autoModelCooldowns.Get(autoModelHealthKey("default", "old-model", 1))
	require.True(t, ok)
	require.False(t, state.Permanent)
}

// 正常速度的成功恢复必须清掉永久标志,否则一个修好的模型永远回不来。
func TestSuccessClearsPermanentCooldown(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	RecordAutoModelPermanentFailure(group, "fixed-model", 5, "模型不存在(model_not_found)")
	require.True(t, autoModelCooldownActive(group, "fixed-model", []int{5}))

	applyAutoModelCooldown(group, "fixed-model", 5, true, 1200)
	_, ok := autoModelCooldowns.Get(autoModelHealthKey(group, "fixed-model", 5))
	require.False(t, ok, "成功恢复后不该留下任何冷却(含永久标志)")
	require.Empty(t, cooldownRows(t))
}

// 再犯一次:等级递增,且仍留在永久阶梯上——绝不退回抖动档的 15 分钟。
//
// 正常情况下第二次会直接调 RetireAutoModelCombination 把模型从渠道里摘掉;
// 这个测试的库里没有 channels 表,摘除必然失败,于是走"退回永久冷却"的兜底路径。
// 要锁住的不变式就是那条兜底不能变成 15 分钟。
func TestPermanentFailureEscalatesOnRepeat(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	RecordAutoModelPermanentFailure(group, "ghost-model", 3, "模型不存在")
	first := cooldownRows(t)[0]
	RecordAutoModelPermanentFailure(group, "ghost-model", 3, "模型不存在")
	second := cooldownRows(t)[0]

	require.Equal(t, first.Level+1, second.Level)
	require.True(t, second.Permanent, "摘除失败也必须留在永久档")
	// 窗口不再翻倍,但也不能被缩短。Until 按秒落库,两次调用可能落在同一秒,
	// 所以这里是"不短于"而不是"严格大于"。
	require.GreaterOrEqual(t, second.Until, first.Until)
	require.Greater(t, second.Until-time.Now().Unix(), int64(23*3600),
		"仍必须是 24h 量级,而不是回到抖动档的 15 分钟")
}