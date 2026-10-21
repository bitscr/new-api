package controller

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// auto 的单请求预算与命名请求的 RetryTimes 解耦:不管全局 RetryTimes 多大,
// auto 只花自己的上限。
func TestAutoModelBudgetIgnoresGlobalRetryTimes(t *testing.T) {
	oldRetryTimes := common.RetryTimes
	t.Cleanup(func() { common.RetryTimes = oldRetryTimes })
	common.RetryTimes = 50

	var budget autoModelAttemptBudget
	attempts := 0
	for budget.canAttempt() {
		require.True(t, budget.reserveAttempt())
		budget.confirmAttempt(true)
		attempts++
		require.LessOrEqual(t, attempts, operation_setting.AutoModelMaxAttempts(),
			"auto 不能超过自己的上限")
	}
	require.Equal(t, operation_setting.AutoModelMaxAttempts(), attempts,
		"RetryTimes=50 时 auto 仍只花自己的上限")
	require.Zero(t, budget.remainingRetries())
	require.False(t, budget.reserveAttempt())
}

func TestAutoModelBudgetHonoursConfiguredLimit(t *testing.T) {
	oldRetryTimes := common.RetryTimes
	oldAttempts := operation_setting.AutoModelMaxAttempts()
	t.Cleanup(func() {
		common.RetryTimes = oldRetryTimes
		operation_setting.SetAutoModelMaxAttempts(oldAttempts)
	})
	common.RetryTimes = 50
	operation_setting.SetAutoModelMaxAttempts(2)
	require.Equal(t, 2, operation_setting.AutoModelMaxAttempts())

	var budget autoModelAttemptBudget
	require.True(t, budget.reserveAttempt())
	budget.confirmAttempt(true)
	require.True(t, budget.reserveAttempt())
	budget.confirmAttempt(true)
	require.False(t, budget.canAttempt(), "配置为 2 时只允许两次真实派发")
	require.False(t, budget.reserveAttempt())
}

// 本地拒绝(没有真实派发)不占真实派发预算:前四次本地 RPM 拒绝、第五次才派发,
// 派发次数只计 1。这也钉住"暂占 → 确认/撤销"两阶段记账:HasUpstreamDispatch 为
// false 的尝试绝不消耗预算。
func TestAutoModelBudgetDoesNotConsumeOnLocalRejection(t *testing.T) {
	oldAttempts := operation_setting.AutoModelMaxAttempts()
	t.Cleanup(func() { operation_setting.SetAutoModelMaxAttempts(oldAttempts) })
	operation_setting.SetAutoModelMaxAttempts(4)

	var budget autoModelAttemptBudget
	for i := 0; i < 4; i++ {
		require.True(t, budget.reserveAttempt(), "本地拒绝不能被真实预算挡住")
		budget.confirmAttempt(false)
	}
	require.Zero(t, budget.dispatched, "本地拒绝不计真实派发")
	require.True(t, budget.canAttempt(), "本地拒绝之后仍应能真正派发")
	require.True(t, budget.reserveAttempt())
	budget.confirmAttempt(true)
	require.Equal(t, 1, budget.dispatched)
	require.Equal(t, 3, budget.remainingRetries())
}

// 确定性失败的分类要在"入队时"定格:队列是延迟 flush 的,事后读 RelayInfo
// 可能已经是另一次尝试的字段。这里锁定时序契约;判据来自管理员配置的关键词。
func TestAutoModelFeedbackFreezesFailureClassAtQueueTime(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("model not found")

	c := newAutoModelFeedbackTestContext()
	start := time.Unix(1700000000, 0)
	info := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "ghost-model",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 4},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(time.Second),
	}
	info.MarkUpstreamDispatch()	// 一次确定性失败(命中关键词"model not found")。
	recordAutoModelUpstreamFailure(c, info, types.NewErrorWithStatusCode(
		errors.New("Requested model not found"), types.ErrorCodeModelNotFound, http.StatusNotFound))

	// 之后 RelayInfo 被复用成别的组合/别的错误,不能影响已经定格的那条。
	info.OriginModelName = "healthy-model"
	info.ChannelMeta.ChannelId = 9

	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 1)
	outcome := pending[autoModelFeedbackKey{group: "default", modelName: "ghost-model", channelID: 4}]
	require.True(t, outcome.permanent, "确定性失败必须在入队时就标成永久")
	require.False(t, outcome.success)
}

// 抖动失败(限流/超时)入队时不能被标成永久:未命中任何管理员关键词的失败
// 一律按抖动处理,源码不内置任何确定性判据。
func TestAutoModelFeedbackKeepsTransientFailureTransient(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	start := time.Unix(1700000000, 0)
	info := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "busy-model",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 2},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(time.Second),
	}
	info.MarkUpstreamDispatch()
	recordAutoModelUpstreamFailure(c, info, types.NewErrorWithStatusCode(
		errors.New("Account rate limit exceeded"), types.ErrorCode("rate_limit_exceeded"), http.StatusTooManyRequests))

	pending := takeAutoModelFeedback(c)
	outcome := pending[autoModelFeedbackKey{group: "default", modelName: "busy-model", channelID: 2}]
	require.False(t, outcome.permanent, "限流是抖动失败,不能永久封禁")
}