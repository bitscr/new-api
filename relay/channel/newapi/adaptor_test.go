package newapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// NewAPI 渠道的核心契约:所有入口路径 1:1 透传给上游 new-api 网关,
// 不做任何路径改写(对比 openai 渠道会把 claude 入站强制改写到
// /v1/chat/completions)。
func TestNewAPIAdaptorGetRequestURLPassesThroughClientPath(t *testing.T) {
	gin.SetMode(gin.TestMode)

	paths := []string{
		"/v1/chat/completions",
		"/v1/responses",
		"/v1/responses/compact",
		"/v1/messages",
		"/v1/embeddings",
		"/v1beta/models/gemini-2.5-flash:generateContent",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				RequestURLPath: path,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType:    constant.ChannelTypeNewAPI,
					ChannelBaseUrl: "https://gateway.example.com",
				},
			}
			a := &Adaptor{}
			url, err := a.GetRequestURL(info)
			require.NoError(t, err)
			require.Equal(t, "https://gateway.example.com"+path, url, "入口路径必须原样透传")
		})
	}
}

// 鉴权按入站格式分派:chat/responses 用 Bearer;claude 加 x-api-key 与
// anthropic-version;gemini 加 x-goog-api-key。
func TestNewAPIAdaptorSetupRequestHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)

	build := func(format types.RelayFormat, clientHeaders map[string]string) (*Adaptor, *gin.Context, *relaycommon.RelayInfo) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		req := httptest.NewRequest(http.MethodPost, "/v1/test", nil)
		for k, v := range clientHeaders {
			req.Header.Set(k, v)
		}
		c.Request = req
		info := &relaycommon.RelayInfo{
			RelayFormat: format,
			RelayMode:   relayconstant.RelayModeChatCompletions,
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType: constant.ChannelTypeNewAPI,
				ApiKey:      "sk-upstream-key",
			},
		}
		return &Adaptor{}, c, info
	}

	t.Run("chat 格式只用 Bearer", func(t *testing.T) {
		a, c, info := build(types.RelayFormatOpenAI, nil)
		header := http.Header{}
		require.NoError(t, a.SetupRequestHeader(c, &header, info))
		require.Equal(t, "Bearer sk-upstream-key", header.Get("Authorization"))
		require.Empty(t, header.Get("x-api-key"))
		require.Empty(t, header.Get("x-goog-api-key"))
	})

	t.Run("claude 格式补 x-api-key 与 anthropic-version", func(t *testing.T) {
		a, c, info := build(types.RelayFormatClaude, nil)
		header := http.Header{}
		require.NoError(t, a.SetupRequestHeader(c, &header, info))
		require.Equal(t, "Bearer sk-upstream-key", header.Get("Authorization"))
		require.Equal(t, "sk-upstream-key", header.Get("x-api-key"))
		require.Equal(t, "2023-06-01", header.Get("anthropic-version"))
	})

	t.Run("claude 格式沿用客户端的 anthropic-version", func(t *testing.T) {
		a, c, info := build(types.RelayFormatClaude, map[string]string{"anthropic-version": "2024-10-22"})
		header := http.Header{}
		require.NoError(t, a.SetupRequestHeader(c, &header, info))
		require.Equal(t, "2024-10-22", header.Get("anthropic-version"))
	})

	t.Run("gemini 格式补 x-goog-api-key", func(t *testing.T) {
		a, c, info := build(types.RelayFormatGemini, nil)
		header := http.Header{}
		require.NoError(t, a.SetupRequestHeader(c, &header, info))
		require.Equal(t, "Bearer sk-upstream-key", header.Get("Authorization"))
		require.Equal(t, "sk-upstream-key", header.Get("x-goog-api-key"))
	})
}

// 渠道类型到 API 类型的映射,以及端点声明。
func TestNewAPIChannelTypeWiring(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeNewAPI)
	require.True(t, ok)
	require.Equal(t, constant.APITypeNewAPI, apiType)
	require.Equal(t, "New API", constant.GetChannelTypeName(constant.ChannelTypeNewAPI))

	endpoints := common.GetEndpointTypesByChannelType(constant.ChannelTypeNewAPI, "gpt-5")
	require.Contains(t, endpoints, constant.EndpointTypeOpenAI)
	require.Contains(t, endpoints, constant.EndpointTypeOpenAIResponse)
	require.Contains(t, endpoints, constant.EndpointTypeAnthropic)
	require.Contains(t, endpoints, constant.EndpointTypeGemini)

	// 上游 new-api 网关支持 stream_options.include_usage,不做剥离。
	// (streamSupportedChannels 为包内私有,经由 GetRelayInfo 间接验证成本高,
	//  这里仅锁常量与映射;流式行为由 controller 层测试覆盖。)
}
