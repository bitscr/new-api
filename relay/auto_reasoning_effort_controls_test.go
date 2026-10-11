package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAutoReasoningEffortRetryUsesOriginal(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ first, firstEffort, second string }{
				{"gemini-3-flash-preview", "high", "gpt-free"},
				{"kimi-k3", "", "deepseek-v4-flash"},
			} {
				name := string(format) + "/" + tc.first + "/nonstream"
				if stream {
					name = string(format) + "/" + tc.first + "/stream"
				}
				t.Run(name, func(t *testing.T) {
					fields, path := `{"reasoning_effort":"max"}`, "reasoning_effort"
					if format == types.RelayFormatOpenAIResponses {
						fields, path = `{"reasoning":{"effort":"max"}}`, "reasoning.effort"
					}
					f := newAutoEffortFixture(t, format, tc.first, fields, true, stream)
					require.Equal(t, tc.firstEffort, gjson.GetBytes(f.send(t), path).String())
					// Simulate only the route binding; retain the same original DTO.
					f.info.OriginModelName, f.info.ClientModelName = tc.second, tc.second
					common.SetContextKey(f.c, constant.ContextKeyOriginalModel, tc.second)
					f.info.Request.SetModelName(tc.second)
					body := f.send(t)
					require.Equal(t, tc.second, gjson.GetBytes(body, "model").String())
					require.Equal(t, "max", gjson.GetBytes(body, path).String())
					require.Equal(t, "max", f.info.ReasoningEffort)
				})
			}
		}
	}
}

func TestAutoReasoningEffortPassthroughRetryUsesOriginal(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ first, firstEffort, second string }{
				{"gemini-3-flash-preview", "high", "gpt-free"},
				{"kimi-k3", "", "deepseek-v4-flash"},
			} {
				name := string(format) + "/" + tc.first + "/nonstream"
				if stream {
					name = string(format) + "/" + tc.first + "/stream"
				}
				t.Run(name, func(t *testing.T) {
					fields, path := `{"reasoning_effort":"max","unknown":false,"seed":0}`, "reasoning_effort"
					if format == types.RelayFormatOpenAIResponses {
						fields, path = `{"reasoning":{"effort":"max","summary":"detailed"},"unknown":false,"seed":0}`, "reasoning.effort"
					}
					f := newAutoEffortFixture(t, format, tc.first, fields, true, stream)
					common.SetContextKey(f.c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: true})
					storage, err := common.GetBodyStorage(f.c)
					require.NoError(t, err)
					t.Cleanup(func() { _ = storage.Close() })
					original, err := storage.Bytes()
					require.NoError(t, err)
					original = bytes.Clone(original)
					first := f.send(t)
					require.Equal(t, tc.first, gjson.GetBytes(first, "model").String())
					require.Equal(t, tc.firstEffort, gjson.GetBytes(first, path).String())
					if tc.firstEffort == "" {
						require.False(t, gjson.GetBytes(first, path).Exists())
					}
					require.Equal(t, tc.firstEffort, f.info.ReasoningEffort)
					retained, err := storage.Bytes()
					require.NoError(t, err)
					require.Equal(t, original, retained, "the first attempt must not adapt the retry source")

					// Simulate the controller's model-only raw-body rebind, using the
					// retained storage, not the adapted wire body or a fresh DTO.
					var payload map[string]json.RawMessage
					require.NoError(t, common.Unmarshal(retained, &payload))
					payload["model"], err = common.Marshal(tc.second)
					require.NoError(t, err)
					retryBody, err := common.Marshal(payload)
					require.NoError(t, err)
					replacement, err := common.CreateBodyStorage(retryBody)
					require.NoError(t, err)
					t.Cleanup(func() { _ = replacement.Close() })
					f.c.Set(common.KeyBodyStorage, replacement)
					f.c.Set(common.KeyRequestBody, nil)
					f.c.Set(gin.BodyBytesKey, retryBody)
					f.c.Request.Body = io.NopCloser(replacement)
					f.c.Request.ContentLength = int64(len(retryBody))
					f.c.Request.Header.Del("Content-Length")
					f.info.OriginModelName, f.info.ClientModelName = tc.second, tc.second
					common.SetContextKey(f.c, constant.ContextKeyOriginalModel, tc.second)
					f.info.Request.SetModelName(tc.second)
					second := f.send(t)
					t.Logf("first_body=%s retry_source=%s second_body=%s", first, retryBody, second)
					require.Equal(t, tc.second, gjson.GetBytes(second, "model").String())
					require.Equal(t, "max", gjson.GetBytes(second, path).String())
					require.Equal(t, "max", f.info.ReasoningEffort)
					require.Equal(t, retryBody, second, "the retry needs no effort adaptation")
					for _, body := range [][]byte{first, second} {
						require.Equal(t, "false", gjson.GetBytes(body, "unknown").Raw)
						require.Equal(t, "0", gjson.GetBytes(body, "seed").Raw)
						if format == types.RelayFormatOpenAIResponses {
							require.Equal(t, "detailed", gjson.GetBytes(body, "reasoning.summary").String())
						}
					}
				})
			}
		}
	}
}

