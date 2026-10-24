package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func autoModelChainPayload(model string) map[string]any {
	return map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	}
}

// A real upstream response that is HTTP 200 but only contains the gateway
// banner must stay staged, then be published as a failure even though this test
// performs no switch. The next request must already see the cooling route.
func TestAutoModelRelayChainBannerOnlyIsNotCommitted(t *testing.T) {
	openAutoModelRouteControllerTestDB(t)
	banner := `{"choices":[{"index":0,"message":{"role":"assistant","content":"[Gateway Warning: busy]"},"finish_reason":"stop"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, banner)
	}))
	defer upstream.Close()

	model := "chain-banner"
	c, info, recorder := newAutoModelAttributionContextForBodyWithRecorder(t, upstream.URL, model, autoModelChainPayload(model))
	info.BeginAttempt()
	staged := relaycommon.InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	apiErr := relay.TextHelper(c, info)
	info.EndAttempt()
	require.Nil(t, apiErr)
	require.True(t, info.HasUpstreamDispatch())
	require.False(t, staged.Committed(), "a banner-only answer must never reach the client")
	require.Zero(t, recorder.Body.Len())

	unusable, reason := info.ClientAnswerUnusable()
	require.True(t, unusable, reason)
	require.Empty(t, autoModelInvalidSwitchBlockedReason(c, info,
		autoModelAttemptBudget{attempts: 1, dispatched: 1, started: true}, staged),
		"an uncommitted banner answer with budget left must be switchable")

	recordAutoModelUnusableAnswer(c, info, reason)
	flushAutoModelFeedback(c)
	require.Empty(t, takeAutoModelFeedback(c), "the failure must not stay queued without a switch")
	cooldown, active := service.GetAutoModelCooldown(info.UsingGroup, info.OriginModelName, info.ChannelId, time.Now())
	require.True(t, active, "the failure must be published before deciding whether to switch")
	require.GreaterOrEqual(t, cooldown.Level, 1)

	staged.Discard()
	staged.Restore()
	require.Zero(t, recorder.Body.Len())
	require.Zero(t, recorder.Header().Get("Content-Type"),
		"the discarded attempt must not leave response headers behind")
}

// A committed answer that the observer cannot classify is neutral evidence: the
// controller must not credit it as a success, and the client still receives the
// whole response.
func TestAutoModelRelayChainUndeterminedAnswerIsNeutral(t *testing.T) {
	openAutoModelRouteControllerTestDB(t)
	// Comfortably beyond the relay/common staging cap (256 KiB).
	body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"` +
		strings.Repeat("z", 600*1024) +
		`"},"finish_reason":"length"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	model := "chain-undetermined"
	c, info, recorder := newAutoModelAttributionContextForBodyWithRecorder(t, upstream.URL, model, autoModelChainPayload(model))
	info.BeginAttempt()
	staged := relaycommon.InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	apiErr := relay.TextHelper(c, info)
	info.EndAttempt()
	require.Nil(t, apiErr)
	require.True(t, staged.Committed(), "a response beyond the staging cap is delivered directly")
	require.True(t, staged.Overflow())
	require.Greater(t, recorder.Body.Len(), 256*1024,
		"the whole oversized response still reaches the client")
	require.Contains(t, recorder.Body.String(), strings.Repeat("z", 1024))
	require.False(t, info.ClientAnswerHasValidOutput())
	require.True(t, info.ClientAnswerUndetermined())
	require.True(t, autoModelAnswerOutcomeUnknown(c, info, staged),
		"an unclassifiable committed answer must stay neutral instead of counting as success")
}

func TestAutoModelResponseDeliveredTracksStagingState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	relaycommon.SetRelayInfo(c, &relaycommon.RelayInfo{IsStream: false})
	relaycommon.InstallRelayResponseWriter(c)

	require.False(t, autoModelResponseDelivered(c, nil), "nothing has reached the client yet")

	staged := relaycommon.InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	_, err := staged.Write([]byte("staged but invalid"))
	require.NoError(t, err)
	require.False(t, autoModelResponseDelivered(c, staged))
	staged.Discard()
	staged.Restore()
	require.False(t, autoModelResponseDelivered(c, nil),
		"a discarded candidate must not be mistaken for a delivered response")

	_, err = c.Writer.Write([]byte("delivered"))
	require.NoError(t, err)
	require.True(t, autoModelResponseDelivered(c, nil))
	require.Equal(t, "delivered", recorder.Body.String())
}
