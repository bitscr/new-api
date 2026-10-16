package controller

import (
	"errors"
	"net/http"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// 能力不匹配(模型给不了本次请求要的能力,如 tools)必须以"确定性失败"进入反馈
// 队列:首犯冷却 24 小时,再犯由 RetireAutoModelCombination 摘除该组合。
//
// 这条契约曾经在真实链路上被一个提前拦截分支破坏——错误在
// recordAutoModelUpstreamFailure 之前被整个吞掉(不冷却、不记分),service 层的
// "冷却 → 摘除"永远走不到,废组合被反复选中、每次白费一次尝试,正是
// "不适配的一直重复运行"。本用例锁在队列层:能力措辞必须判成 permanent。
func TestAutoModelFeedbackMarksCapabilityMismatchPermanent(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	start := time.Unix(1700000000, 0)
	info := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "no-tools-model",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 11},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(time.Second),
	}
	info.MarkUpstreamDispatch()

	// 上游对带 tools 的请求回 400,正文是最常见的措辞之一。
	recordAutoModelUpstreamFailure(c, info, types.NewErrorWithStatusCode(
		errors.New("function calling is not supported"), types.ErrorCodeBadResponse, http.StatusBadRequest))

	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 1)
	outcome := pending[autoModelFeedbackKey{group: "default", modelName: "no-tools-model", channelID: 11}]
	require.True(t, outcome.permanent,
		"能力不匹配必须按确定性失败记录,否则废组合留在候选池里被反复选中")
	require.False(t, outcome.success)
	require.NotEmpty(t, outcome.reason, "冷却原因要能写进日志与面板")
}
