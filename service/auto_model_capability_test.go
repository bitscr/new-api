package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// 能力判据要认出"上游明说这个模型不提供某种能力"的措辞,并把能力名带出来
// (日志要用它,将来"按模型记录不支持什么"也要用它做键)。
func TestAutoModelCapabilityFailureRecognizesFeatureWording(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{"tools-en", "function calling is not supported", "tools"},
		{"tools-en-long", "tools / function calling is not supported by the upstream backend", "tools"},
		{"tools-cn", "该模型不支持工具调用", "tools"},
		{"vision-en", "This model does not support image input", "vision"},
		{"json-schema-en", "response_format is not supported for this model", "json_schema"},
		{"reasoning-en", "thinking is not supported by this model", "reasoning"},
		{"audio-cn", "不支持音频输入", "audio"},
		// 泛化措辞刻意不收:可能是请求体别的字段写错,算到模型头上会误伤。
		{"generic-unsupported-parameter", "unsupported parameter: temperature", ""},
		{"generic-invalid-request", "invalid request: messages[0].content", ""},		// "模型不存在"是组合级问题,不是能力问题,必须留给关键词判据。
		{"model-not-found", "the model gpt-5 does not exist", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := types.NewOpenAIError(errors.New(tc.message),
				types.ErrorCode("invalid_request_error"), http.StatusBadRequest)
			require.Equal(t, tc.want, AutoModelCapabilityFailure(err))
		})
	}
	require.Empty(t, AutoModelCapabilityFailure(nil))
}

// 能力不匹配只影响重试作用域:换模型而不是换渠道。同一条上游的同名模型通常
// 一样不支持,换渠道是白跑;而 400 默认不在重试状态码区间里,不提前判用户会
// 直接拿到 400。
//
// 它不再是内置的确定性判据:未配置确定性关键词时,这类失败按抖动失败处理;
// 管理员把能力措辞配成关键词后才会升级(见 TestAutoModelCapabilityFailureHonoursAdminKeyword)。
func TestAutoModelCapabilityFailureRetriesWithAnotherModel(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		err := types.NewOpenAIError(errors.New("tool calling is not supported"),
			types.ErrorCode("invalid_request_error"), status)
		require.False(t, AutoModelPermanentFailure(err),
			"能力措辞不再是内置判据,状态码 %d 也不例外", status)
		require.Equal(t, AutoModelRetryNextModel, ClassifyAutoModelRetry(err),
			"必须换模型,而且要在状态码白名单之前判——400 默认不可重试")
	}
}

// 渠道配置错误与本地请求体错误仍走各自原有的分支,不能被能力判据抢走。
func TestAutoModelCapabilityFailureDoesNotOverrideChannelOrLocalDecisions(t *testing.T) {
	channelErr := types.NewErrorWithStatusCode(errors.New("tool calling is not supported"),
		types.ErrorCodeChannelNoAvailableKey, http.StatusBadRequest)
	require.Equal(t, AutoModelRetryNextChannel, ClassifyAutoModelRetry(channelErr),
		"渠道配置错误必须先于能力判据")

	// 网关自己判定"这次请求不成立"时没有上游可换,换模型也没有意义。
	local := types.NewErrorWithStatusCode(errors.New("tool calling is not supported"),
		types.ErrorCodeInvalidRequest, http.StatusBadRequest)
	require.Equal(t, AutoModelRetryStop, ClassifyAutoModelRetry(local),
		"本地请求体错误保持不重试")
}

// 能力措辞写成永久关键词要照常生效:管理员抄一次日志就应该能把这类失败
// 判成确定性失败(命中即直接摘除组合)。
func TestAutoModelCapabilityFailureHonoursAdminKeyword(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("tool calling is not supported")

	err := types.NewOpenAIError(errors.New("tool calling is not supported"),
		types.ErrorCode("invalid_request_error"), http.StatusBadRequest)
	require.True(t, AutoModelPermanentFailure(err))
}

// 上游网关/CF 侧的 5xx 不再有内置的确定性判定:源码不内置状态码判据,未配置
// 关键词时它们按抖动失败走普通冷却阶梯。管理员想摘掉某个组合,把对应文案配成
// 确定性关键词即可(注意 RelayErrorHandler 把这类失败渲染成
// "bad response status code 524",没有冒号,关键词要按这个写法配)。
func TestUpstreamGatewayStatusCodesStayTransientWithoutKeywords(t *testing.T) {
	for _, status := range []int{
		http.StatusGatewayTimeout, 520, 521, 522, 523, 524, 525, 526, 527,
	} {
		err := types.NewOpenAIError(
			errors.New("bad response status code"), types.ErrorCode("bad_response_status_code"), status)
		require.False(t, AutoModelPermanentFailure(err),
			"未配置关键词时 HTTP %d 不得被判成确定性失败", status)
	}

	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("status code 524")
	err := types.NewOpenAIError(
		errors.New("bad response status code 524"), types.ErrorCode("bad_response_status_code"), 524)
	require.True(t, AutoModelPermanentFailure(err), "配置关键词后按正文命中")

	// 限流仍是抖动失败:冷却阶梯才是它该待的地方。
	rateLimited := types.NewOpenAIError(errors.New("rpm exhausted"),
		types.ErrorCode("rate_limit_exceeded"), http.StatusTooManyRequests)
	require.False(t, AutoModelPermanentFailure(rateLimited))
}

// 措辞表是判定与面板共用的同一份数据,避免两处各写一份而悄悄失配。
func TestMatchAutoModelCapabilityText(t *testing.T) {
	require.Equal(t, "tools", operation_setting.MatchAutoModelCapabilityText("Function Calling Is Not Supported"))
	require.Empty(t, operation_setting.MatchAutoModelCapabilityText("anonymous backend retired"))
	require.NotEmpty(t, operation_setting.AutoModelCapabilityTexts())
}
