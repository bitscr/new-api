package common

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestRelayInfoGetFinalRequestRelayFormatPrefersExplicitFinal(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:             types.RelayFormatOpenAI,
		RequestConversionChain:  []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
		FinalRequestRelayFormat: types.RelayFormatOpenAIResponses,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatOpenAIResponses), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToConversionChain(t *testing.T) {
	info := &RelayInfo{
		RelayFormat:            types.RelayFormatOpenAI,
		RequestConversionChain: []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude},
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatClaude), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatFallsBackToRelayFormat(t *testing.T) {
	info := &RelayInfo{
		RelayFormat: types.RelayFormatGemini,
	}

	require.Equal(t, types.RelayFormat(types.RelayFormatGemini), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoGetFinalRequestRelayFormatNilReceiver(t *testing.T) {
	var info *RelayInfo
	require.Equal(t, types.RelayFormat(""), info.GetFinalRequestRelayFormat())
}

func TestRelayInfoAttemptTimingPreservesRequestTiming(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	info := &RelayInfo{
		StartTime:         start,
		FirstResponseTime: start.Add(-time.Second),
		isFirstResponse:   true,
	}
	info.BeginAttempt()
	firstAttemptStart := info.AttemptStartTime
	require.Equal(t, start, info.StartTime)
	require.Equal(t, start.Add(-time.Second), info.FirstResponseTime)
	info.SetFirstResponseTime()
	requestFirst := info.FirstResponseTime
	firstAttemptResponse := info.AttemptFirstResponseTime
	require.Equal(t, requestFirst, firstAttemptResponse)
	info.SetFirstResponseTime()
	require.Equal(t, firstAttemptResponse, info.AttemptFirstResponseTime, "only the first event sets attempt TTFB")
	info.EndAttempt()
	firstAttemptEnd := info.AttemptEndTime
	require.False(t, firstAttemptEnd.Before(firstAttemptResponse))
	require.Equal(t, firstAttemptEnd.Sub(firstAttemptStart), info.AttemptElapsed())
	info.EndAttempt()
	require.Equal(t, firstAttemptEnd, info.AttemptEndTime, "elapsed time is frozen")

	info.BeginAttempt()
	require.True(t, info.AttemptFirstResponseTime.IsZero())
	require.True(t, info.AttemptEndTime.IsZero())
	require.False(t, info.AttemptStartTime.Before(firstAttemptEnd))
	require.Equal(t, start, info.StartTime)
	require.Equal(t, requestFirst, info.FirstResponseTime)
	info.SetFirstResponseTime()
	require.False(t, info.AttemptFirstResponseTime.Before(info.AttemptStartTime))
	require.Equal(t, requestFirst, info.FirstResponseTime, "retry must not rewrite request-level first response logs")
	info.EndAttempt()
	latency, ok := info.AttemptFirstResponseLatency()
	require.True(t, ok)
	require.Equal(t, info.AttemptFirstResponseTime.Sub(info.AttemptStartTime), latency)
}

func TestRelayInfoAttemptTimingMissingAndInvalidValues(t *testing.T) {
	var info *RelayInfo
	info.BeginAttempt()
	info.EndAttempt()
	require.Zero(t, info.AttemptElapsed())
	_, ok := info.AttemptFirstResponseLatency()
	require.False(t, ok)

	info = &RelayInfo{}
	info.EndAttempt()
	require.True(t, info.AttemptEndTime.IsZero())
	_, ok = info.AttemptFirstResponseLatency()
	require.False(t, ok)
	start := time.Unix(1700000000, 0)
	info.AttemptStartTime = start
	info.AttemptEndTime = start.Add(-time.Second)
	require.Zero(t, info.AttemptElapsed())
}

func TestRelayInfoBeginAttemptResetsAnswerObservation(t *testing.T) {
	info := &RelayInfo{answer: &answerObservation{}}
	info.BeginAttempt()
	info.ObserveClientAnswer([]byte(`{"choices":[{"message":{"content":"previous answer"}}]}`))
	unusable, _ := info.ClientAnswerUnusable()
	require.False(t, unusable)
	info.EndAttempt()

	info.BeginAttempt()
	info.ObserveClientAnswer([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`))
	unusable, _ = info.ClientAnswerUnusable()
	require.True(t, unusable, "a previous attempt's answer must not hide an empty final answer")
	info.EndAttempt()

	info.BeginAttempt()
	info.ObserveClientAnswer([]byte(`{"choices":[{"message":{"content":"current answer"}}]}`))
	unusable, _ = info.ClientAnswerUnusable()
	require.False(t, unusable)

	unobserved := &RelayInfo{}
	unobserved.BeginAttempt()
	require.Nil(t, unobserved.answer, "do not enable observation on manually constructed relay info")
}
