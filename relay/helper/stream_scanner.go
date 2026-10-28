package helper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

const (
	InitialScannerBufferSize    = 64 << 10 // 64KB (64*1024)
	DefaultMaxScannerBufferSize = 64 << 20 // 64MB (64*1024*1024) default SSE buffer size
	DefaultPingInterval         = 10 * time.Second
)

func getScannerBufferSize() int {
	if constant.StreamScannerMaxBufferMB > 0 {
		return constant.StreamScannerMaxBufferMB << 20
	}
	return DefaultMaxScannerBufferSize
}

func StreamScannerHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, dataHandler func(data string, sr *StreamResult)) {
	streamScannerHandler(c, resp, info, dataHandler, true)
}

// ScanSSE consumes upstream events without writing downstream headers or pings.
// Use it when aggregating an upstream stream into a non-streaming response.
func ScanSSE(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, dataHandler func(data string, sr *StreamResult)) {
	streamScannerHandler(c, resp, info, dataHandler, false)
}

type scannedStreamEvent struct {
	data       string
	receivedAt time.Time
	endReason  relaycommon.StreamEndReason
	err        error
}

func streamScannerHandler(c *gin.Context, resp *http.Response, info *relaycommon.RelayInfo, dataHandler func(data string, sr *StreamResult), downstreamStream bool) {
	if c == nil || c.Request == nil || resp == nil || resp.Body == nil || info == nil || dataHandler == nil {
		return
	}
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
	}

	requestCtx := c.Request.Context()
	logCtx := context.WithValue(requestCtx, common.RequestIdKey, c.GetString(common.RequestIdKey))
	stopWriteCancellation := BeginStreamWriteCancellation(c)
	defer stopWriteCancellation()
	scanCtx, cancelScan := context.WithCancel(requestCtx)
	body := resp.Body
	streamingTimeout := time.Duration(constant.StreamingTimeout) * time.Second
	if streamingTimeout <= 0 {
		streamingTimeout = 30 * time.Second
	}
	ticker := time.NewTicker(streamingTimeout)
	var pingTicker *time.Ticker
	var pingTicks <-chan time.Time
	generalSettings := operation_setting.GetGeneralSetting()
	if downstreamStream && generalSettings.PingIntervalEnabled && !info.DisablePing {
		pingInterval := time.Duration(generalSettings.PingIntervalSeconds) * time.Second
		if pingInterval <= 0 {
			pingInterval = DefaultPingInterval
		}
		pingTicker = time.NewTicker(pingInterval)
		pingTicks = pingTicker.C
	}
	if downstreamStream {
		SetEventStreamHeaders(c)
	}

	// Only the caller invokes dataHandler and writes pings. A callback often
	// closes over c and provider response/usage state; returning while it is still
	// running lets Gin reuse c for another request and can splice the old answer
	// into that request. The scanner owns no Gin context or RelayInfo reference.
	events := make(chan scannedStreamEvent, 10)
	scannerStopped := make(chan struct{})
	defer func() {
		cancelScan()
		_ = body.Close() // Unblock an upstream read on cancellation or handler stop.
		ticker.Stop()
		if pingTicker != nil {
			pingTicker.Stop()
		}
		select {
		case <-scannerStopped:
		case <-time.After(5 * time.Second):
			// A custom reader may ignore Close. It holds only its private scanner,
			// immutable request context and event channel, never a client writer.
			logger.LogError(logCtx, "timeout waiting for stream scanner to exit; client writer isolated")
		}
		if info.StreamStatus.IsNormalEnd() && !info.StreamStatus.HasErrors() {
			info.MarkUpstreamContentExhausted()
			logger.LogInfo(logCtx, fmt.Sprintf("stream ended: %s", info.StreamStatus.Summary()))
		} else if info.StreamStatus.EndReason == relaycommon.StreamEndReasonClientGone {
			logger.LogInfo(logCtx, fmt.Sprintf("stream ended: %s, received=%d", info.StreamStatus.Summary(), info.ReceivedResponseCount))
		} else {
			logger.LogError(logCtx, fmt.Sprintf("stream ended: %s, received=%d", info.StreamStatus.Summary(), info.ReceivedResponseCount))
		}
	}()

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, InitialScannerBufferSize), getScannerBufferSize())
	scanner.Split(bufio.ScanLines)
	go scanStreamEvents(scanCtx, scanner, ticker, streamingTimeout, events, scannerStopped)

	timeoutTicks := ticker.C
	scannerExit := (<-chan struct{})(scannerStopped)
	sr := newStreamResult(info.StreamStatus)
	defer func() {
		if recovered := recover(); recovered != nil {
			err := fmt.Errorf("handler panic: %v", recovered)
			logger.LogError(logCtx, fmt.Sprintf("data handler panic: %v", recovered))
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPanic, err)
		}
	}()
	for {
		// Scanner completion means there is no remaining upstream read to time
		// out. Slow downstream processing must still drain the queued terminal
		// event in order, without turning a complete upstream answer into failure.
		if scannerExit != nil {
			select {
			case <-scannerExit:
				scannerExit = nil
				timeoutTicks = nil
			default:
			}
		}
		select {
		case <-requestCtx.Done():
			setCanceledStreamEnd(info.StreamStatus, requestCtx)
			return
		case <-scannerExit:
			scannerExit = nil
			timeoutTicks = nil
		case <-timeoutTicks:
			select {
			case <-scannerStopped:
				scannerExit = nil
				timeoutTicks = nil
				continue
			default:
			}
			if len(events) > 0 {
				// The bounded queue can pause upstream reads while the client is
				// slow. Buffered upstream events are not evidence of upstream silence.
				ticker.Reset(streamingTimeout)
				continue
			}
			info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonTimeout, nil)
			return
		case <-pingTicks:
			if requestCtx.Err() != nil {
				setCanceledStreamEnd(info.StreamStatus, requestCtx)
				return
			}
			if err := PingData(c); err != nil {
				logger.LogError(logCtx, "ping data error: "+err.Error())
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonPingFail, err)
				return
			}
		case event, ok := <-events:
			// Cancellation takes precedence over buffered chunks; they belong to
			// a request that can no longer receive an answer.
			if requestCtx.Err() != nil {
				setCanceledStreamEnd(info.StreamStatus, requestCtx)
				return
			}
			if !ok {
				info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
				return
			}
			if event.endReason != relaycommon.StreamEndReasonNone {
				if event.err != nil {
					logger.LogError(logCtx, "scanner error: "+event.err.Error())
				}
				info.StreamStatus.SetEndReason(event.endReason, event.err)
				return
			}
			info.SetFirstResponseTimeAt(event.receivedAt)
			info.ReceivedResponseCount++
			sr.reset()
			// Observe the original SSE error before any adaptor drops or converts
			// its structured fields. Ordinary answer/tool content is ignored.
			info.ObserveUpstreamError([]byte(event.data))
			dataHandler(event.data, sr)
			if sr.IsStopped() {
				return
			}
		}
	}
}

