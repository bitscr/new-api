package controller

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/require"
)

func TestAutoModelProtocolErrorKeywordsRetireOnlyMatchedPair(t *testing.T) {
	oldPermanent := operation_setting.AutoModelPermanentKeywordsToString()
	oldTransient := operation_setting.AutoModelTransientKeywordsToString()
	t.Cleanup(func() {
		operation_setting.SetAutoModelPermanentKeywords(oldPermanent)
		operation_setting.SetAutoModelTransientKeywords(oldTransient)
	})
	for _, tc := range []struct {
		name, payload, transient string
		permanent                bool
	}{
		{"chat-model-missing", `{"error":{"message":"Model not found","code":"model_not_found"}}`, "", true},
		{"responses-model-missing", `{"type":"response.failed","response":{"error":{"message":"Model not found","code":"model_not_found"}}}`, "", true},
		{"transient-override", `{"error":{"message":"All candidates failed: Model not found","code":"model_not_found"}}`, "all candidates failed", false},
		{"credit-exhaustion", `{"error":{"message":"You have no remaining credits","code":"insufficient_quota"}}`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openAutoModelRouteControllerTestDB(t)
			group, modelName := t.Name(), "protocol-retire"
			channel := createAutoModelRouteTestChannel(t, db, group, modelName, "working-sibling")
			c, info := newAutoModelAttributionContextForBody(t, "http://upstream.fixture", modelName, autoModelChainPayload(modelName))
			info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: channel.Id}
			info.UsingGroup = group
			info.BeginAttempt()
			info.MarkUpstreamDispatch()
			operation_setting.SetAutoModelPermanentKeywords("model_not_found")
			operation_setting.SetAutoModelTransientKeywords(tc.transient)
			info.IsStream = true
			helper.StreamScannerHandler(c, &http.Response{
				Body: io.NopCloser(strings.NewReader("data: " + tc.payload + "\n\ndata: [DONE]\n\n")),
			}, info, func(string, *helper.StreamResult) {})
			recordAutoModelUnusableAnswer(c, info, "protocol_error")
			pending := takeAutoModelFeedback(c)
			require.Len(t, pending, 1)
			outcome := pending[autoModelFeedbackKey{group: group, modelName: modelName, channelID: channel.Id}]
			require.Equal(t, tc.permanent, outcome.permanent)
			require.Equal(t, !tc.permanent, outcome.unusable)
			c.Set(autoModelFeedbackContextKey, pending)
			flushAutoModelFeedback(c)
			require.NoError(t, db.First(channel, channel.Id).Error)
			if tc.permanent {
				require.Equal(t, "working-sibling", channel.Models)
			} else {
				require.Contains(t, channel.Models, modelName)
				_, cooling := service.GetAutoModelCooldown(group, modelName, channel.Id, time.Now())
				require.True(t, cooling)
			}
		})
	}
}
