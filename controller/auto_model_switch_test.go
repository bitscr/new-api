package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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

// 一次请求里同一个 (模型, 渠道) 只记一次反馈：
// 上游排队时实测同一请求重试 51 次，逐次记分会让观测数虚高、冷却等级被一次请求顶满。
func TestMarkAutoModelFeedbackRecordedDedupes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	require.True(t, markAutoModelFeedbackRecorded(c, "DeepSeek-Flash", 1), "第一次要记账")
	require.False(t, markAutoModelFeedbackRecorded(c, "DeepSeek-Flash", 1), "同一组合重复尝试不再记账")

	require.True(t, markAutoModelFeedbackRecorded(c, "DeepSeek-Flash", 10), "换渠道是新的组合")
	require.True(t, markAutoModelFeedbackRecorded(c, "GLM-5.3-Flash", 1), "换模型是新的组合")

	require.True(t, markAutoModelFeedbackRecorded(nil, "x", 1), "没有上下文时保持原有记账行为")
}