func TestAutoReasoningEffortMappedUpstream(t *testing.T) {
	for _, tc := range []struct{ model, mapping, upstream, effort string }{
		{"gpt-public", `{"gpt-public":"hop","hop":"vendor/Qwen3.8-Flash"}`, "vendor/Qwen3.8-Flash", "xhigh"},
		{"qwen3-public", `{"qwen3-public":"deepseek-ai/deepseek-v4-flash"}`, "deepseek-ai/deepseek-v4-flash", "max"},
		{"gpt-public", `{"gpt-public":"deployment-42"}`, "deployment-42", "high"},
	} {
		t.Run(tc.upstream, func(t *testing.T) {
			f := newAutoEffortFixture(t, types.RelayFormatOpenAI, tc.model, `{"reasoning_effort":"max"}`, true, false)
			f.c.Set("model_mapping", tc.mapping)
			body := f.send(t)
			require.Equal(t, tc.upstream, gjson.GetBytes(body, "model").String())
			require.Equal(t, tc.effort, gjson.GetBytes(body, "reasoning_effort").String())
		})
	}
}

func TestAutoReasoningEffortNamedAndMissingControls(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatOpenAIResponses} {
		for _, raw := range []bool{false, true} {
			for _, named := range []bool{false, true} {
				for _, modelName := range []string{"qwen3.8-flash", "kimi-k3"} {
					t.Run(string(format)+"/"+modelName, func(t *testing.T) {
						fields, path, want := `{}`, "reasoning_effort", ""
						if named {
							fields, want = `{"reasoning_effort":"max"}`, "max"
						}
						if format == types.RelayFormatOpenAIResponses {
							path = "reasoning.effort"
							if named {
								fields = `{"reasoning":{"effort":"max"}}`
							}
						}
						f := newAutoEffortFixture(t, format, modelName, fields, !named, false)
						common.SetContextKey(f.c, constant.ContextKeyChannelSetting, dto.ChannelSettings{PassThroughBodyEnabled: raw})
						body := f.send(t)
						require.Equal(t, want, gjson.GetBytes(body, path).String())
						if !named {
							require.False(t, gjson.GetBytes(body, path).Exists())
						}
						if named && raw {
							storage, err := common.GetBodyStorage(f.c)
							require.NoError(t, err)
							original, err := storage.Bytes()
							require.NoError(t, err)
							require.Equal(t, original, body)
						}
					})
				}
			}
		}
	}
}

func TestAutoReasoningEffortProviderSuffixPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, model, upstream, fields, path, inputPath, inputEffort, want, urlPath string
		format                                                                     types.RelayFormat
		channel                                                                    int
	}{
		{"openai", "gpt-6-luna-low", "gpt-6-luna", `{"reasoning_effort":"max"}`, "reasoning_effort", "reasoning_effort", "max", "low", "/v1/chat/completions", types.RelayFormatOpenAI, constant.ChannelTypeOpenAI},
		{"claude", "claude-opus-4-6-low", "claude-opus-4-6", `{"output_config":{"effort":"max"}}`, "output_config.effort", "output_config.effort", "max", "low", "/v1/messages", types.RelayFormatClaude, constant.ChannelTypeAnthropic},
		{"gemini", "gemini-3.1-pro-low", "gemini-3.1-pro", `{"reasoning_effort":"max"}`, "generationConfig.thinkingConfig.thinkingLevel", "reasoning_effort", "max", "low", "/v1beta/models/gemini-3.1-pro:generateContent", types.RelayFormatOpenAI, constant.ChannelTypeGemini},
		{"deepseek", "deepseek-v4-flash-max", "deepseek-v4-flash", `{"reasoning_effort":"low"}`, "reasoning_effort", "reasoning_effort", "low", "max", "/chat/completions", types.RelayFormatOpenAI, constant.ChannelTypeDeepSeek},
	} {
		for _, auto := range []bool{false, true} {
			for _, override := range []bool{false, true} {
				for _, stream := range []bool{false, true} {
					name := tc.name + "/named"
					if auto {
						name = tc.name + "/auto"
					}
					if override {
						name += "/override"
					} else {
						name += "/suffix"
					}
					if stream {
						name += "/stream"
					} else {
						name += "/nonstream"
					}
					t.Run(name, func(t *testing.T) {
						f := newAutoEffortFixture(t, tc.format, tc.model, tc.fields, auto, stream)
						common.SetContextKey(f.c, constant.ContextKeyChannelType, tc.channel)
						model_setting.GetGlobalSettings().ThinkingModelBlacklist = nil
						if tc.channel == constant.ChannelTypeGemini {
							settings := model_setting.GetGeminiSettings()
							saved := settings.ThinkingAdapterEnabled
							t.Cleanup(func() { settings.ThinkingAdapterEnabled = saved })
							settings.ThinkingAdapterEnabled = true
						}
						want := tc.want
						if override {
							common.SetContextKey(f.c, constant.ContextKeyChannelParamOverride, map[string]any{
								"operations": []any{map[string]any{"path": tc.path, "mode": "set", "value": "medium"}},
							})
							want = "medium"
						}
						baseURL, capturedURL := captureAutoEffortURL(t, f)
						body := f.send(t)
						var upstreamURL string
						select {
						case upstreamURL = <-capturedURL:
						default:
							t.Fatal("request did not reach the suffix URL capture fixture")
						}
						wantPath := tc.urlPath
						if stream && tc.channel == constant.ChannelTypeGemini {
							wantPath = "/v1beta/models/" + tc.upstream + ":streamGenerateContent?alt=sse"
						}
						t.Logf("upstream_url=%s upstream_body=%s", upstreamURL, body)
						require.Equal(t, baseURL+wantPath, upstreamURL)
						require.Equal(t, tc.upstream, f.info.UpstreamModelName)
						if tc.channel != constant.ChannelTypeGemini {
							require.Equal(t, tc.upstream, gjson.GetBytes(body, "model").String())
						}
						require.Equal(t, want, gjson.GetBytes(body, tc.path).String())
						require.Equal(t, want, f.info.ReasoningEffort, "metadata must reflect the final wire effort")
						original, err := common.Marshal(f.info.Request)
						require.NoError(t, err)
						require.Equal(t, tc.inputEffort, gjson.GetBytes(original, tc.inputPath).String())
					})
				}
			}
		}
	}
}

func TestAutoReasoningEffortChatToResponsesAndOverrides(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "mapped", true: "explicit override"}[override], func(t *testing.T) {
			f := newAutoEffortFixture(t, types.RelayFormatOpenAI, "qwen3.8-flash", `{"reasoning_effort":"max"}`, true, false)
			settings := model_setting.GetGlobalSettings()
			settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: true, AllChannels: true, ModelPatterns: []string{".*"}}
			want := "xhigh"
			if override {
				common.SetContextKey(f.c, constant.ContextKeyChannelParamOverride, map[string]any{"reasoning_effort": "low"})
				want = "low"
			}
			body := f.send(t)
			require.Equal(t, want, gjson.GetBytes(body, "reasoning.effort").String())
			require.False(t, gjson.GetBytes(body, "reasoning_effort").Exists())
			require.True(t, gjson.GetBytes(body, "input").Exists())
			require.Equal(t, want, f.info.ReasoningEffort)
		})
	}
}
