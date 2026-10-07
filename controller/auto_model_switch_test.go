package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// auto 遇到"这个渠道服务不了这个候选"的错误要换候选，而不是把 404/400 甩给客户端。
func TestAutoModelSwitchableError(t *testing.T) {
	require.True(t, autoModelSwitchableError(types.NewErrorWithStatusCode(
		errors.New("model not supported"), types.ErrorCodeGetChannelFailed, http.StatusNotFound)))
	require.True(t, autoModelSwitchableError(types.NewErrorWithStatusCode(
		errors.New("response_format unavailable"), types.ErrorCodeGetChannelFailed, http.StatusBadRequest)))
	require.True(t, autoModelSwitchableError(types.NewErrorWithStatusCode(
		errors.New("unprocessable"), types.ErrorCodeGetChannelFailed, http.StatusUnprocessableEntity)))

	// 这些不该触发换候选：服务端故障走正常重试，客户端问题不该放大成多次上游调用
	require.False(t, autoModelSwitchableError(nil))
	require.False(t, autoModelSwitchableError(types.NewErrorWithStatusCode(
		errors.New("upstream 500"), types.ErrorCodeGetChannelFailed, http.StatusInternalServerError)))
	require.False(t, autoModelSwitchableError(types.NewErrorWithStatusCode(
		errors.New("rate limited"), types.ErrorCodeGetChannelFailed, http.StatusTooManyRequests)))
	require.False(t, autoModelSwitchableError(types.NewErrorWithStatusCode(
		errors.New("unauthorized"), types.ErrorCodeGetChannelFailed, http.StatusUnauthorized)))
}

func TestTrySwitchAutoModelSkipsUnauthorizedCandidates(t *testing.T) {
	oldEnabled := operation_setting.AutoModelEnabled
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = oldEnabled })
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	common.SetContextKey(c, constant.ContextKeyAutoModelCandidates, []string{"old", "denied", "allowed"})
	common.SetContextKey(c, constant.ContextKeyAutoModelIndex, 0)
	common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"old": true, "allowed": true})
	c.Set("original_model", "old")
	c.Set("use_channel", []string{"9"})
	info := &relaycommon.RelayInfo{
		OriginModelName: "old",
		ClientModelName: "old",
		UsingGroup:     "default",
		UserGroup:      "default",
		UserSetting:    dto.UserSetting{AcceptUnsetRatioModel: true},
	}
	retry := &service.RetryParam{Ctx: c, ModelName: "old", Retry: common.GetPointer(1)}
	require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
	require.Equal(t, "allowed", info.OriginModelName)
	require.Equal(t, "allowed", info.ClientModelName)
	require.Equal(t, "allowed", retry.ModelName)
	require.Equal(t, "allowed", c.GetString("original_model"))
	require.Equal(t, 2, common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex))
	require.Empty(t, c.GetStringSlice("use_channel"))
}

func TestTrySwitchAutoModelRejectsUnauthorizedOrInvalidSnapshotWithoutMutation(t *testing.T) {
	oldEnabled := operation_setting.AutoModelEnabled
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = oldEnabled })
	for _, index := range []int{0, -1, 2} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
		common.SetContextKey(c, constant.ContextKeyAutoModelCandidates, []string{"old", "denied"})
		common.SetContextKey(c, constant.ContextKeyAutoModelIndex, index)
		common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
		common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"old": true, "auto": true})
		c.Set("original_model", "old")
		c.Set("use_channel", []string{"9"})
		info := &relaycommon.RelayInfo{OriginModelName: "old", ClientModelName: "old"}
		retry := &service.RetryParam{Ctx: c, ModelName: "old", Retry: common.GetPointer(1)}
		// nil meta would fail if the denied candidate reached model pricing.
		require.False(t, trySwitchAutoModel(c, info, retry, 0, nil))
		require.Equal(t, "old", info.OriginModelName)
		require.Equal(t, "old", info.ClientModelName)
		require.Equal(t, "old", retry.ModelName)
		require.Equal(t, "old", c.GetString("original_model"))
		require.Equal(t, index, common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex))
		require.Equal(t, []string{"9"}, c.GetStringSlice("use_channel"))
	}
}
