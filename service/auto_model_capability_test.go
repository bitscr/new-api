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

// 命中能力措辞后,判定必须同时做到三件事:
//   - 不升级成长冷却(否则 24 小时里连不需要该能力的请求一起被挡);
//   - 不摘除模型(再犯会从渠道的 models 列表里永久删掉它);
//   - 重试作用域是换模型(同上游的同名模型通常一样不支持,换渠道是白跑)。
func TestAutoModelCapabilityFailureIsNeitherPermanentNorChannelScoped(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		err := types.NewOpenAIError(errors.New("tool calling is not supported"),
			types.ErrorCode("invalid_request_error"), status)
		require.False(t, AutoModelPermanentFailure(err),
			"能力不匹配不是确定性失败,状态码 %d 也不例外", status)
		require.Equal(t, AutoModelRetryNextModel, ClassifyAutoModelRetry(err))
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

// 能力判据优先于管理员关键词,否则抄一次日志就能把"摘除模型"这个坑重新踩回来。
func TestAutoModelCapabilityFailureBeatsAdminKeyword(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("tool calling is not supported")

	err := types.NewOpenAIError(errors.New("tool calling is not supported"),
		types.ErrorCode("invalid_request_error"), http.StatusBadRequest)
	require.False(t, AutoModelPermanentFailure(err))
}

// 措辞表与判定共用同一份数据:面板校验用 MatchAutoModelCapabilityText 挡住
// "永远轮不到"的条目,这里锁住那条共用路径确实认得出能力措辞。
func TestMatchAutoModelCapabilityTextIsSharedWithValidation(t *testing.T) {
	require.Equal(t, "tools", operation_setting.MatchAutoModelCapabilityText("Function Calling Is Not Supported"))
	require.Empty(t, operation_setting.MatchAutoModelCapabilityText("anonymous backend retired"))
	require.NotEmpty(t, operation_setting.AutoModelCapabilityTexts())
}
