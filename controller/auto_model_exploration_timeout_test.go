package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestAutoModelExplorationTimeoutRetainsSameChannelFallback(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	oldSelfUse := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = true
	t.Cleanup(func() { operation_setting.SelfUseModeEnabled = oldSelfUse })
	channel := createAutoModelRouteTestChannel(t, db, "default", "unknown", "working")
	c, info, retry := newAutoModelRouteTestContext(t, "default", []service.AutoModelRoute{
		{Group: "default", ModelName: "unknown", ChannelID: channel.Id},
		{Group: "default", ModelName: "working", ChannelID: channel.Id},
	})
	bindInitialAutoModelRouteForTest(t, c, info)
	info.BeginAttempt()
	info.MarkUpstreamDispatch()
	markCurrentAutoModelRouteAttempted(c)
	apiErr := types.NewErrorWithStatusCode(fmt.Errorf("attempt: %w", relaycommon.ErrAutoExplorationTimeout),
		types.ErrorCodeChannelResponseTimeExceeded, http.StatusGatewayTimeout)
	require.True(t, prepareAutoModelRetry(c, info, apiErr, 1))
	require.Empty(t, service.GetAutoModelHardExcludedChannelIDs(c))
	require.False(t, service.IsAutoModelModelFailed(c, "default", "unknown"))
	require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
	require.Equal(t, channel.Id, info.ChannelId)
	require.Equal(t, "working", info.OriginModelName)
	require.False(t, prepareAutoModelRetry(c, info, apiErr, 0), "timeout cannot bypass the dispatch budget")
	skipErr := types.NewErrorWithStatusCode(relaycommon.ErrAutoExplorationTimeout,
		types.ErrorCodeChannelResponseTimeExceeded, http.StatusGatewayTimeout, types.ErrOptionWithSkipRetry())
	require.False(t, prepareAutoModelRetry(c, info, skipErr, 1), "timeout cannot bypass explicit retry guards")
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	cancel()
	require.False(t, prepareAutoModelRetry(c, info, apiErr, 1), "canceled client cannot start fallback")
}

func TestAutoModelRelayChainExplorationTimeoutCancelsOnlyAttempt(t *testing.T) {
	openAutoModelRouteControllerTestDB(t)
	canceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(canceled)
	}))
	defer upstream.Close()
	c, info, recorder := newAutoModelAttributionContextForBodyWithRecorder(t, upstream.URL, "unknown", autoModelChainPayload("unknown"))
	parentRequest := c.Request
	ctx, guard := relaycommon.NewAutoExplorationAttempt(parentRequest.Context(), 40*time.Millisecond)
	defer guard.Finish()
	info.AutoExplorationAttempt = guard
	c.Request = c.Request.WithContext(ctx)
	info.BeginAttempt()
	staged := relaycommon.InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	staged.SetExplorationAttempt(guard)
	defer staged.Restore()
	apiErr := relay.TextHelper(c, info)
	info.EndAttempt()
	require.NotNil(t, apiErr)
	require.ErrorIs(t, context.Cause(ctx), relaycommon.ErrAutoExplorationTimeout)
	guard.Finish()
	info.AutoExplorationAttempt = nil
	c.Request = parentRequest
	require.NoError(t, c.Request.Context().Err(), "fallback must retain the live client context")
	require.False(t, staged.Committed())
	require.Empty(t, recorder.Body.String())
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("timed-out speculative HTTP request kept running upstream")
	}
	apiErr = types.NewErrorWithStatusCode(relaycommon.ErrAutoExplorationTimeout,
		types.ErrorCodeChannelResponseTimeExceeded, http.StatusGatewayTimeout)
	require.True(t, prepareAutoModelRetry(c, info, apiErr, 1))
	recordAutoModelUpstreamFailure(c, info, apiErr)
	flushAutoModelFeedback(c)
	_, active := service.GetAutoModelCooldown(info.UsingGroup, info.OriginModelName, info.ChannelId, time.Now())
	require.True(t, active, "gateway timeout must publish failed-pair cooldown before fallback")
	require.True(t, errors.Is(apiErr, relaycommon.ErrAutoExplorationTimeout))
}
