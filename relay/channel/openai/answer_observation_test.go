package openai

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	observationError   = `{"error":{"message":"upstream failed","type":"server_error","code":"overloaded"}}`
	observationPartial = `{"id":"test","model":"test-model","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}`
	observationUsage   = `"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}`
	observationStop    = `{"id":"test","model":"test-model","created":1,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` + observationUsage + `}`
)

type observationFormat struct {
	name            string
	format          types.RelayFormat
	force, thinking bool
}

func observationFormats() []observationFormat {
	return []observationFormat{
		{"passthrough", types.RelayFormatOpenAI, false, false},
		{"force", types.RelayFormatOpenAI, true, false},
		{"thinking", types.RelayFormatOpenAI, false, true},
		{"both", types.RelayFormatOpenAI, true, true},
		{"claude", types.RelayFormatClaude, false, false},
		{"gemini", types.RelayFormatGemini, false, false},
	}
}

func newObservedAnswerContext(t *testing.T, format observationFormat, stream bool) (*gin.Context, *httptest.ResponseRecorder, *relaycommon.RelayInfo) {
	t.Helper()
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 3
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "test-model")
	common.SetContextKey(c, constant.ContextKeyEstimatedTokens, 7)
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{
		ShowErrorDetails: true, ForceFormat: format.force, ThinkingToContent: format.thinking,
	})
	request := &dto.GeneralOpenAIRequest{Model: "test-model", Stream: &stream}
	var info *relaycommon.RelayInfo
	switch format.format {
	case types.RelayFormatClaude:
		info = relaycommon.GenRelayInfoClaude(c, request)
	case types.RelayFormatGemini:
		info = relaycommon.GenRelayInfoGemini(c, request)
	default:
		info = relaycommon.GenRelayInfoOpenAI(c, request)
	}
	info.InitChannelMeta(c)
	info.DisablePing = true
	info.BeginAttempt()
	relaycommon.InstallRelayResponseWriter(c)
	return c, w, info
}

func observationSSE(chunks ...string) string {
	return "data: " + strings.Join(chunks, "\n\ndata: ") + "\n\ndata: [DONE]\n\n"
}

func observationResponse(body string, stream bool) *http.Response {
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// Keep wire/usage snapshots in verbose logs so pre-hook and fixed runs can be
// compared byte-for-byte, including conversions that intentionally omit errors.
func logObservationWire(t *testing.T, w *httptest.ResponseRecorder, usage *dto.Usage) {
	t.Helper()
	snapshot, err := common.Marshal(struct {
		Status int         `json:"status"`
		Header http.Header `json:"header"`
		Body   string      `json:"body"`
		Usage  *dto.Usage  `json:"usage"`
	}{w.Code, w.Header(), w.Body.String(), usage})
	require.NoError(t, err)
	t.Logf("wire_snapshot=%s", snapshot)
}

func TestOaiStreamHandlerProtocolErrors(t *testing.T) {
	errorWithUsage := strings.TrimSuffix(observationError, "}") + "," + observationUsage + "}"
	for _, format := range observationFormats() {
		for _, tc := range []struct {
			name     string
			chunks   []string
			hasUsage bool
		}{
			{"only", []string{observationError}, false},
			{"first", []string{observationError, observationPartial, observationStop}, true},
			{"middle", []string{observationPartial, observationError, observationStop}, true},
			{"final", []string{observationPartial, observationError}, false},
			{"final-usage", []string{observationPartial, errorWithUsage}, true},
		} {
			for _, includeUsage := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/usage=%t", format.name, tc.name, includeUsage), func(t *testing.T) {
					c, w, info := newObservedAnswerContext(t, format, true)
					info.ShouldIncludeUsage = includeUsage
					usage, apiErr := OaiStreamHandler(c, info, observationResponse(observationSSE(tc.chunks...), true))
					logObservationWire(t, w, usage)
					require.Nil(t, apiErr, "feedback must not introduce a retryable error after streaming")
					require.NotNil(t, usage)
					require.Equal(t, 7, usage.PromptTokens)
					if tc.hasUsage {
						require.Equal(t, 3, usage.CompletionTokens)
						require.Equal(t, 10, usage.TotalTokens)
					}
					require.Equal(t, http.StatusOK, w.Code)
					require.False(t, info.StreamStatus.HasErrors(), "generic OpenAI stream status behavior is unchanged")
					require.False(t, info.ClientDeliveryBroken())
					if format.format == types.RelayFormatOpenAI {
						require.Equal(t, 1, strings.Count(w.Body.String(), "[DONE]"))
					}
					if format.force || format.thinking || format.format != types.RelayFormatOpenAI {
						require.NotContains(t, w.Body.String(), `"error"`, "preserve the existing lossy conversion")
					}
					unusable, reason := info.ClientAnswerUnusable()
					require.True(t, unusable, "explicit protocol error was lost; reason=%s", reason)
				})
			}
		}
	}
}

