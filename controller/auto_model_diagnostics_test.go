package controller

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAutoModelRetryDiagnosticExplainsActualBlockingGuard(t *testing.T) {
	oldAuto := operation_setting.AutoModelEnabled
	oldCodes := operation_setting.AutomaticRetryStatusCodesToString()
	operation_setting.AutoModelEnabled = true
	require.NoError(t, operation_setting.AutomaticRetryStatusCodesFromString("500-599"))
	t.Cleanup(func() {
		operation_setting.AutoModelEnabled = oldAuto
		require.NoError(t, operation_setting.AutomaticRetryStatusCodesFromString(oldCodes))
	})

	for _, tc := range []struct {
		name      string
		remaining int
		setup     func(*gin.Context, *relaycommon.RelayInfo) *types.NewAPIError
		reason    string
		advice    string
	}{
		{
			name: "real_dispatch_budget", remaining: 0, reason: "budget_exhausted", advice: "AutoModelMaxAttempts",
		},
		{
			name: "explicit_skip", remaining: 2, reason: "error_skip_retry", advice: "不会改变该限制",
			setup: func(_ *gin.Context, _ *relaycommon.RelayInfo) *types.NewAPIError {
				return types.NewErrorWithStatusCode(errors.New("do not retry"), types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			},
		},
		{
			name: "hard_skip_code", remaining: 2, reason: "error_code_skip_retry", advice: "不会改变该限制",
			setup: func(_ *gin.Context, _ *relaycommon.RelayInfo) *types.NewAPIError {
				return types.NewErrorWithStatusCode(errors.New("bad body"), types.ErrorCodeBadResponseBody, http.StatusBadGateway)
			},
		},
		{
			name: "excluded_upstream_status", remaining: 2, reason: "status_not_retryable", advice: "AutomaticRetryStatusCodes",
			setup: func(_ *gin.Context, _ *relaycommon.RelayInfo) *types.NewAPIError {
				return types.WithOpenAIError(types.OpenAIError{Code: "rate_limit_exceeded", Message: "Account rate limit exceeded"}, http.StatusTooManyRequests)
			},
		},
		{
			name: "local_conversion", remaining: 2, reason: "not_dispatched", advice: "本地请求转换",
			setup: func(_ *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
				info.BeginAttempt()
				return types.NewErrorWithStatusCode(errors.New("conversion failed"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
			},
		},
		{
			name: "local_error_is_not_retry_status_configuration", remaining: 2, reason: "local_request_error", advice: "不能绕过该保护",
			setup: func(_ *gin.Context, _ *relaycommon.RelayInfo) *types.NewAPIError {
				return types.NewErrorWithStatusCode(errors.New("invalid local request"), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
			},
		},
		{
			name: "invalid_status_is_not_retry_status_configuration", remaining: 2, reason: "status_invalid", advice: "错误封装",
			setup: func(_ *gin.Context, _ *relaycommon.RelayInfo) *types.NewAPIError {
				return types.NewErrorWithStatusCode(errors.New("invalid status"), types.ErrorCodeBadResponse, 700)
			},
		},
		{
			name: "error_with_success_status", remaining: 2, reason: "status_success", advice: "错误封装",
			setup: func(_ *gin.Context, _ *relaycommon.RelayInfo) *types.NewAPIError {
				return types.NewErrorWithStatusCode(errors.New("bad envelope"), types.ErrorCodeBadResponse, http.StatusOK)
			},
		},
		{
			name: "next_channel_without_channel_identity", remaining: 2, reason: "channel_unidentified", advice: "渠道元数据",
			setup: func(_ *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
				info.ChannelMeta = nil
				return types.NewErrorWithStatusCode(errors.New("upstream server failed"), types.ErrorCodeBadResponse, http.StatusBadGateway)
			},
		},
		{
			name: "canceled_client_with_budget", remaining: 2, reason: "client_canceled", advice: "增加网关预算不能恢复",
			setup: func(c *gin.Context, _ *relaycommon.RelayInfo) *types.NewAPIError {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := service.AutoModelRoute{Group: "default", ModelName: "slow-working", ChannelID: 17}
			c, info, _ := newAutoModelRouteTestContext(t, route.Group, []service.AutoModelRoute{route})
			info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: route.ChannelID}
			info.BeginAttempt()
			info.MarkUpstreamDispatch()
			apiErr := types.WithOpenAIError(types.OpenAIError{Code: "server_error", Message: "Upstream unavailable"}, http.StatusBadGateway)
			if tc.setup != nil {
				if replacement := tc.setup(c, info); replacement != nil {
					apiErr = replacement
				}
			}

			reason := autoModelRetryIneligibilityReason(c, info, apiErr, tc.remaining)
			require.Equal(t, tc.reason, reason)
			require.Contains(t, autoModelRetryAdvice(reason, apiErr), tc.advice)
			require.False(t, prepareAutoModelRetry(c, info, apiErr, tc.remaining), "the explanation must match the actual retry guard")
			require.Empty(t, service.GetAutoModelHardExcludedChannelIDs(c), "diagnostics must not exclude a route")
			require.False(t, service.IsAutoModelModelFailed(c, route.Group, route.ModelName))
			current, ok := service.GetCurrentAutoModelRoute(c)
			require.True(t, ok)
			require.Equal(t, route, current)
		})
	}
}

// The same configured writers feed the console and existing daily log files.
// Capture them together to guard the established prefix and one-line format.
type autoModelDiagnosticLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *autoModelDiagnosticLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *autoModelDiagnosticLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func captureAutoModelDiagnosticLogs(t *testing.T) *autoModelDiagnosticLogBuffer {
	t.Helper()
	buffer := &autoModelDiagnosticLogBuffer{}
	common.LogWriterMu.Lock()
	oldWriter, oldErrorWriter := gin.DefaultWriter, gin.DefaultErrorWriter
	gin.DefaultWriter, gin.DefaultErrorWriter = buffer, buffer
	common.LogWriterMu.Unlock()
	t.Cleanup(func() {
		common.LogWriterMu.Lock()
		gin.DefaultWriter, gin.DefaultErrorWriter = oldWriter, oldErrorWriter
		common.LogWriterMu.Unlock()
	})
	return buffer
}

func TestAutoModelDiagnosticResultAndFeedbackUseExistingRequestLogger(t *testing.T) {
	openAutoModelRouteControllerTestDB(t)
	logs := captureAutoModelDiagnosticLogs(t)
	route := service.AutoModelRoute{Group: t.Name(), ModelName: "slow-working", ChannelID: 713}
	c, info, _ := newAutoModelRouteTestContext(t, route.Group, []service.AutoModelRoute{route})
	c.Set(common.RequestIdKey, "req-route-diagnostic")
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	service.SetAutoModelRoutePlanMeta(c, service.AutoModelRoutePlanMeta{
		PlannedAt:  time.Now(),
		Categories: map[service.AutoModelRoute]string{route: "successful_trial"},
		Reasons:    map[service.AutoModelRoute]string{route: "samples\r\n are\tinsufficient"},
		Parameters: map[service.AutoModelRoute]service.AutoModelRouteParameters{
			route: {Priority: 100, ChannelWeight: 2, ModelWeight: 0.25},
		},
	})
	info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: route.ChannelID}
	info.IsStream = true
	start := time.Now().Add(-12 * time.Second)
	info.AttemptStartTime = start
	info.AttemptDispatchTime = start.Add(5 * time.Second)
	info.AttemptEndTime = start.Add(9 * time.Second)
	info.RetryIndex = 3 // Three local rejections preceded the first actual dispatch.
	info.MarkUpstreamDispatch()
	traceAutoModelAttempt(c, info, "error", "upstream\r\n disconnected\tbefore answer")

	info.AttemptFirstValidOutputTime = start.Add(6 * time.Second)
	info.RetryIndex = 4
	traceAutoModelAttempt(c, info, "ok", "")
	recordAutoModelFeedback(c, info, true)
	flushAutoModelFeedback(c)

	var diagnosticLines []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, "auto route diag:") {
			diagnosticLines = append(diagnosticLines, line)
			require.Regexp(t, `^\[INFO\] \d{4}/\d{2}/\d{2} - \d{2}:\d{2}:\d{2} \| req-route-diagnostic \| auto route diag:`, line)
			require.NotContains(t, line, "\r")
			require.NotContains(t, line, "\t")
		}
	}
	require.Len(t, diagnosticLines, 3, "every dispatched result and its published feedback use the existing writer")
	require.Contains(t, diagnosticLines[0], "event=result attempt=4 real_dispatched=1")
	require.Contains(t, diagnosticLines[0], "elapsed_ms=9000 dispatch_elapsed_ms=4000 first_valid_output_ms=?")
	require.Contains(t, diagnosticLines[0], "client_deadline_remaining_ms=?", "HTTP requests do not expose the external client's timeout")
	require.Contains(t, diagnosticLines[0], `detail="upstream disconnected before answer"`)
	require.Contains(t, diagnosticLines[0], `reason="samples are insufficient"`)
	require.Contains(t, diagnosticLines[0], "priority=100 channel_weight=2 model_weight=0.25")
	require.Contains(t, diagnosticLines[1], "event=result attempt=5 real_dispatched=2")
	require.Contains(t, diagnosticLines[1], "first_valid_output_ms=1000")
	require.Contains(t, diagnosticLines[2], "event=feedback")
	require.Contains(t, diagnosticLines[2], "action=success_recovery")
	require.Contains(t, diagnosticLines[2], "latency_known=true latency_ms=1000")
	require.Contains(t, diagnosticLines[2], "cooldown_active=false")
	require.Contains(t, diagnosticLines[2], "persist_pending=false")
	require.Empty(t, takeAutoModelFeedback(c), "diagnostic logging must not leave feedback queued")
	entries := autoModelAttemptTrace(c)
	require.Len(t, entries, 2)
	require.Equal(t, int64(9000), entries[0].ElapsedMS)
	require.Equal(t, int64(4000), entries[0].DispatchElapsedMS)
	require.Equal(t, int64(-1), entries[0].TTFTMS)
	require.Equal(t, int64(1000), entries[1].TTFTMS)
}

func TestAutoModelDiagnosticRequestDeadlineDistinguishesUnknownFromExpired(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	require.Equal(t, "client_deadline_remaining_ms=?", autoModelRequestDeadlineLog(c))
	ctx, cancel := context.WithDeadline(c.Request.Context(), time.Now().Add(-time.Second))
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	require.Equal(t, "client_deadline_remaining_ms=0", autoModelRequestDeadlineLog(c))
}
