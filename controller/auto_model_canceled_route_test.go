package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type canceledRouteRoundTripper func(*http.Request) (*http.Response, error)

func (f canceledRouteRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

// Exercise Relay's error/cancellation branch rather than just its feedback
// helpers. A client's patience is not an upstream failure signal, even when a
// dispatched request has exceeded 20 seconds and produced no response bytes.
func TestRelayCanceledAttemptDoesNotCoolHistoricallySlowSuccessfulRoute(t *testing.T) {
	db, _, channel := setupUsageLogGateway(t, "http://upstream.fixture")
	resetAutoModelCooldownState(db)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	modelName := "slow-canceled-fixture"
	channel.Models = modelName
	require.NoError(t, channel.Update())
	require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"slow-canceled-fixture":0}`))
	oldAuto := operation_setting.AutoModelEnabled
	oldCandidates := operation_setting.AutoModelCandidatesToJSONString()
	operation_setting.AutoModelEnabled = true
	require.NoError(t, operation_setting.SetAutoModelCandidates(`{"default":["slow-canceled-fixture"]}`))
	t.Cleanup(func() {
		operation_setting.AutoModelEnabled = oldAuto
		_ = operation_setting.SetAutoModelCandidates(oldCandidates)
	})
	_, err := service.ClearAutoModelNegativeEffects("default", modelName, channel.Id)
	require.NoError(t, err)
	service.RecordAutoModelOutcomeWithMode("default", modelName, channel.Id, true, true, true,
		75000, 150000, "")
	defer service.ClearAutoModelNegativeEffects("default", modelName, channel.Id)

	var relayContext *gin.Context
	engine := gin.New()
	engine.Use(middleware.RequestId(), middleware.BodyStorageCleanup(), middleware.TokenAuth(), middleware.Distribute())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		relayContext = c
		Relay(c, types.RelayFormatOpenAI)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hello"}],"stream":true}`))
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+typeSafeGatewayKey)

	client := service.GetHttpClient()
	oldTransport := client.Transport
	t.Cleanup(func() { client.Transport = oldTransport })
	calls := 0
	client.Transport = canceledRouteRoundTripper(func(upstreamRequest *http.Request) (*http.Response, error) {
		calls++
		info := relaycommon.GetRelayInfo(relayContext)
		require.NotNil(t, info)
		require.True(t, info.HasUpstreamDispatch(), "cancellation must happen after actual dispatch")
		// Simulate the incident's long zero-byte wait without a wall-clock sleep.
		info.AttemptStartTime = time.Now().Add(-120 * time.Second)
		info.AttemptDispatchTime = info.AttemptStartTime
		cancel()
		return nil, upstreamRequest.Context().Err()
	})
	engine.ServeHTTP(httptest.NewRecorder(), request)
	require.Equal(t, 1, calls, "a canceled client must not start another upstream request")
	_, cooling := service.GetAutoModelCooldown("default", modelName, channel.Id, time.Now())
	require.False(t, cooling, "client cancellation must not turn a proven slow route into a failed route")
	rows, err := service.ListAutoModelScores(time.Now(), true)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.False(t, rows[0].PendingRecovery)
	require.InDelta(t, 1, rows[0].Reliability, 0.001)
	require.InDelta(t, 1, rows[0].Observations, 0.001, "cancellation must not add failure evidence")
	require.InDelta(t, 1, rows[0].StreamLatencySamples, 0.001, "the successful latency sample must survive cancellation")
	require.InDelta(t, 75000, rows[0].StreamLatencyMS, 1)
	require.Empty(t, service.GetAutoModelHardExcludedChannelIDs(relayContext))
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}
