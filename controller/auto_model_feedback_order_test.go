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
func TestAutoModelFeedbackChronologicalChannelRanking(t *testing.T) {
	oldRatios := ratio_setting.ModelRatio2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(oldRatios)) })
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"score-probe":1}`))
	type observation struct {
		model   string
		success bool
	}
	for _, tc := range []struct {
		name       string
		outcomes   []observation
		wantScored bool
	}{
		{"failure_then_success", []observation{{"failed", false}, {"succeeded", true}}, true},
		{"success_then_failure", []observation{{"succeeded", true}, {"failed", false}}, false},
		{"replacement_moves_last", []observation{{"succeeded", false}, {"failed", false}, {"succeeded", true}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openAutoModelRouteControllerTestDB(t)
			const probes = 256
			wrong := 0
			for i := 0; i < probes; i++ {
				group := fmt.Sprintf("%s_%d", t.Name(), i)
				scored := createAutoModelRouteTestChannel(t, db, group, "score-probe")
				neutral := createAutoModelRouteTestChannel(t, db, group, "score-probe")
				c := newAutoModelFeedbackTestContext()
				info := &relaycommon.RelayInfo{UsingGroup: group, ChannelMeta: &relaycommon.ChannelMeta{ChannelId: scored.Id}}
				for _, outcome := range tc.outcomes {
					info.OriginModelName = outcome.model
					queueAutoModelFeedback(c, info, autoModelFeedback{success: outcome.success, elapsedMS: 1})
				}
				flushAutoModelFeedback(c)
				require.Empty(t, takeAutoModelFeedback(c), "flush must drain the batch exactly once")
				routes, err := service.BuildAutoModelRoutesForRequest(c, group)
				require.NoError(t, err)
				require.Len(t, routes, 2, "both candidate models are cold; only channel health distinguishes them")
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
