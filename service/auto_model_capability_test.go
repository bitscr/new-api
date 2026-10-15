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
		{"generic-invalid-request", "invalid request: messages[0].content", ""},
		// "模型不存在"是组合级问题,不是能力问题,必须留给永久判据。
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

// 能力不匹配要同时做对两件事,它们分属两个不同的判定,别把它们混起来:
//
//   - 冷却档位:按确定性失败处理(首犯 24 小时,再犯摘除该组合)。本网关的 auto
//     主要用于工具调用,一个给不了 tools 的模型对这个用途就是废的;不摘掉它会被
//     反复选中、每次白费一次尝试。
//   - 重试作用域:换模型而不是换渠道。同一条上游的同名模型通常一样不支持,
//     换渠道是白跑;而 400 默认不在重试状态码区间里,不提前判用户会直接拿到 400。
func TestAutoModelCapabilityFailureIsPermanentAndRetriesWithAnotherModel(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		err := types.NewOpenAIError(errors.New("tool calling is not supported"),
			types.ErrorCode("invalid_request_error"), status)
		require.True(t, AutoModelPermanentFailure(err),
			"能力不匹配是确定性失败,状态码 %d 也不例外", status)
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

// 能力措辞写成永久关键词要照常生效:管理员抄一次日志就应该能把它升级成长冷却,
// 这也是内置关键词表里那条 "function calling is not supported" 依赖的机制。
func TestAutoModelCapabilityFailureHonoursAdminKeyword(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("tool calling is not supported")

	err := types.NewOpenAIError(errors.New("tool calling is not supported"),
		types.ErrorCode("invalid_request_error"), http.StatusBadRequest)
	require.True(t, AutoModelPermanentFailure(err))
}

// 上游网关/CF 侧的 5xx 一律按确定性失败处理:这些是上游的问题,本网关改不了,
// 靠"每 15 分钟再撞一次"等它自愈只会让同一个组合反复被选中、反复失败。
//
// 判状态码而不是关键词是有原因的:RelayErrorHandler 把这类失败渲染成
// "bad response status code 524",旧的关键词写法 "status code: 524" 带冒号,
// 子串匹配永远命中不了——它们过去表现成抖动冷却,只是因为认不出的失败默认走抖动档。
func TestUpstreamGatewayStatusCodesArePermanent(t *testing.T) {
	for _, status := range []int{
		http.StatusGatewayTimeout, 520, 521, 522, 523, 524, 525, 526, 527,
	} {
		err := types.NewOpenAIError(
			errors.New("bad response status code"), types.ErrorCode("bad_response_status_code"), status)
		require.True(t, AutoModelPermanentFailure(err),
			"上游网关故障(HTTP %d)必须按确定性失败处理,否则它永远删不掉", status)
	}

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