func TestOaiStreamHandlerEscapedProtocolErrorOverridesContent(t *testing.T) {
	const escapedError = `{"\u0065rror":{"message":"upstream failed"}}`
	for _, format := range observationFormats()[1:4] {
		t.Run(format.name, func(t *testing.T) {
			c, w, info := newObservedAnswerContext(t, format, true)
			usage, apiErr := OaiStreamHandler(c, info,
				observationResponse(observationSSE(observationPartial, escapedError, observationStop), true))
			logObservationWire(t, w, usage)
			require.Nil(t, apiErr)
			require.Equal(t, 10, usage.TotalTokens)
			require.Contains(t, w.Body.String(), "partial")
			require.NotContains(t, w.Body.String(), "upstream failed", "preserve the lossy conversion")
			unusable, reason := info.ClientAnswerUnusable()
			require.True(t, unusable, "escaped protocol error must override partial content: %s", reason)
		})
	}
}

func TestOpenaiHandlersKeepHealthyAnswersUsable(t *testing.T) {
	for _, format := range observationFormats() {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, body, extra string
			}{
				{"text", `{"content":"answer"}`, ""},
				{"reasoning", `{"reasoning_content":"thinking"}`, ""},
				{"tools", `{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, ""},
				{"quoted error", `{"content":"{\"error\":{\"message\":\"example\"}}"}`, ""},
				{"null error", `{"content":"answer"}`, `,"error":null`},
				{"empty error", `{"content":"answer"}`, `,"error":{}`},
				{"success code", `{"content":"answer"}`, `,"error":{"code":200}`},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", format.name, stream, tc.name), func(t *testing.T) {
					c, w, info := newObservedAnswerContext(t, format, stream)
					key := "message"
					if stream {
						key = "delta"
					}
					body := `{"id":"test","model":"test-model","created":1,"choices":[{"index":0,"` + key + `":` + tc.body + `,"finish_reason":"stop"}],` + observationUsage + tc.extra + `}`
					var usage *dto.Usage
					var apiErr *types.NewAPIError
					if stream {
						usage, apiErr = OaiStreamHandler(c, info, observationResponse(observationSSE(body), true))
					} else {
						usage, apiErr = OpenaiHandler(c, info, observationResponse(body, false))
					}
					logObservationWire(t, w, usage)
					require.Nil(t, apiErr)
					require.Equal(t, 10, usage.TotalTokens)
					unusable, reason := info.ClientAnswerUnusable()
					require.False(t, unusable, "healthy control must not be penalized: %s", reason)
				})
			}
		}
	}
}

func TestOaiStreamHandlerIgnoresErrorMetadata(t *testing.T) {
	for _, value := range []string{`null`, `{}`, `""`, `false`, `true`, `[]`, `{"request_id":"req-1"}`, `{"code":"ok"}`} {
		t.Run(value, func(t *testing.T) {
			c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatOpenAI}, true)
			body := `{"error":` + value + `}`
			usage, apiErr := OaiStreamHandler(c, info, observationResponse(observationSSE(body), true))
			logObservationWire(t, w, usage)
			require.Nil(t, apiErr)
			require.Equal(t, observationSSE(body), w.Body.String())
			unusable, reason := info.ClientAnswerUnusable()
			require.False(t, unusable, "metadata is not an explicit error: %s", reason)
		})
	}
}

func TestOpenaiHandlersDoNotCreditUnsentChoices(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatOpenAI}, stream)
			body := `{"choices":[{"message":{"content":"not delivered"},"finish_reason":"stop"}],` + observationUsage + `}`
			emptyBody := strings.ReplaceAll(body, "not delivered", "")
			var usage *dto.Usage
			var apiErr *types.NewAPIError
			if stream {
				body = strings.ReplaceAll(body, `"message"`, `"delta"`)
				emptyBody = strings.ReplaceAll(emptyBody, `"message"`, `"delta"`)
				usage, apiErr = OaiStreamHandlerWithDataTransformer(c, info, observationResponse(observationSSE(body), true),
					func(string) (string, error) { return emptyBody, nil })
			} else {
				usage, apiErr = OpenaiHandlerWithBodyTransformer(c, info, observationResponse(body, false),
					func([]byte) ([]byte, error) { return []byte(emptyBody), nil })
			}
			logObservationWire(t, w, usage)
			require.Nil(t, apiErr)
			require.Equal(t, 10, usage.TotalTokens)
			require.NotContains(t, w.Body.String(), "not delivered")
			unusable, reason := info.ClientAnswerUnusable()
			require.True(t, unusable, "raw choices must not override the empty delivered answer: %s", reason)
		})
	}
}

func TestOaiStreamHandlerProtocolErrorWire(t *testing.T) {
	formattedError := `{"id":"","object":"","created":0,"model":"","system_fingerprint":null,"choices":null,"usage":null}`
	for _, format := range observationFormats()[:4] {
		t.Run(format.name, func(t *testing.T) {
			c, w, info := newObservedAnswerContext(t, format, true)
			usage, apiErr := OaiStreamHandler(c, info, observationResponse(observationSSE(observationError), true))
			require.Nil(t, apiErr)
			require.Zero(t, usage.CompletionTokens)
			expected := observationError
			if format.force || format.thinking {
				expected = formattedError
			}
			require.Equal(t, observationSSE(expected), w.Body.String())
		})
	}
	t.Run("suppressed usage error", func(t *testing.T) {
		c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatOpenAI}, true)
		body := strings.TrimSuffix(observationError, "}") + "," + observationUsage + "}"
		usage, apiErr := OaiStreamHandler(c, info, observationResponse(observationSSE(body), true))
		require.Nil(t, apiErr)
		require.Equal(t, 10, usage.TotalTokens)
		require.Equal(t, "data: [DONE]\n\n", w.Body.String())
		unusable, _ := info.ClientAnswerUnusable()
		require.True(t, unusable)
	})
	t.Run("hidden details", func(t *testing.T) {
		c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatOpenAI}, true)
		common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ShowErrorDetails: false})
		usage, apiErr := OaiStreamHandler(c, info, observationResponse(observationSSE(observationError), true))
		logObservationWire(t, w, usage)
		require.Nil(t, apiErr)
		require.NotContains(t, w.Body.String(), "upstream failed")
		require.Contains(t, w.Body.String(), `"message":"overloaded"`)
		unusable, _ := info.ClientAnswerUnusable()
		require.True(t, unusable)
	})
}

func TestOpenaiHandlersKeepProviderErrorPolicy(t *testing.T) {
	for _, channelType := range []int{constant.ChannelTypeOpenAI, constant.ChannelTypeDeepSeek, constant.ChannelTypeKilo, constant.ChannelTypeCline} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("channel=%d/stream=%t", channelType, stream), func(t *testing.T) {
				c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatOpenAI}, stream)
				info.ChannelType = channelType
				info.ChannelSetting.ForceFormat = channelType == constant.ChannelTypeDeepSeek
				if stream {
					usage, apiErr := OaiStreamHandler(c, info, observationResponse(observationSSE(observationError), true))
					logObservationWire(t, w, usage)
					require.Nil(t, apiErr, "never add a retryable error after streaming")
					require.NotNil(t, usage)
					require.Equal(t, channelType != constant.ChannelTypeOpenAI, info.StreamStatus.HasErrors())
					require.Equal(t, channelType == constant.ChannelTypeOpenAI, strings.Contains(w.Body.String(), "[DONE]"))
					require.Contains(t, w.Body.String(), `"error"`)
				} else {
					usage, apiErr := OpenaiHandler(c, info, observationResponse(observationError, false))
					require.Nil(t, usage)
					require.NotNil(t, apiErr)
					status := http.StatusOK
					if channelType == constant.ChannelTypeKilo || channelType == constant.ChannelTypeCline {
						status = http.StatusBadGateway
					}
					require.Equal(t, status, apiErr.StatusCode)
					require.Empty(t, w.Body.String())
				}
			})
		}
	}
}

func TestOpenaiHandlerProtocolErrors(t *testing.T) {
	for _, format := range observationFormats() {
		for _, tc := range []struct {
			name, value string
		}{
			{"message without type", `{"message":"upstream failed"}`},
			{"string code without type", `{"code":"overloaded"}`},
			{"numeric code without type", `{"code":429}`},
		} {
			t.Run(format.name+"/"+tc.name, func(t *testing.T) {
				c, w, info := newObservedAnswerContext(t, format, false)
				body := `{"error":` + tc.value + `,` + observationUsage + `}`
				usage, apiErr := OpenaiHandler(c, info, observationResponse(body, false))
				logObservationWire(t, w, usage)
				require.Nil(t, apiErr, "preserve the existing no-type error return policy")
				require.Equal(t, 10, usage.TotalTokens)
				require.Equal(t, http.StatusOK, w.Code)
				require.False(t, info.ClientDeliveryBroken())
				if format.format != types.RelayFormatOpenAI {
					require.NotContains(t, w.Body.String(), `"error"`, "preserve the existing conversion")
				} else if !format.force {
					require.Equal(t, body, w.Body.String(), "passthrough bytes must not change")
				}
				unusable, reason := info.ClientAnswerUnusable()
				require.True(t, unusable, "explicit error was lost in body conversion: %s", reason)
			})
		}
	}
}

func TestOpenaiHandlerProtocolErrorTransformer(t *testing.T) {
	protocolError := `{"error":{"message":"upstream failed"},` + observationUsage + `}`
	for _, tc := range []struct {
		name, input, output string
	}{
		{"erased raw error", protocolError, `{` + observationUsage + `}`},
		{"exposed wrapped error", `{"wrapped":` + protocolError + `}`, protocolError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatClaude}, false)
			usage, apiErr := OpenaiHandlerWithBodyTransformer(c, info, observationResponse(tc.input, false),
				func(data []byte) ([]byte, error) {
					require.Equal(t, tc.input, string(data))
					return []byte(tc.output), nil
				})
			logObservationWire(t, w, usage)
			require.Nil(t, apiErr)
			require.Equal(t, 10, usage.TotalTokens)
			require.NotContains(t, w.Body.String(), `"error"`)
			unusable, reason := info.ClientAnswerUnusable()
			require.True(t, unusable, "raw/transformed error observation is missing: %s", reason)
		})
	}
}

func TestOaiStreamHandlerProtocolErrorTransformer(t *testing.T) {
	for _, tc := range []struct {
		name, input, output string
	}{
		{"erased raw error", observationError, `{}`},
		{"exposed wrapped error", `{"wrapped":` + observationError + `}`, observationError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatOpenAI, force: true}, true)
			usage, apiErr := OaiStreamHandlerWithDataTransformer(c, info,
				observationResponse(observationSSE(observationPartial, tc.input, observationStop), true),
				func(data string) (string, error) {
					if data == tc.input {
						return tc.output, nil
					}
					return data, nil
				})
			logObservationWire(t, w, usage)
			require.Nil(t, apiErr)
			require.Equal(t, 10, usage.TotalTokens)
			require.NotContains(t, w.Body.String(), `"error"`)
			unusable, reason := info.ClientAnswerUnusable()
			require.True(t, unusable, "raw/transformed error observation is missing: %s", reason)
		})
	}
}
