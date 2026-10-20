package controller

import (
	"fmt"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/stretchr/testify/require"
)

// Exercise the real queue, service EWMA and route planner. No private score
// getter or new production symbol is needed for this regression to go RED.
//
// 排序判据是模型有效得分(渠道聚合健康分已随分档方案移除),所以观察必须记在
// 真实路由模型上。快慢两种成功都不触发冷却(只有超过 20s 的慢成功才冷却),
// 让最后一次观察(last attempt wins)成为唯一区分:快成功把该渠道的该模型抬进
// 更高的档,慢成功的延迟惩罚把它压进更低的档,冷启动渠道保持 0.5。
func TestAutoModelFeedbackChronologicalChannelRanking(t *testing.T) {
	oldRatios := ratio_setting.ModelRatio2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(oldRatios)) })
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"score-probe":1}`))
	const (
		fastLatencyMS = 100
		slowLatencyMS = 15000
	)
	type observation struct {
		latencyMS int64
	}
	for _, tc := range []struct {
		name       string
		outcomes   []observation
		wantScored bool
	}{
		{"slow_then_fast", []observation{{slowLatencyMS}, {fastLatencyMS}}, true},
		{"fast_then_slow", []observation{{fastLatencyMS}, {slowLatencyMS}}, false},
		{"replacement_moves_last", []observation{{slowLatencyMS}, {slowLatencyMS}, {fastLatencyMS}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openAutoModelRouteControllerTestDB(t)
			const probes = 16
			wrong := 0
			for i := 0; i < probes; i++ {
				group := fmt.Sprintf("%s_%d", t.Name(), i)
				scored := createAutoModelRouteTestChannel(t, db, group, "score-probe")
				neutral := createAutoModelRouteTestChannel(t, db, group, "score-probe")
				c := newAutoModelFeedbackTestContext()
				info := &relaycommon.RelayInfo{UsingGroup: group, OriginModelName: "score-probe", ChannelMeta: &relaycommon.ChannelMeta{ChannelId: scored.Id}}
				for _, outcome := range tc.outcomes {
					queueAutoModelFeedback(c, info, autoModelFeedback{
						success: true, latencyMS: outcome.latencyMS, elapsedMS: outcome.latencyMS,
					})
				}
				flushAutoModelFeedback(c)
				require.Empty(t, takeAutoModelFeedback(c), "flush must drain the batch exactly once")
				routes, err := service.BuildAutoModelRoutesForRequest(c, group)
				require.NoError(t, err)
				require.Len(t, routes, 2, "both routes stay eligible: fast/slow successes never trip a cooldown")
				want := neutral.Id
				if tc.wantScored {
					want = scored.Id
				}
				if routes[0].ChannelID != want {
					wrong++
				}
			}
			t.Logf("probes=%d incorrect_channel_rankings=%d", probes, wrong)
			if wrong != 0 {
				t.Errorf("%d channel ranking violations against chronological final-outcome order", wrong)
			}
		})
	}
}
