package controller

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// 失败必须带上真实耗时:秒拒和卡满超时都是"失败",但对客户端是两种体验,
// 评分里必须能区分。这里锁住"耗时会进入反馈",不锁具体 EWMA 数值。
func TestAutoModelUpstreamFailureCarriesElapsedLatency(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	start := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
	}{
		{"instant_reject", 200 * time.Millisecond},
		{"hung_then_failed", 45 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				UsingGroup:       "default",
				OriginModelName:  "ghost-model",
				ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 4},
				AttemptStartTime: start,
				AttemptEndTime:   start.Add(tc.elapsed),
			}
			info.MarkUpstreamDispatch()
			recordAutoModelUpstreamFailure(c, info, types.NewErrorWithStatusCode(
				errors.New("Requested model not found"), types.ErrorCodeModelNotFound, http.StatusNotFound))
			pending := takeAutoModelFeedback(c)
			require.Len(t, pending, 1)
			for _, feedback := range pending {
				require.False(t, feedback.success)
				require.Greater(t, feedback.latencyMS, int64(0),
					"失败必须记录耗时,否则秒拒和卡死无法区分")
				require.Equal(t, tc.elapsed.Milliseconds(), feedback.latencyMS)
				require.Equal(t, feedback.elapsedMS, feedback.latencyMS)
			}
		})
	}
}

// 关键词把原本认不出的失败升级为长冷却,并且这条路径必须是真实请求路径:
// 同一个错误只改管理员关键词,分类结果必须翻转。
func TestAutoModelKeywordUpgradesOnlyWhenConfigured(t *testing.T) {
	original := operation_setting.AutoModelPermanentKeywordsToString()
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords(original) })
	_ = common.OptionMap

	err := types.NewErrorWithStatusCode(
		errors.New("tools / function calling is not supported by the upstream anonymous backend"),
		types.ErrorCode("unsupported_feature"), http.StatusBadRequest)

	operation_setting.SetAutoModelPermanentKeywords("")
	require.False(t, service.AutoModelPermanentFailure(err), "未配置关键词时这类失败只是抖动")

	operation_setting.SetAutoModelPermanentKeywords("function calling is not supported")
	require.True(t, service.AutoModelPermanentFailure(err), "配置关键词后同一失败升级为确定性")
}