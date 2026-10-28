package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFirstResponseReceiptTimeSurvivesQueuedDelivery(t *testing.T) {
	started := time.Now().Add(-time.Second)
	received := started.Add(100 * time.Millisecond)
	info := &RelayInfo{
		StartTime:        started,
		AttemptStartTime: started,
		isFirstResponse:  true,
	}
	// Processing this already-received event much later must not make the
	// upstream look slower, or overwrite its first receipt with later chunks.
	info.SetFirstResponseTimeAt(received)
	info.SetFirstResponseTimeAt(received.Add(400 * time.Millisecond))
	require.Equal(t, received, info.FirstResponseTime)
	require.Equal(t, received, info.AttemptFirstResponseTime)
}
