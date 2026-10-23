package controller

import (
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/require"
)

func TestQueueAutoModelFeedbackPinsObservationTime(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	info := &relaycommon.RelayInfo{
		UsingGroup: "feedback-time", OriginModelName: "m",
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 61},
	}
	before := time.Now()
	queueAutoModelFeedback(c, info, autoModelFeedback{success: true, latencyKnown: true, latencyMS: 10})
	after := time.Now()

	pending := takeAutoModelFeedback(c)
	require.Len(t, pending, 1)
	for _, feedback := range pending {
		require.False(t, feedback.observedAt.Before(before))
		require.False(t, feedback.observedAt.After(after),
			"the observation time is fixed at enqueue, not at a later flush")
	}
}

func TestTrialSlotStaysHeldThroughFeedbackFlush(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	route := service.AutoModelRoute{Group: "trial-flush", ModelName: "m", ChannelID: 62}
	release, ok := service.AcquireAutoModelTrialSlot(route)
	require.True(t, ok)
	holdAutoModelTrialSlot(c, route, release)

	_, second := service.AcquireAutoModelTrialSlot(route)
	require.False(t, second, "the slot is still held before its feedback is published")

	info := &relaycommon.RelayInfo{
		UsingGroup: route.Group, OriginModelName: route.ModelName,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: route.ChannelID},
	}
	queueAutoModelFeedback(c, info, autoModelFeedback{success: true, latencyKnown: true, latencyMS: 12, elapsedMS: 12})
	flushAutoModelFeedback(c)

	_, after := service.AcquireAutoModelTrialSlot(route)
	require.True(t, after, "flush publishes feedback and then releases the trial slot")
}

func TestPreDispatchRejectionReleasesTrialSlotImmediately(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	route := service.AutoModelRoute{Group: "trial-pre-dispatch", ModelName: "m", ChannelID: 63}
	release, ok := service.AcquireAutoModelTrialSlot(route)
	require.True(t, ok)
	holdAutoModelTrialSlot(c, route, release)

	releaseAutoModelTrialSlot(c, route)
	_, again := service.AcquireAutoModelTrialSlot(route)
	require.True(t, again, "a pre-dispatch rejection must free the slot without feedback")
}
