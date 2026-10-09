package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Compatibility fallback needs model evidence, not just a 400/404/422 status.
func TestAutoModelCompatibilityFallbackRequiresModelEvidence(t *testing.T) {
	oldAuto := operation_setting.AutoModelEnabled
	oldCodes := operation_setting.AutomaticRetryStatusCodesToString()
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() {
		operation_setting.AutoModelEnabled = oldAuto
		require.NoError(t, operation_setting.AutomaticRetryStatusCodesFromString(oldCodes))
	})
	require.NoError(t, operation_setting.AutomaticRetryStatusCodesFromString("429"))
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 7}}
	info.MarkUpstreamDispatch()
	for _, tc := range []struct {
		status  int
		message string
		want    bool
	}{
		{http.StatusNotFound, "model not supported", true},
		{http.StatusBadRequest, "response_format unavailable", false},
		{http.StatusUnprocessableEntity, "unprocessable", false},
		{http.StatusInternalServerError, "upstream 500", false},
		{http.StatusTooManyRequests, "rate limited", true},
		{http.StatusUnauthorized, "unauthorized", false},
	} {
		c := newAutoModelFeedbackTestContext()
		err := types.NewErrorWithStatusCode(errors.New(tc.message), types.ErrorCodeGetChannelFailed, tc.status)
		require.Equal(t, tc.want, prepareAutoModelRetry(c, info, err, 1), tc.message)
	}
	require.False(t, prepareAutoModelRetry(newAutoModelFeedbackTestContext(), info, nil, 1))
}

func TestTrySwitchAutoModelSkipsUnauthorizedCandidates(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	channel := createAutoModelRouteTestChannel(t, db, "default", "old", "denied", "allowed")
	c, info, retry := newAutoModelRouteTestContext(t, "default", []service.AutoModelRoute{
		{Group: "default", ModelName: "old", ChannelID: channel.Id},
		{Group: "default", ModelName: "denied", ChannelID: channel.Id},
		{Group: "default", ModelName: "allowed", ChannelID: channel.Id},
	})
	common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"old": true, "allowed": true})
	bindInitialAutoModelRouteForTest(t, c, info)
	c.Set("use_channel", []string{"9"})
	require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
	require.Equal(t, "allowed", info.OriginModelName)
	require.Equal(t, "allowed", info.ClientModelName)
	require.Equal(t, "allowed", retry.ModelName)
	require.Equal(t, "allowed", c.GetString("original_model"))
	require.Equal(t, 2, common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex))
	require.Equal(t, []string{"9"}, c.GetStringSlice("use_channel"), "route switches must not erase channel history")
}

func TestTrySwitchAutoModelRejectsUnauthorizedOrInvalidSnapshotWithoutMutation(t *testing.T) {
	oldEnabled := operation_setting.AutoModelEnabled
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = oldEnabled })
	for _, index := range []int{0, -1, 2} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
		service.SetAutoModelRoutePlan(c, []service.AutoModelRoute{
			{Group: "default", ModelName: "old", ChannelID: 9},
			{Group: "default", ModelName: "denied", ChannelID: 9},
		})
		common.SetContextKey(c, constant.ContextKeyAutoModelIndex, index)
		common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
		common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"old": true, "auto": true})
		c.Set("original_model", "old")
		c.Set("use_channel", []string{"9"})
		info := &relaycommon.RelayInfo{OriginModelName: "old", ClientModelName: "old", TokenGroup: "default", UsingGroup: "default"}
		retry := &service.RetryParam{Ctx: c, ModelName: "old", Retry: common.GetPointer(1)}
		require.False(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
		require.Equal(t, "old", info.OriginModelName)
		require.Equal(t, "old", info.ClientModelName)
		require.Equal(t, "old", retry.ModelName)
		require.Equal(t, "old", c.GetString("original_model"))
		require.Equal(t, index, common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex))
		require.Equal(t, []string{"9"}, c.GetStringSlice("use_channel"))
	}
}
