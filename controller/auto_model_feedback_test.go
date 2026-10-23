package controller

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newAutoModelFeedbackTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	return c
}

func TestAutoModelFeedbackRetrySuccessReplacesFailure(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	start := time.Unix(1700000000, 0)
	info := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "model-a",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 1},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(40 * time.Second),
	}
	info.MarkUpstreamDispatch()
	for i := 0; i < 51; i++ {
		recordAutoModelFeedback(c, info, false)
	}
	info.AttemptStartTime = start.Add(time.Minute)
	info.AttemptFirstResponseTime = info.AttemptStartTime.Add(500 * time.Millisecond)
	info.AttemptFirstValidOutputTime = info.AttemptStartTime.Add(time.Second)
	info.AttemptEndTime = info.AttemptStartTime.Add(2 * time.Minute)
	info.IsStream = true
	recordAutoModelFeedback(c, info, true)

	// Feedback must have captured timing and identity, not a mutable info pointer.
	info.OriginModelName = "model-b"
	info.UsingGroup = "vip"
	info.ChannelMeta.ChannelId = 2
	info.AttemptFirstResponseTime = time.Time{}
	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 1, "all attempts of one combination produce one outcome")
	outcome := pending[autoModelFeedbackKey{group: "default", modelName: "model-a", channelID: 1}]
	require.True(t, outcome.success, "a later success must not be discarded by the first failure")
	require.False(t, outcome.unusable)
	require.Equal(t, int64(1000), outcome.latencyMS)
	require.Equal(t, int64(1000), outcome.elapsedMS, "stream cooldown uses final attempt TTFB, not generation duration")
	require.Empty(t, takeAutoModelFeedback(c), "flush drains feedback exactly once")
}

func TestAutoModelFeedbackKeepsGroupModelAndChannelIndependent(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	start := time.Unix(1700000000, 0)
	info := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "model-a",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 1},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(30 * time.Second),
	}
	info.MarkUpstreamDispatch()
	for i := 0; i < 51; i++ {
		recordAutoModelFeedback(c, info, false)
	}
	info.ChannelMeta.ChannelId = 2
	recordAutoModelFeedback(c, info, false)
	info.OriginModelName = "model-b"
	recordAutoModelFeedback(c, info, false)
	info.UsingGroup = "vip"
	recordAutoModelFeedback(c, info, false)

	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 4)
	for _, key := range []autoModelFeedbackKey{
		{group: "default", modelName: "model-a", channelID: 1},
		{group: "default", modelName: "model-a", channelID: 2},
		{group: "default", modelName: "model-b", channelID: 2},
		{group: "vip", modelName: "model-b", channelID: 2},
	} {
		outcome, ok := pending[key]
		require.True(t, ok)
		require.False(t, outcome.success)
		require.Zero(t, outcome.latencyMS)
		require.Equal(t, int64(30000), outcome.elapsedMS)
	}
}

func TestAutoModelFeedbackUnusableFinalAnswerReplacesFailure(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	info := &relaycommon.RelayInfo{UsingGroup: "default", OriginModelName: "model-a", ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 1}}
	info.MarkUpstreamDispatch()
	recordAutoModelFeedback(c, info, false)
	recordAutoModelUnusableAnswer(c, info, "empty response")
	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 1)
	outcome := pending[autoModelFeedbackKey{group: "default", modelName: "model-a", channelID: 1}]
	require.False(t, outcome.success)
	require.True(t, outcome.unusable)
	require.Equal(t, "empty response", outcome.reason)
}

// 具名请求(固定模型)的成败也并入健康体系——这是刻意行为:成败都是
// (分组,模型,渠道) 的证据,差异只在路由行为。但受 AutoModelEnabled 门控,
// 且 info 缺失时必须安全跳过。
func TestAutoModelFeedbackNamedRequestsAndMissingInfo(t *testing.T) {
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = true })

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{}
	info.MarkUpstreamDispatch()
	recordAutoModelFeedback(c, info, true)
	require.Len(t, takeAutoModelFeedback(c), 1, "具名请求的成败也是健康证据")

	operation_setting.AutoModelEnabled = false
	recordAutoModelFeedback(c, info, true)
	recordAutoModelUnusableAnswer(c, info, "empty")
	require.Empty(t, takeAutoModelFeedback(c), "auto 关闭时具名请求不并入")
	operation_setting.AutoModelEnabled = true

	c = newAutoModelFeedbackTestContext()
	recordAutoModelFeedback(c, nil, true)
	recordAutoModelUnusableAnswer(c, nil, "empty")
	require.Empty(t, takeAutoModelFeedback(c))
	recordAutoModelFeedback(nil, nil, false)
	flushAutoModelFeedback(nil)
}
