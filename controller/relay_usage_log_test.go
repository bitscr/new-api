package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const usageLogErrorStream = "data: {\"error\":{\"message\":\"synthetic upstream failure\",\"type\":\"upstream_error\",\"code\":\"fixture_error\"}}\n\ndata: [DONE]\n\n"
const usageLogRequest = `{"model":"jev-latest","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":8}`

type brokenUsageLogWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (w *brokenUsageLogWriter) Write(payload []byte) (int, error) {
	w.writes++
	return 0, io.ErrClosedPipe
}

func setupUsageLogGateway(t *testing.T, upstream string) (*gorm.DB, *gin.Engine, *model.Channel) {
	t.Helper()
	db, _, channel := setupTypeSafeGateway(t, upstream, "upstream-fixture")
	channel.Type = constant.ChannelTypeOpenAI
	require.NoError(t, channel.Update())
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"jev-latest":0}`))
	previous, previousTimeout := constant.ErrorLogEnabled, constant.StreamingTimeout
	constant.ErrorLogEnabled, constant.StreamingTimeout = true, 3
	t.Cleanup(func() {
		constant.ErrorLogEnabled, constant.StreamingTimeout = previous, previousTimeout
	})
	engine := gin.New()
	engine.Use(middleware.RequestId(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.ModelRequestRateLimit(), middleware.Distribute())
	engine.POST("/v1/chat/completions", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
	return db, engine, channel
}

func TestRelayUnusableAnswerWritesUsageError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, usageLogErrorStream)
	}))
	defer upstream.Close()
	db, engine, channel := setupUsageLogGateway(t, upstream.URL)
	response := requestTypeSafe(t, engine, "/v1/chat/completions", usageLogRequest, typeSafeGatewayKey)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "fixture_error")
	require.EqualValues(t, 1, calls.Load(), "an already delivered error must not be retried")
	var entries []model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeError).Find(&entries).Error)
	require.Len(t, entries, 1, "a non-auto HTTP200 error must appear in usage logs")
	entry := entries[0]
	require.Equal(t, 1, entry.UserId)
	require.Equal(t, 1, entry.TokenId)
	require.Equal(t, "typesafe-test", entry.TokenName)
	require.Equal(t, "jev-latest", entry.ModelName)
	require.Equal(t, channel.Id, entry.ChannelId)
	require.Equal(t, "default", entry.Group)
	require.True(t, entry.IsStream)
	require.NotEmpty(t, entry.RequestId)
	require.Equal(t, response.Header().Get(common.RequestIdKey), entry.RequestId)
	require.NotEmpty(t, entry.Content)
	require.Zero(t, entry.Quota)
	require.Zero(t, entry.PromptTokens)
	require.Zero(t, entry.CompletionTokens)
	require.Equal(t, "192.0.2.1", entry.Ip, "preserve the existing IP logging policy")
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(entry.Other, &other))
	require.Equal(t, float64(http.StatusOK), other["status_code"])
	require.Equal(t, string(types.ErrorCodeBadResponse), other["error_code"])
	require.Equal(t, "/v1/chat/completions", other["request_path"])
	assertTypeSafeQuota(t, db, 0)
	require.NoError(t, db.First(channel, channel.Id).Error)
	require.Equal(t, common.ChannelStatusEnabled, channel.Status, "logging must not ban a channel")
}

func TestRelayUsageLogControls(t *testing.T) {
	for _, test := range []struct {
		name, payload    string
		disabled, broken bool
	}{
		{name: "healthy", payload: "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"},
		{name: "null_error", payload: "data: {\"error\":null,\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"},
		{name: "unknown_error", payload: "data: {\"error\":{\"metadata\":\"info\"},\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"},
		{name: "disabled_logging", payload: usageLogErrorStream, disabled: true},
		{name: "broken_delivery", payload: usageLogErrorStream, broken: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.payload)
			}))
			defer upstream.Close()
			db, engine, _ := setupUsageLogGateway(t, upstream.URL)
			constant.ErrorLogEnabled = !test.disabled
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(usageLogRequest))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+typeSafeGatewayKey)
			response := httptest.NewRecorder()
			broken := &brokenUsageLogWriter{ResponseRecorder: response}
			if test.broken {
				engine.ServeHTTP(broken, request)
				require.Positive(t, broken.writes, "the failing delivery must really be attempted")
			} else {
				engine.ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				require.NotEmpty(t, response.Body.String())
			}
			require.EqualValues(t, 1, calls.Load())
			var count int64
			require.NoError(t, db.Model(&model.Log{}).Where("type = ?", model.LogTypeError).Count(&count).Error)
			require.Zero(t, count)
		})
	}
}

func TestRelayUsageLogOrdinaryErrorRemainsSingle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"ordinary rejection","type":"invalid_request_error","code":"fixture_error"}}`)
	}))
	defer upstream.Close()
	db, engine, _ := setupUsageLogGateway(t, upstream.URL)
	response := requestTypeSafe(t, engine, "/v1/chat/completions", usageLogRequest, typeSafeGatewayKey)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	var entries []model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeError).Find(&entries).Error)
	require.Len(t, entries, 1)
	require.Contains(t, entries[0].Content, "ordinary rejection")
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(entries[0].Other, &other))
	require.Equal(t, float64(http.StatusBadRequest), other["status_code"])
	require.Equal(t, "fixture_error", other["error_code"])
	assertTypeSafeQuota(t, db, 0)
}

func TestRelayUsageLogRespectsNoRecordOption(t *testing.T) {
	db, _, channel := setupUsageLogGateway(t, "http://127.0.0.1")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set("id", 1)
	c.Set("token_id", 1)
	c.Set("channel_id", channel.Id)
	c.Set("original_model", "jev-latest")
	err := types.NewError(io.ErrUnexpectedEOF, types.ErrorCodeBadResponse, types.ErrOptionWithNoRecordErrorLog())
	processChannelError(c, *types.NewChannelError(channel.Id, channel.Type, channel.Name, false, "", false), err)
	var count int64
	require.NoError(t, db.Model(&model.Log{}).Where("type = ?", model.LogTypeError).Count(&count).Error)
	require.Zero(t, count, "existing deliberately unrecorded errors must stay unrecorded")
}

func TestRelayUnusableNonstreamWritesUsageError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"message":"synthetic nonstream failure"}}`)
	}))
	defer upstream.Close()
	db, engine, _ := setupUsageLogGateway(t, upstream.URL)
	request := strings.Replace(usageLogRequest, `"stream":true`, `"stream":false`, 1)
	response := requestTypeSafe(t, engine, "/v1/chat/completions", request, typeSafeGatewayKey)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var entries []model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeError).Find(&entries).Error)
	require.Len(t, entries, 1)
	require.False(t, entries[0].IsStream)
	require.Contains(t, entries[0].Content, "显式协议错误")
	assertTypeSafeQuota(t, db, 0)
}
