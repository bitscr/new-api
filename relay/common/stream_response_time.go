package common

import "time"

// SetFirstResponseTimeAt preserves upstream receipt time when a scanner queues
// an event before the caller handles it. Queueing must not inflate upstream FRT.
func (info *RelayInfo) SetFirstResponseTimeAt(receivedAt time.Time) {
	if !info.AttemptStartTime.IsZero() && info.AttemptEndTime.IsZero() && info.AttemptFirstResponseTime.IsZero() {
		info.AttemptFirstResponseTime = receivedAt
	}
	if info.isFirstResponse {
		info.FirstResponseTime = receivedAt
		info.isFirstResponse = false
	}
}
