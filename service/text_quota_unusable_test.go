package service_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeBilling struct {
	settled  bool
	refunded bool
}

func (f *fakeBilling) Settle(int) error    { f.settled = true; return nil }
func (f *fakeBilling) Refund(*gin.Context) { f.refunded = true }
func (f *fakeBilling) NeedsRefund() bool   { return !f.settled && !f.refunded }
func (f *fakeBilling) GetPreConsumedQuota() int {
	return 0
}
func (f *fakeBilling) Reserve(int) error { return nil }

func newUnusableBillingContext(t *testing.T) (*gin.Context, *relaycommon.RelayInfo, *fakeBilling) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-5.4")
	common.SetContextKey(c, constant.ContextKeyChannelId, 1)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "http://127.0.0.1")
	common.SetContextKey(c, constant.ContextKeyChannelKey, "fixture-key")
	common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{})
	c.Set("original_model", "gpt-5.4")
	request, apiErr := helper.GetAndValidateRequest(c, types.RelayFormatOpenAI)
	require.Nil(t, apiErr)
	info, err := relaycommon.GenRelayInfo(c, types.RelayFormatOpenAI, request, nil)
	require.NoError(t, err)
	info.BeginAttempt()
	billing := &fakeBilling{}
	info.Billing = billing
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	return c, info, billing
}

func TestPostTextConsumeQuotaRefundsEmptyAnswer(t *testing.T) {
	c, info, billing := newUnusableBillingContext(t)
	info.MarkUpstreamContentExhausted()

	service.PostTextConsumeQuota(c, info, nil, nil)

	require.True(t, billing.refunded, "an empty 200 answer must refund the pre-charge")
	require.False(t, billing.settled, "an empty 200 answer must not settle billing")
}

func TestPostTextConsumeQuotaRefundsExplicitProtocolError(t *testing.T) {
	c, info, billing := newUnusableBillingContext(t)
	info.ObserveUpstreamError([]byte(`{"error":{"message":"upstream failed"}}`))
	require.True(t, info.HasExplicitProtocolError())

	service.PostTextConsumeQuota(c, info, nil, nil)

	require.True(t, billing.refunded, "an explicit protocol error must refund the pre-charge")
	require.False(t, billing.settled, "an explicit protocol error must not settle billing")
}

func TestPostTextConsumeQuotaRefundsAbnormalStream(t *testing.T) {
	c, info, billing := newUnusableBillingContext(t)
	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonScannerErr, errors.New("unexpected EOF"))

	service.PostTextConsumeQuota(c, info, nil, nil)

	require.True(t, billing.refunded, "a truncated stream must refund the pre-charge")
	require.False(t, billing.settled, "a truncated stream must not settle billing")
}

func TestHasExplicitProtocolErrorNilSafe(t *testing.T) {
	var info *relaycommon.RelayInfo
	require.False(t, info.HasExplicitProtocolError())
}
