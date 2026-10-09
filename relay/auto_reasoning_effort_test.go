package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Capture the actual HTTP request, then reject it before billing. Each helper
// invocation reads the same original DTO, just as successive auto attempts do.
type autoEffortFixture struct {
	c        *gin.Context
	info     *relaycommon.RelayInfo
	captured chan []byte
	format   types.RelayFormat
}

func newAutoEffortFixture(t *testing.T, format types.RelayFormat, modelName, fields string, auto, stream bool) *autoEffortFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	service.InitHttpClient()
	settings := model_setting.GetGlobalSettings()
	saved := *settings
	t.Cleanup(func() { *settings = saved })
	settings.PassThroughRequestEnabled = false
	settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{}
	if format == "" {
		format = types.RelayFormatOpenAI
	}
	var payload map[string]any
	require.NoError(t, common.UnmarshalJsonStr(fields, &payload))
	payload["model"], payload["stream"] = modelName, stream
	path := "/v1/chat/completions"
	switch format {
	case types.RelayFormatOpenAIResponses:
		path, payload["input"] = "/v1/responses", "hi"
	case types.RelayFormatClaude:
		path = "/v1/messages"
		payload["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
		payload["max_tokens"] = 64
	case types.RelayFormatGemini:
		path = "/v1beta/models/" + modelName + ":generateContent"
		payload["contents"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}
	default:
		payload["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
	}
	body, err := common.Marshal(payload)
	require.NoError(t, err)
	captured := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "fixture read failed", http.StatusInternalServerError)
			return
		}
		captured <- data
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"effort fixture captured"}}`)
	}))
	t.Cleanup(server.Close)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "model", Value: modelName + ":generateContent"}}
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "fixture-only")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, modelName)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
	common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{RemoveGifImagesEnabled: common.GetPointer(false)})
	if auto {
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	}
	request, apiErr := helper.GetAndValidateRequest(c, format)
	require.NoError(t, apiErr)
	info := &relaycommon.RelayInfo{
		Request: request, OriginModelName: modelName, RelayFormat: format,
		RelayMode: relayconstant.Path2RelayMode(path), RequestURLPath: path,
		StartTime: time.Now(), DisablePing: true, IsStream: stream, ReasoningEffort: "stale",
	}
	return &autoEffortFixture{c: c, info: info, captured: captured, format: format}
}

func (f *autoEffortFixture) send(t *testing.T) []byte {
	t.Helper()
	var apiErr *types.NewAPIError
	switch f.format {
	case types.RelayFormatOpenAIResponses:
		apiErr = ResponsesHelper(f.c, f.info)
	case types.RelayFormatClaude:
		apiErr = ClaudeHelper(f.c, f.info)
	case types.RelayFormatGemini:
		apiErr = GeminiHelper(f.c, f.info)
	default:
		apiErr = TextHelper(f.c, f.info)
	}
	require.NotNil(t, apiErr)
	require.Equal(t, http.StatusBadRequest, apiErr.StatusCode, apiErr.Error())
	select {
	case body := <-f.captured:
		return body
	default:
		t.Fatalf("request failed before reaching fixture: %v", apiErr)
		return nil
	}
}

func TestAutoReasoningEffortMaxChatFamilies(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"openai/gpt-6-luna", "max"},
		{"deepseek-ai/deepseek-v4-flash-0731", "max"},
		{"nim/nvidia/glm-5.3-flash", "max"},
		{"intern/inkstone/qwen3.8-flash", "xhigh"},
		{"xai/grok-4.6", "xhigh"},
		{"moonshotai/kimi-k2.7-code", ""},
		{"MiniMaxAI/MiniMax-M3", ""},
		{"google/gemini-3.1-pro-preview", "high"},
		{"provider/unknown-model", "high"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			f := newAutoEffortFixture(t, types.RelayFormatOpenAI, tc.model, `{"reasoning_effort":"max","temperature":0}`, true, false)
			body := f.send(t)
			require.Equal(t, tc.want, gjson.GetBytes(body, "reasoning_effort").String())
			if tc.want == "" {
				require.False(t, gjson.GetBytes(body, "reasoning_effort").Exists())
			}
			require.Equal(t, tc.model, gjson.GetBytes(body, "model").String())
			require.Equal(t, "0", gjson.GetBytes(body, "temperature").Raw)
			require.Equal(t, tc.want, f.info.ReasoningEffort)
			require.Equal(t, "max", f.info.Request.(*dto.GeneralOpenAIRequest).ReasoningEffort)
		})
	}
}

func TestAutoReasoningEffortResponses(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"qwen/qwen3.8-27b", "xhigh"}, {"grok-4.6", "xhigh"},
		{"kimi-k3", ""}, {"minimax-m3", ""},
		{"deepseek-v4-flash", "max"}, {"gemini-3-flash-preview", "high"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			f := newAutoEffortFixture(t, types.RelayFormatOpenAIResponses, tc.model, `{"reasoning":{"effort":"max","summary":"detailed"}}`, true, false)
			body := f.send(t)
			require.Equal(t, tc.want, gjson.GetBytes(body, "reasoning.effort").String())
			if tc.want == "" {
				require.False(t, gjson.GetBytes(body, "reasoning.effort").Exists())
			}
			require.Equal(t, "detailed", gjson.GetBytes(body, "reasoning.summary").String())
			require.Equal(t, tc.want, f.info.ReasoningEffort)
			require.Equal(t, "max", f.info.Request.(*dto.OpenAIResponsesRequest).Reasoning.Effort)
		})
	}
}

func TestAutoReasoningEffortNestedChat(t *testing.T) {
	for _, tc := range []struct{ model, want string }{{"qwen3.8-flash", "xhigh"}, {"kimi-k3", ""}} {
		t.Run(tc.model, func(t *testing.T) {
			f := newAutoEffortFixture(t, types.RelayFormatOpenAI, tc.model, `{"reasoning_effort":"max","reasoning":{"effort":"max","enabled":true,"exclude":false,"budget_tokens":0},"thinking":{"type":"enabled"}}`, true, false)
			body := f.send(t)
			require.Equal(t, tc.want, gjson.GetBytes(body, "reasoning.effort").String())
			if tc.want == "" {
				require.False(t, gjson.GetBytes(body, "reasoning_effort").Exists())
				require.False(t, gjson.GetBytes(body, "reasoning.effort").Exists())
			}
			require.Equal(t, "true", gjson.GetBytes(body, "reasoning.enabled").Raw)
			require.Equal(t, "false", gjson.GetBytes(body, "reasoning.exclude").Raw)
			require.Equal(t, "0", gjson.GetBytes(body, "reasoning.budget_tokens").Raw)
			require.Equal(t, "enabled", gjson.GetBytes(body, "thinking.type").String())
		})
	}
}

func TestAutoReasoningEffortPassthrough(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, tc := range []struct{ model, want string }{{"qwen3.8-flash", "xhigh"}, {"kimi-k3", ""}} {
			t.Run(string(format)+"/"+tc.model, func(t *testing.T) {
				fields, path := `{"reasoning_effort":"max","unknown":false,"seed":0}`, "reasoning_effort"
				if format == types.RelayFormatOpenAIResponses {
					fields, path = `{"reasoning":{"effort":"max","summary":"detailed"},"unknown":false,"seed":0}`, "reasoning.effort"
				}
				f := newAutoEffortFixture(t, format, tc.model, fields, true, false)
				common.SetContextKey(f.c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: true})
				// Passthrough ignores this mapping, so the real body model must win.
				f.c.Set("model_mapping", `{"qwen3.8-flash":"gpt-6-luna","kimi-k3":"gpt-6-luna"}`)
				body := f.send(t)
				require.Equal(t, tc.model, gjson.GetBytes(body, "model").String())
				require.Equal(t, tc.want, gjson.GetBytes(body, path).String())
				if tc.want == "" {
					require.False(t, gjson.GetBytes(body, path).Exists())
				}
				require.Equal(t, "false", gjson.GetBytes(body, "unknown").Raw)
				require.Equal(t, "0", gjson.GetBytes(body, "seed").Raw)
				require.Equal(t, tc.want, f.info.ReasoningEffort)
				storage, err := common.GetBodyStorage(f.c)
				require.NoError(t, err)
				original, err := storage.Bytes()
				require.NoError(t, err)
				require.Equal(t, "max", gjson.GetBytes(original, path).String())
			})
		}
	}
}

func TestAutoReasoningEffortNativeInputs(t *testing.T) {
	for _, tc := range []struct {
		name, model, fields, path, rawPath, want string
		format                                   types.RelayFormat
		channel                                  int
	}{
		{"claude fallback", "claude-opus-4-6", `{"output_config":{"effort":"max","custom":false}}`, "output_config.effort", "output_config.effort", "high", types.RelayFormatClaude, constant.ChannelTypeAnthropic},
		{"claude kimi", "kimi-k3", `{"output_config":{"effort":"max","custom":false},"thinking":{"type":"adaptive"}}`, "output_config.effort", "output_config.effort", "", types.RelayFormatClaude, constant.ChannelTypeAnthropic},
		{"gemini", "gemini-3-pro-preview", `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"max","includeThoughts":true}}}`, "generationConfig.thinkingConfig.thinkingLevel", "generationConfig.thinkingConfig.thinkingLevel", "high", types.RelayFormatGemini, constant.ChannelTypeGemini},
		{"gemini snake", "gemini-3-pro-preview", `{"generationConfig":{"thinking_config":{"thinking_level":"max","include_thoughts":true}}}`, "generationConfig.thinkingConfig.thinkingLevel", "generationConfig.thinking_config.thinking_level", "high", types.RelayFormatGemini, constant.ChannelTypeGemini},
	} {
		for _, raw := range []bool{false, true} {
			name := tc.name + "/converted"
			if raw {
				name = tc.name + "/passthrough"
			}
			t.Run(name, func(t *testing.T) {
				f := newAutoEffortFixture(t, tc.format, tc.model, tc.fields, true, false)
				common.SetContextKey(f.c, constant.ContextKeyChannelType, tc.channel)
				common.SetContextKey(f.c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: raw})
				body := f.send(t)
				path := tc.path
				if raw {
					path = tc.rawPath
				}
				require.Equal(t, tc.want, gjson.GetBytes(body, path).String())
				if tc.want == "" {
					require.False(t, gjson.GetBytes(body, path).Exists())
					require.Equal(t, "adaptive", gjson.GetBytes(body, "thinking.type").String())
				}
				if tc.format == types.RelayFormatClaude {
					require.Equal(t, "false", gjson.GetBytes(body, "output_config.custom").Raw)
				}
				require.Equal(t, tc.want, f.info.ReasoningEffort)
			})
		}
	}
}

func TestAutoReasoningEffortDeepSeekUnchanged(t *testing.T) {
	f := newAutoEffortFixture(t, types.RelayFormatOpenAI, "deepseek-ai/deepseek-v4-flash-0731", `{"reasoning_effort":"xhigh"}`, true, false)
	body := f.send(t)
	require.Equal(t, "xhigh", gjson.GetBytes(body, "reasoning_effort").String())
	require.Equal(t, "xhigh", f.info.ReasoningEffort)
	require.Equal(t, "xhigh", f.info.Request.(*dto.GeneralOpenAIRequest).ReasoningEffort)
}
