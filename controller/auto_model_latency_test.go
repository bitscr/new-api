package controller

import (
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

func TestObservedAutoLatency(t *testing.T) {
	start := time.Unix(1700000000, 0)
	streamed := &relaycommon.RelayInfo{
		StartTime:                start.Add(-time.Minute),
		FirstResponseTime:        start.Add(-30 * time.Second), // failed previous attempt
		IsStream:                 true,
		AttemptStartTime:         start,
		AttemptFirstResponseTime: start.Add(2 * time.Second),
		AttemptEndTime:           start.Add(90 * time.Second),
	}
	require.Equal(t, int64(2000), observedAutoLatency(streamed, true), "TTFB belongs to this attempt")
	require.Equal(t, int64(2000), observedAutoModelCooldownLatency(streamed, true), "long generation must not cause slow cooldown")
	require.Equal(t, int64(90000), observedAutoModelElapsed(streamed))
	require.Equal(t, int64(90000), observedAutoModelCooldownLatency(streamed, false), "failure uses this attempt's full elapsed time")
	require.Zero(t, observedAutoLatency(streamed, false), "failed requests do not produce a successful latency sample")

	nonStream := &relaycommon.RelayInfo{
		StartTime:         start.Add(-time.Minute),
		FirstResponseTime: start.Add(-30 * time.Second),
		AttemptStartTime:  start,
		AttemptEndTime:    start.Add(3 * time.Second),
	}
	require.Equal(t, int64(3000), observedAutoLatency(nonStream, true), "non-stream elapsed excludes preceding attempts")
	require.Equal(t, int64(3000), observedAutoModelCooldownLatency(nonStream, true))
	require.Zero(t, observedAutoLatency(&relaycommon.RelayInfo{StartTime: start}, true), "request timing is not an attempt timing fallback")
	require.Zero(t, observedAutoLatency(nil, true))
	require.Zero(t, observedAutoModelElapsed(nil))
	require.Zero(t, observedAutoModelCooldownLatency(nil, false))
}

func TestObservedAutoLatencyRejectsStaleAndInvalidStreamFirstResponse(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, first := range []time.Time{time.Time{}, start.Add(-time.Second), start.Add(2 * time.Minute)} {
		info := &relaycommon.RelayInfo{
			StartTime:                start.Add(-time.Minute),
			FirstResponseTime:        start.Add(-30 * time.Second),
			IsStream:                 true,
			AttemptStartTime:         start,
			AttemptFirstResponseTime: first,
			AttemptEndTime:           start.Add(time.Minute),
		}
		require.Zero(t, observedAutoLatency(info, true), "missing/stale/out-of-window first response must not become a latency sample")
		require.Zero(t, observedAutoModelCooldownLatency(info, true), "do not substitute long generation time for unknown TTFB")
	}
}

func TestObservedAutoLatencySupportsLegacyDirectFirstResponse(t *testing.T) {
	start := time.Unix(1700000000, 0)
	info := &relaycommon.RelayInfo{
		StartTime:         start.Add(-time.Minute),
		FirstResponseTime: start.Add(time.Second),
		IsStream:          true,
		AttemptStartTime:  start,
		AttemptEndTime:    start.Add(time.Minute),
	}
	require.Equal(t, int64(1000), observedAutoLatency(info, true))
	require.Equal(t, int64(1000), observedAutoModelCooldownLatency(info, true))
}
