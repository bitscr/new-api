package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// TestAutoModelChannelScoreFn 锁定渠道打分回调：只有 auto 请求才启用，
// 未观测的 (模型, 渠道) 回中性 0.5，观测过的按健康分。
func TestAutoModelChannelScoreFn(t *testing.T) {
	autoModelTestReset()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	// 非 auto 请求：不打分，保持原来的纯权重加权随机
	require.Nil(t, autoModelChannelScoreFn(c, "m"))

	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")

	score := autoModelChannelScoreFn(c, "m")
	require.NotNil(t, score)
	require.InDelta(t, 0.5, score(7), 0.0001, "未观测的组合回中性 0.5")

	RecordAutoModelOutcome("default", "m", 7, true, 300, 300)
	RecordAutoModelOutcome("default", "m", 8, false, 0, 0)
	score = autoModelChannelScoreFn(c, "m")
	require.Greater(t, score(7), 0.5, "快速成功的组合分数应高于中性")
	require.Less(t, score(8), 0.5, "失败过的组合分数应低于中性")
}
