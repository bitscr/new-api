package controller

import (
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/require"
)

// TestObservedAutoLatency 锁定 auto 评分延迟信号的来源：
// 流式用首字节时间，非流式没有首字节时间时必须退化成总耗时。
// 若非流式返回 0，一次 75 秒的"成功"会被记成零延迟的完美候选，
// 排在前面的慢渠道就会一直压住没观测过的候选，auto 每次都会再选它。
func TestObservedAutoLatency(t *testing.T) {
	start := time.Now().Add(-5 * time.Second)

	streamed := &relaycommon.RelayInfo{
		StartTime:         start,
		FirstResponseTime: start.Add(2 * time.Second),
	}
	require.InDelta(t, 2000, observedAutoLatency(streamed, true), 200, "流式用 TTFB")

	nonStream := &relaycommon.RelayInfo{StartTime: time.Now().Add(-70 * time.Second)}
	require.GreaterOrEqual(t, observedAutoLatency(nonStream, true), int64(69000), "非流式退化成总耗时")

	require.Zero(t, observedAutoLatency(nonStream, false), "失败请求不产生延迟信号")
	require.Zero(t, observedAutoLatency(&relaycommon.RelayInfo{}, true), "没有开始时间时返回 0")
	require.Zero(t, observedAutoLatency(nil, true))
}
