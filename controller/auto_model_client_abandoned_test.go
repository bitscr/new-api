package controller

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// autoModelClientAbandoned 用来判断"客户端在拿到本次交付物之前就离开"。
// 关键回归（实测）：只看 ctx.Err() 会把"客户端收完正文后正常关闭连接"也判成离开，
// 于是每个正常完成的请求的反馈都被丢掉，分数不再更新、慢成功的冷却也不触发。
func TestAutoModelClientAbandonedOnlyOnInterruptedDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
		return c
	}

	info := &relaycommon.RelayInfo{}

	c := newContext()
	require.False(t, autoModelClientAbandoned(c, info), "客户端还在等着，不算离开")

	request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	c.Request = request.WithContext(ctx)
	require.True(t, autoModelClientAbandoned(c, info),
		"ctx 取消且一个字节都没写出去：客户端在等到任何内容前就走了")

	c.Writer.WriteHeader(200)
	_, err := c.Writer.Write([]byte(`{"choices":[]}`))
	require.NoError(t, err)
	require.False(t, autoModelClientAbandoned(c, info),
		"ctx 取消但正文已经写完：客户端是收完才关的，必须照常记分")
}

func TestAutoModelClientAbandonedRecognizesCanceledPartialStreams(t *testing.T) {
	for _, auto := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			reason   relaycommon.StreamEndReason
			endErr   error
			canceled bool
			want     bool
		}{
			{"client-gone", relaycommon.StreamEndReasonClientGone, context.Canceled, true, true},
			{"scanner-cancel-race", relaycommon.StreamEndReasonScannerErr, fmt.Errorf("read: %w", context.Canceled), true, true},
			{"scanner-deadline-race", relaycommon.StreamEndReasonScannerErr, context.DeadlineExceeded, true, true},
			{"active-read-canceled", relaycommon.StreamEndReasonScannerErr, context.Canceled, false, false},
			{"real-read-failure", relaycommon.StreamEndReasonScannerErr, fmt.Errorf("unexpected EOF"), true, false},
			{"normal-done", relaycommon.StreamEndReasonDone, nil, true, false},
			{"normal-eof", relaycommon.StreamEndReasonEOF, nil, true, false},
			{"exploration-timeout", relaycommon.StreamEndReasonTimeout, relaycommon.ErrAutoExplorationTimeout, false, false},
		} {
			t.Run(fmt.Sprintf("%s/auto=%t", tc.name, auto), func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				if auto {
					common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
				}
				request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				ctx, cancel := context.WithCancel(request.Context())
				defer cancel()
				c.Request = request.WithContext(ctx)
				_, err := c.Writer.Write([]byte("partial answer"))
				require.NoError(t, err)
				if tc.canceled {
					cancel()
				}
				info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
				info.StreamStatus.SetEndReason(tc.reason, tc.endErr)
				require.Equal(t, tc.want, autoModelClientAbandoned(c, info))
			})
		}
	}
}

func TestCanceledPartialStreamPreservesEarlierFailureFeedback(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	c.Request = request.WithContext(ctx)
	info := &relaycommon.RelayInfo{
		UsingGroup: "default", OriginModelName: "earlier-failure",
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 1},
	}
	info.MarkUpstreamDispatch()
	recordAutoModelStreamFailure(c, info, "unexpected EOF")
	info.OriginModelName = "canceled-partial"
	info.StreamStatus = relaycommon.NewStreamStatus()
	_, err := c.Writer.Write([]byte("partial answer"))
	require.NoError(t, err)
	cancel()
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, context.Canceled)
	recordAutoModelStreamFailure(c, info, info.StreamStatus.Summary())
	recordAutoModelFeedback(c, info, true)
	recordAutoModelUnusableAnswer(c, info, "partial response")
	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 1, "client cancellation must neither cool this pair nor erase an earlier real failure")
	require.Equal(t, "unexpected EOF", pending[autoModelFeedbackKey{group: "default", modelName: "earlier-failure", channelID: 1}].reason)
}
