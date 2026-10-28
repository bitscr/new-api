package service

import (
	"net/http/httptest"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGenerateTextOtherInfoUsesFinalAttemptTimingAfterRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
	start := time.Now().Add(-3 * time.Minute)
	info := &relaycommon.RelayInfo{
		StartTime:                   start,
		FirstResponseTime:           start.Add(1444 * time.Millisecond),
		AttemptStartTime:            start.Add(32 * time.Second),
		AttemptDispatchTime:         start.Add(32500 * time.Millisecond),
		AttemptFirstResponseTime:    start.Add(115 * time.Second),
		AttemptFirstValidOutputTime: start.Add(115750 * time.Millisecond),
		AttemptEndTime:              start.Add(116 * time.Second),
		ChannelMeta:                 &relaycommon.ChannelMeta{},
	}

	other := GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 0, 0, 0)
	require.Equal(t, true, other["frt_known"])
	require.Equal(t, float64(115000), other["frt"], "request latency includes time spent on the failed candidate")
	require.Equal(t, float64(115750), other["first_valid_output_ms"])
	require.Equal(t, float64(83250), other["attempt_first_valid_output_ms"], "the route sample excludes earlier attempts and local setup")
	require.Equal(t, start.Add(1444*time.Millisecond), info.FirstResponseTime, "logging must not alter retry control timing")
}

func TestResponseTimingDoesNotInheritFailedAttemptResponse(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	info := &relaycommon.RelayInfo{
		StartTime:         start,
		FirstResponseTime: start.Add(time.Second),
		AttemptStartTime:  start.Add(10 * time.Second),
		AttemptEndTime:    start.Add(20 * time.Second),
	}
	other := make(map[string]interface{})
	appendResponseTiming(info, other)
	require.Equal(t, false, other["frt_known"])
	require.NotContains(t, other, "frt", "an unknown final response must not use a failed candidate's timing")
	require.NotContains(t, other, "first_valid_output_ms")

	// Legacy adaptors that set the request timestamp directly are still
	// supported, provided the timestamp belongs to this attempt.
	info.FirstResponseTime = start.Add(15 * time.Second)
	appendResponseTiming(info, other)
	require.Equal(t, true, other["frt_known"])
	require.Equal(t, float64(15000), other["frt"])
}

func TestResponseTimingReportsMissingTimingAsUnknown(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	for _, first := range []time.Time{{}, start.Add(-time.Second)} {
		info := &relaycommon.RelayInfo{StartTime: start, FirstResponseTime: first}
		other := make(map[string]interface{})
		appendResponseTiming(info, other)
		require.Equal(t, false, other["frt_known"])
		require.NotContains(t, other, "frt", "missing instrumentation must not produce a negative or zero measurement")
	}

	// The non-stream answer observer can provide actual output timing even
	// when the adaptor has no first-response instrumentation.
	info := &relaycommon.RelayInfo{
		StartTime:                   start,
		FirstResponseTime:           start.Add(-time.Second),
		AttemptStartTime:            start,
		AttemptDispatchTime:         start.Add(time.Second),
		AttemptFirstValidOutputTime: start.Add(2 * time.Second),
		AttemptEndTime:              start.Add(3 * time.Second),
	}
	other := make(map[string]interface{})
	appendResponseTiming(info, other)
	require.Equal(t, false, other["frt_known"])
	require.NotContains(t, other, "frt")
	require.Equal(t, float64(2000), other["first_valid_output_ms"])
	require.Equal(t, float64(1000), other["attempt_first_valid_output_ms"])
}

func TestResponseTimingPreservesZeroAndNonAttemptMeasurements(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	for _, latency := range []time.Duration{0, 2500 * time.Millisecond} {
		info := &relaycommon.RelayInfo{StartTime: start, FirstResponseTime: start.Add(latency)}
		other := make(map[string]interface{})
		appendResponseTiming(info, other)
		require.Equal(t, true, other["frt_known"])
		require.Equal(t, float64(latency.Milliseconds()), other["frt"])
	}

	other := make(map[string]interface{})
	appendResponseTiming(&relaycommon.RelayInfo{}, other)
	require.Equal(t, false, other["frt_known"])
	require.NotContains(t, other, "frt")
}
