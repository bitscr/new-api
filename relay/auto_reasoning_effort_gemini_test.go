package relay

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func captureAutoEffortURL(t *testing.T, f *autoEffortFixture) (string, <-chan string) {
	t.Helper()
	capturedURL := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "fixture read failed", http.StatusInternalServerError)
			return
		}
		capturedURL <- "http://" + r.Host + r.URL.RequestURI()
		f.captured <- body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"effort fixture captured"}}`)
	}))
	t.Cleanup(server.Close)
	common.SetContextKey(f.c, constant.ContextKeyChannelBaseUrl, server.URL)
	return server.URL, capturedURL
}

func TestAutoReasoningEffortPassthroughSelectedAdaptor(t *testing.T) {
	const selectedModel = "gpt-public"
	const mappedModel = "gemini-3.1-pro"
	for _, route := range []struct {
		name     string
		format   types.RelayFormat
		channel  int
		urlModel bool
	}{
		{"openai to gemini", types.RelayFormatOpenAI, constant.ChannelTypeGemini, true},
		{"gemini to openai", types.RelayFormatGemini, constant.ChannelTypeOpenAI, false},
		{"openai to openai", types.RelayFormatOpenAI, constant.ChannelTypeOpenAI, false},
		{"gemini to gemini", types.RelayFormatGemini, constant.ChannelTypeGemini, true},
	} {
		for _, tc := range []struct {
			name, effort string
			auto         bool
		}{
			{"auto max", "max", true},
			{"auto non-max", "medium", true},
			{"named max", "max", false},
		} {
			for _, stream := range []bool{false, true} {
				mode, action := "nonstream", "generateContent"
				if stream {
					mode, action = "stream", "streamGenerateContent?alt=sse"
				}
				t.Run(route.name+"/"+tc.name+"/"+mode, func(t *testing.T) {
					fields := `{"reasoning_effort":"` + tc.effort + `","temperature":0,"unknown":false,"seed":0}`
					effortPath := "reasoning_effort"
					if route.format == types.RelayFormatGemini {
						fields = `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + tc.effort + `","includeThoughts":false,"thinkingBudget":0},"temperature":0},"unknown":false,"seed":0}`
						effortPath = "generationConfig.thinkingConfig.thinkingLevel"
					}
					f := newAutoEffortFixture(t, route.format, selectedModel, fields, tc.auto, stream)
					common.SetContextKey(f.c, constant.ContextKeyChannelType, route.channel)
					common.SetContextKey(f.c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: true})
					f.c.Set("model_mapping", `{"gpt-public":"gemini-3.1-pro"}`)

					baseURL, capturedURL := captureAutoEffortURL(t, f)

					storage, err := common.GetBodyStorage(f.c)
					require.NoError(t, err)
					original, err := storage.Bytes()
					require.NoError(t, err)
					original = bytes.Clone(original)
					body := f.send(t)
					var upstreamURL string
					select {
					case upstreamURL = <-capturedURL:
					default:
						t.Fatal("request did not reach the upstream URL capture fixture")
					}
					t.Logf("upstream_url=%s upstream_body=%s original_body=%s", upstreamURL, body, original)
					wantURL := baseURL + "/v1/chat/completions"
					wantEffort := tc.effort
					if route.urlModel {
						wantURL = baseURL + "/v1beta/models/" + mappedModel + ":" + action
						if tc.auto && tc.effort == "max" {
							wantEffort = "high"
						}
					}
					require.Equal(t, wantURL, upstreamURL)
					require.Equal(t, selectedModel, gjson.GetBytes(body, "model").String())
					require.Equal(t, "false", gjson.GetBytes(body, "unknown").Raw)
					require.Equal(t, "0", gjson.GetBytes(body, "seed").Raw)
					if route.format == types.RelayFormatGemini {
						require.Equal(t, "false", gjson.GetBytes(body, "generationConfig.thinkingConfig.includeThoughts").Raw)
						require.Equal(t, "0", gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingBudget").Raw)
						require.Equal(t, "0", gjson.GetBytes(body, "generationConfig.temperature").Raw)
						require.Equal(t, tc.effort, f.info.Request.(*dto.GeminiChatRequest).GenerationConfig.ThinkingConfig.ThinkingLevel)
					} else {
						require.Equal(t, "0", gjson.GetBytes(body, "temperature").Raw)
						require.Equal(t, tc.effort, f.info.Request.(*dto.GeneralOpenAIRequest).ReasoningEffort)
					}
					retained, err := storage.Bytes()
					require.NoError(t, err)
					require.Equal(t, original, retained, "BodyStorage must remain the original retry source")
					require.Equal(t, wantEffort, gjson.GetBytes(body, effortPath).String(), "effort must follow the selected upstream adaptor's model, not the ingress format")
					require.Equal(t, wantEffort, f.info.ReasoningEffort)
					if !tc.auto || tc.effort == wantEffort {
						require.Equal(t, original, body, "unadapted passthrough must remain byte-for-byte unchanged")
					}
				})
			}
		}
	}
}

