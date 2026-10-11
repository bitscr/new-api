package controller

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
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
	retry := &service.RetryParam{}
	// 头一次 beginAttempt 冻结预算:与渠道 override、全局 RetryTimes 都无关。
	require.True(t, budget.beginAttempt(&model.Channel{RetryTimes: common.GetPointer(1000)}, retry))
	attempts := 1
	for budget.canAttempt() {
		require.True(t, budget.beginAttempt(&model.Channel{RetryTimes: common.GetPointer(1000)}, retry))
		attempts++
		require.LessOrEqual(t, attempts, operation_setting.AutoModelMaxAttempts(),
			"auto 不能超过自己的上限")
	}
	require.Equal(t, operation_setting.AutoModelMaxAttempts(), attempts,
		"RetryTimes=50 时 auto 仍只花自己的上限")
	require.Zero(t, budget.remainingRetries)
	require.False(t, budget.beginAttempt(&model.Channel{}, retry))
}

// 面板/环境设置的上限必须被采纳。环境变量现在是启动种子(InitAutoModelSettingsFromEnv),
// 运行时以面板值为准,所以这里走 setter 而不是 t.Setenv。
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
	retry := &service.RetryParam{}
	require.True(t, budget.beginAttempt(&model.Channel{}, retry))
	require.True(t, budget.beginAttempt(&model.Channel{}, retry))
	require.False(t, budget.canAttempt(), "配置为 2 时只允许两次 dispatch")
	require.False(t, budget.beginAttempt(&model.Channel{}, retry))
}

// 确定性失败的分类要在"入队时"定格:队列是延迟 flush 的,事后读 RelayInfo
// 可能已经是另一次尝试的字段。这里锁定时序契约。
func TestAutoModelFeedbackFreezesFailureClassAtQueueTime(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	start := time.Unix(1700000000, 0)
	info := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "ghost-model",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 4},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(time.Second),
	}
	info.MarkUpstreamDispatch()

	// 一次确定性失败(模型不存在)。
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

// 抖动失败(限流/5xx)入队时不能被标成永久。
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
		errors.New("Upstream unavailable"), types.ErrorCode("server_error"), http.StatusServiceUnavailable))

	pending := takeAutoModelFeedback(c)
	outcome := pending[autoModelFeedbackKey{group: "default", modelName: "busy-model", channelID: 2}]
	require.False(t, outcome.permanent, "5xx 是抖动失败,不能永久封禁")
}