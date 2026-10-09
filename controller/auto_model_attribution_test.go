package controller

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newAutoModelAttributionContext(t *testing.T, upstreamURL, effort string) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	settings := model_setting.GetGlobalSettings()
	saved := *settings
	t.Cleanup(func() { *settings = saved })
	settings.PassThroughRequestEnabled = false
	settings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{}

	body, err := common.Marshal(map[string]any{
		"model": "gpt-5.4", "reasoning_effort": effort,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	require.NoError(t, err)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, t.Name())
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-5.4")
	common.SetContextKey(c, constant.ContextKeyChannelId, 970101)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, upstreamURL)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "fixture-only")
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
	request, apiErr := helper.GetAndValidateRequest(c, types.RelayFormatOpenAI)
	require.Nil(t, apiErr)
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatOpenAI, request, nil)
	require.NoError(t, err)
	info.DisablePing = true
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	return c, info
}

// Keep this regression on pre-existing APIs: it must fail on a queued outcome,
// not fail to compile because the dispatch lifecycle marker does not exist yet.
func TestAutoModelLocalValidationDoesNotQueueFeedback(t *testing.T) {
	for _, unusable := range []bool{false, true} {
		name := "failure"
		if unusable {
			name = "unusable"
		}
		t.Run(name, func(t *testing.T) {
			openAutoModelRouteControllerTestDB(t)
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer upstream.Close()
			c, info := newAutoModelAttributionContext(t, upstream.URL, "max")
			info.BeginAttempt()
			apiErr := relay.TextHelper(c, info)
			info.EndAttempt()
			require.NotNil(t, apiErr)
			require.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
			require.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())
			require.Contains(t, apiErr.Error(), `does not support reasoning_effort="max"`)
			require.Zero(t, calls.Load(), "the real GPT validator must reject before the upstream is contacted")
			require.Equal(t, "max", info.Request.(*dto.GeneralOpenAIRequest).ReasoningEffort)

			if unusable {
				recordAutoModelUnusableAnswer(c, info, "no upstream answer")
			} else {
				recordAutoModelFeedback(c, info, false)
			}
			if pending, _ := c.Get(autoModelFeedbackContextKey); pending != nil {
				t.Error("local validation queued auto health feedback without upstream dispatch")
			}
			flushAutoModelFeedback(c)
			rows, err := model.GetAutoModelCooldowns()
			require.NoError(t, err)
			require.Empty(t, rows, "a never-dispatched local error must not create a cooldown")
			require.Empty(t, service.GetAutoModelCoolingChannelIDs(info.UsingGroup, info.OriginModelName))
		})
	}
}

func TestAutoModelRealUpstreamFailuresKeepFeedback(t *testing.T) {
	for _, refused := range []bool{false, true} {
		name := "upstream_error"
		if refused {
			name = "connection_refused"
		}
		t.Run(name, func(t *testing.T) {
			openAutoModelRouteControllerTestDB(t)
			var calls atomic.Int64
			captured := make(chan map[string]any, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var request map[string]any
				if err := common.DecodeJson(r.Body, &request); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				captured <- request
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":{"type":"upstream_error","message":"fixture unavailable"}}`)
			}))
			defer upstream.Close()
			upstreamURL := upstream.URL
			if refused {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				require.NoError(t, err)
				upstreamURL = "http://" + listener.Addr().String()
				require.NoError(t, listener.Close())
			}
			c, info := newAutoModelAttributionContext(t, upstreamURL, "high")
			connectErrors := make(chan error, 1)
			if refused {
				trace := &httptrace.ClientTrace{ConnectDone: func(_, _ string, err error) {
					select {
					case connectErrors <- err:
					default:
					}
				}}
				c.Request = c.Request.WithContext(httptrace.WithClientTrace(c.Request.Context(), trace))
			}
			info.BeginAttempt()
			apiErr := relay.TextHelper(c, info)
			info.EndAttempt()
			require.NotNil(t, apiErr)
			if refused {
				require.Equal(t, types.ErrorCodeDoRequestFailed, apiErr.GetErrorCode())
				select {
				case connectErr := <-connectErrors:
					require.ErrorIs(t, connectErr, syscall.ECONNREFUSED)
				default:
					t.Fatal("expected an observed failed connection, not a local validation error")
				}
				require.Zero(t, calls.Load(), "a refused connection never yields a response, but is still an upstream attempt")
			} else {
				require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
				require.Equal(t, int64(1), calls.Load())
				request := <-captured
				require.Equal(t, "gpt-5.4", request["model"])
				require.Equal(t, "high", request["reasoning_effort"])
			}
			recordAutoModelFeedback(c, info, false)
			pending, _ := c.Get(autoModelFeedbackContextKey)
			require.NotNil(t, pending, "dispatch/network failures must still reach auto health feedback")
			flushAutoModelFeedback(c)
			rows, err := model.GetAutoModelCooldowns()
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, info.UsingGroup, rows[0].Group)
			require.Equal(t, "gpt-5.4", rows[0].Model)
			require.Equal(t, info.ChannelId, rows[0].ChannelId)
			require.Equal(t, 1, rows[0].Level)
		})
	}
}

func TestAutoModelLocalRetryDoesNotInheritUpstreamDispatch(t *testing.T) {
	openAutoModelRouteControllerTestDB(t)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"fixture unavailable"}}`)
	}))
	defer upstream.Close()
	c, info := newAutoModelAttributionContext(t, upstream.URL, "high")
	info.BeginAttempt()
	apiErr := relay.TextHelper(c, info)
	info.EndAttempt()
	require.NotNil(t, apiErr)
	require.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
	recordAutoModelFeedback(c, info, false)
	firstKey := autoModelFeedbackKey{group: info.UsingGroup, modelName: info.OriginModelName, channelID: info.ChannelId}

	// A subsequent route can fail locally. The first route's dispatch must not
	// turn validation failure on this new channel into upstream evidence.
	common.SetContextKey(c, constant.ContextKeyChannelId, 970102)
	info.Request.(*dto.GeneralOpenAIRequest).ReasoningEffort = "max"
	info.BeginAttempt()
	apiErr = relay.TextHelper(c, info)
	info.EndAttempt()
	require.NotNil(t, apiErr)
	require.Equal(t, types.ErrorCodeInvalidRequest, apiErr.GetErrorCode())
	require.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	require.Equal(t, int64(1), calls.Load(), "the retry must stop before a second upstream request")
	recordAutoModelFeedback(c, info, false)
	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 1, "only the first, dispatched attempt may contribute feedback")
	feedback, ok := pending[firstKey]
	require.True(t, ok)
	require.False(t, feedback.success)
}