func TestAutoReasoningEffortGeminiPassthroughMappedModel(t *testing.T) {
	const selectedModel = "gpt-public"
	const mappedModel = "gemini-3.1-pro-preview"
	const effortPath = "generationConfig.thinkingConfig.thinkingLevel"
	for _, tc := range []struct {
		name, effort, want string
		auto               bool
	}{
		{"auto max", "max", "high", true},
		{"auto non-max", "medium", "medium", true},
		{"named max", "max", "max", false},
	} {
		for _, stream := range []bool{false, true} {
			name, action := tc.name+"/nonstream", "generateContent"
			if stream {
				name, action = tc.name+"/stream", "streamGenerateContent?alt=sse"
			}
			t.Run(name, func(t *testing.T) {
				fields := `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"` + tc.effort + `","includeThoughts":false,"thinkingBudget":0},"temperature":0},"unknown":false,"seed":0}`
				f := newAutoEffortFixture(t, types.RelayFormatGemini, selectedModel, fields, tc.auto, stream)
				common.SetContextKey(f.c, constant.ContextKeyChannelType, constant.ChannelTypeGemini)
				common.SetContextKey(f.c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: true})
				f.c.Set("model_mapping", `{"gpt-public":"gemini-3.1-pro-preview"}`)

				// Capture the URL as well as the body through the real Gemini adaptor.
				_, capturedURL := captureAutoEffortURL(t, f)

				storage, err := common.GetBodyStorage(f.c)
				require.NoError(t, err)
				original, err := storage.Bytes()
				require.NoError(t, err)
				original = bytes.Clone(original)

				body := f.send(t)
				var upstreamURL string
				select {
				case upstreamURL = <-capturedURL:
				default:
					t.Fatal("request did not reach the Gemini URL capture fixture")
				}
				t.Logf("upstream_url=%s upstream_body=%s original_body=%s", upstreamURL, body, original)
				require.Contains(t, upstreamURL, "/models/"+mappedModel+":"+action)
				require.NotContains(t, upstreamURL, selectedModel)
				require.Equal(t, selectedModel, gjson.GetBytes(body, "model").String())
				require.Equal(t, "false", gjson.GetBytes(body, "generationConfig.thinkingConfig.includeThoughts").Raw)
				require.Equal(t, "0", gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingBudget").Raw)
				require.Equal(t, "0", gjson.GetBytes(body, "generationConfig.temperature").Raw)
				require.Equal(t, "false", gjson.GetBytes(body, "unknown").Raw)
				require.Equal(t, "0", gjson.GetBytes(body, "seed").Raw)

				retained, err := storage.Bytes()
				require.NoError(t, err)
				require.Equal(t, original, retained, "BodyStorage must remain the original retry source")
				require.Equal(t, tc.effort, gjson.GetBytes(retained, effortPath).String())
				require.Equal(t, tc.effort, f.info.Request.(*dto.GeminiChatRequest).GenerationConfig.ThinkingConfig.ThinkingLevel)
				if !tc.auto || tc.effort == tc.want {
					require.Equal(t, original, body, "unadapted passthrough must remain byte-for-byte unchanged")
				}
				require.Equal(t, tc.want, gjson.GetBytes(body, effortPath).String(), "native Gemini effort must follow the model in the upstream URL")
				require.Equal(t, tc.want, f.info.ReasoningEffort)
			})
		}
	}
}
