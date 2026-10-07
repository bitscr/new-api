package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAutoModelCanceledRequestPreservesObservedError(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer stop()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "client_cancel", ctx: canceled, want: context.Canceled},
		{name: "request_deadline", ctx: expired, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newAutoModelFeedbackTestContext()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(tc.ctx)
			require.ErrorIs(t, autoModelRequestContextError(c), tc.want)
			generated := autoModelCanceledRequestError(c, nil)
			require.NotNil(t, generated, "cancellation before the first attempt still needs a response error")
			require.ErrorIs(t, generated, tc.want)
			require.Equal(t, http.StatusRequestTimeout, generated.StatusCode)
			require.True(t, types.IsSkipRetryError(generated))
			require.False(t, types.IsRecordErrorLog(generated))

			upstream := types.NewError(errors.New("observed upstream failure"), types.ErrorCodeChannelNoAvailableKey)
			require.Same(t, upstream, autoModelCanceledRequestError(c, upstream), "do not erase an earlier real upstream error")
			require.False(t, shouldRetry(c, upstream, 10), "even normally retryable channel errors stop on client cancellation")
		})
	}
}

func TestAutoModelCancellationLeavesNonAutoAndActiveRequestsUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	upstream := types.NewError(errors.New("upstream failure"), types.ErrorCodeChannelNoAvailableKey)
	require.NoError(t, autoModelRequestContextError(c))
	require.Nil(t, autoModelCanceledRequestError(c, upstream))
	require.True(t, shouldRetry(c, upstream, 10), "non-auto retry semantics must remain unchanged")

	active := newAutoModelFeedbackTestContext()
	active.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.NoError(t, autoModelRequestContextError(active))
	require.Nil(t, autoModelCanceledRequestError(active, upstream))
	require.True(t, shouldRetry(active, upstream, 10))
	require.NoError(t, autoModelRequestContextError(nil))
	require.Nil(t, autoModelCanceledRequestError(nil, nil))
	require.NoError(t, autoModelRequestContextError(newAutoModelFeedbackTestContext()), "test contexts may omit Request")
}

func TestAutoModelCancellationDoesNotOverwritePriorFeedback(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	start := time.Unix(1700000000, 0)
	info := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "model-a",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 1},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(30 * time.Second),
	}
	recordAutoModelFeedback(c, info, false)
	cancel()

	// A canceled retry must neither replace this earlier failure with a false
	// success/empty-answer outcome nor create a new failure on another channel.
	info.AttemptEndTime = start.Add(time.Second)
	recordAutoModelFeedback(c, info, false)
	recordAutoModelFeedback(c, info, true)
	recordAutoModelUnusableAnswer(c, info, "partial stream after client disconnect")
	info.ChannelMeta.ChannelId = 2
	recordAutoModelFeedback(c, info, false)
	recordAutoModelUnusableAnswer(c, info, "client disconnected before any answer")

	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 1)
	feedback := pending[autoModelFeedbackKey{group: "default", modelName: "model-a", channelID: 1}]
	require.False(t, feedback.success)
	require.False(t, feedback.unusable)
	require.Equal(t, int64(30000), feedback.elapsedMS, "the real earlier observation remains available for deferred flushing")
	require.Empty(t, takeAutoModelFeedback(c))
}

func TestObservedAutoLatencyRetainsValidSubMillisecondSuccess(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, firstResponseDelay := range []time.Duration{0, 500 * time.Microsecond} {
		info := &relaycommon.RelayInfo{
			IsStream:                 true,
			AttemptStartTime:         start,
			AttemptFirstResponseTime: start.Add(firstResponseDelay),
			AttemptEndTime:           start.Add(time.Minute),
		}
		require.Equal(t, int64(1), observedAutoLatency(info, true))
		require.Equal(t, int64(1), observedAutoModelCooldownLatency(info, true), "valid fast streams must clear prior cooldowns")
		require.Equal(t, int64(60000), observedAutoModelCooldownLatency(info, false), "failure still uses elapsed time")
	}

	nonStream := &relaycommon.RelayInfo{AttemptStartTime: start, AttemptEndTime: start.Add(500 * time.Microsecond)}
	require.Equal(t, int64(1), observedAutoLatency(nonStream, true))
	require.Equal(t, int64(1), observedAutoModelCooldownLatency(nonStream, true))
	require.Zero(t, observedAutoModelCooldownLatency(&relaycommon.RelayInfo{}, true), "missing timing must stay unknown")
	require.Zero(t, observedAutoModelCooldownLatency(&relaycommon.RelayInfo{
		IsStream: true, AttemptStartTime: start, AttemptEndTime: start.Add(time.Minute),
	}, true), "a stream with no observed TTFB must not receive an invented sample")
	require.Zero(t, observedAutoModelCooldownLatency(&relaycommon.RelayInfo{
		AttemptStartTime: start, AttemptEndTime: start.Add(-time.Second),
	}, true), "invalid timing must stay unknown")
}
