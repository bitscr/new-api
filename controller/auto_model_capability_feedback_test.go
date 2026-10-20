package controller

import (
	"errors"
	"net/http"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// 能力不匹配(模型给不了本次请求要的能力,如 tools)必须以"确定性失败"进入反馈
// 队列:命中管理员配置的确定性关键词即直接摘除该组合。
//
// 这条契约曾经在真实链路上被一个提前拦截分支破坏——错误在
// recordAutoModelUpstreamFailure 之前被整个吞掉(不冷却、不记分),service 层的
// 摘除永远走不到,废组合被反复选中、每次白费一次尝试,正是
// "不适配的一直重复运行"。本用例锁在队列层:配置关键词后,能力措辞必须判成
// permanent;判据完全来自管理员关键词,源码不再内置任何文案判据。
func TestAutoModelFeedbackMarksCapabilityMismatchPermanent(t *testing.T) {
	original := operation_setting.AutoModelPermanentKeywordsToString()
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords(original) })
	operation_setting.SetAutoModelPermanentKeywords("is not supported")

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
		"命中管理员确定性关键词的能力不匹配必须按确定性失败记录,否则废组合留在候选池里被反复选中")
	require.False(t, outcome.success)
	require.NotEmpty(t, outcome.reason, "冷却原因要能写进日志与面板")
}
