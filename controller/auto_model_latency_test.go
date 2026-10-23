package controller

import (
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

func TestObservedAutoLatencyUsesFirstValidOutputForStreams(t *testing.T) {
	start := time.Unix(1700000000, 0)
	streamed := &relaycommon.RelayInfo{
		StartTime:                   start.Add(-time.Minute),
		FirstResponseTime:           start.Add(-30 * time.Second),
		IsStream:                    true,
		AttemptStartTime:            start,
		AttemptFirstResponseTime:    start.Add(200 * time.Millisecond),
		AttemptFirstValidOutputTime: start.Add(2 * time.Second),
		AttemptEndTime:              start.Add(90 * time.Second),
	}
	require.Equal(t, int64(2000), observedAutoLatency(streamed, true), "stream speed is first valid output, not first byte")
	require.Equal(t, int64(2000), observedAutoModelCooldownLatency(streamed, true))
	require.Equal(t, int64(90000), observedAutoModelElapsed(streamed))
	require.Equal(t, int64(90000), observedAutoModelCooldownLatency(streamed, false))
	require.Zero(t, observedAutoLatency(streamed, false))

	nonStream := &relaycommon.RelayInfo{
		StartTime:        start.Add(-time.Minute),
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(3 * time.Second),
	}
	require.Equal(t, int64(3000), observedAutoLatency(nonStream, true), "non-stream uses completion duration")
	require.Equal(t, int64(3000), observedAutoModelCooldownLatency(nonStream, true))
	require.Zero(t, observedAutoLatency(&relaycommon.RelayInfo{StartTime: start}, true))
	require.Zero(t, observedAutoLatency(nil, true))
	require.Zero(t, observedAutoModelElapsed(nil))
	require.Zero(t, observedAutoModelCooldownLatency(nil, false))
}

func TestObservedAutoLatencyStartsAtRealDispatch(t *testing.T) {
	start := time.Unix(1700000000, 0)
	dispatched := start.Add(time.Second)
	info := &relaycommon.RelayInfo{
		IsStream:                    true,
		AttemptStartTime:            start,
		AttemptDispatchTime:         dispatched,
		AttemptFirstResponseTime:    start.Add(1200 * time.Millisecond),
		AttemptFirstValidOutputTime: start.Add(2500 * time.Millisecond),
		AttemptEndTime:              start.Add(5 * time.Second),
	}
	require.Equal(t, int64(1500), observedAutoLatency(info, true),
		"stream speed must exclude local conversion before dispatch")
	require.Equal(t, int64(1500), observedAutoModelCooldownLatency(info, true))
	require.Equal(t, int64(5000), observedAutoModelElapsed(info),
		"full attempt elapsed remains available for logs")

	nonStream := &relaycommon.RelayInfo{
		AttemptStartTime:    start,
		AttemptDispatchTime: dispatched,
		AttemptEndTime:      start.Add(3 * time.Second),
	}
	require.Equal(t, int64(2000), observedAutoLatency(nonStream, true),
		"non-stream completion time must also start at dispatch")
}

func TestObservedAutoLatencyRejectsStreamWithoutValidOutput(t *testing.T) {
	start := time.Unix(1700000000, 0)
	for _, first := range []time.Time{time.Time{}, start.Add(-time.Second), start.Add(2 * time.Minute)} {
		info := &relaycommon.RelayInfo{
			StartTime:                   start.Add(-time.Minute),
			IsStream:                    true,
			AttemptStartTime:            start,
			AttemptFirstResponseTime:    start.Add(100 * time.Millisecond),
			AttemptFirstValidOutputTime: first,
			AttemptEndTime:              start.Add(time.Minute),
		}
		require.Zero(t, observedAutoLatency(info, true), "missing/stale valid output must be unknown")
		require.Zero(t, observedAutoModelCooldownLatency(info, true))
	}
}