func setCanceledStreamEnd(status *relaycommon.StreamStatus, ctx context.Context) {
	cause := context.Cause(ctx)
	if errors.Is(cause, relaycommon.ErrAutoExplorationTimeout) {
		// The gateway ended an unverified attempt while its client is still
		// waiting. Preserve the cause so it cannot become neutral abandonment.
		status.SetEndReason(relaycommon.StreamEndReasonTimeout, cause)
		return
	}
	status.SetEndReason(relaycommon.StreamEndReasonClientGone, cause)
}

func scanStreamEvents(ctx context.Context, scanner *bufio.Scanner, ticker *time.Ticker, timeout time.Duration, events chan<- scannedStreamEvent, stopped chan<- struct{}) {
	defer close(stopped)
	defer close(events)
	send := func(event scannedStreamEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			send(scannedStreamEvent{endReason: relaycommon.StreamEndReasonPanic, err: fmt.Errorf("scanner panic: %v", recovered)})
		}
	}()
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		ticker.Reset(timeout)
		data := scanner.Text()
		if !strings.HasPrefix(data, "data:") {
			continue
		}
		data = strings.TrimSpace(strings.TrimPrefix(data, "data:"))
		if data == "" {
			continue
		}
		if strings.HasPrefix(data, "[DONE]") {
			send(scannedStreamEvent{endReason: relaycommon.StreamEndReasonDone})
			return
		}
		if !send(scannedStreamEvent{data: data, receivedAt: time.Now()}) {
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		send(scannedStreamEvent{endReason: relaycommon.StreamEndReasonScannerErr, err: err})
		return
	}
	send(scannedStreamEvent{endReason: relaycommon.StreamEndReasonEOF})
}
