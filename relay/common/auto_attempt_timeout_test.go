package common

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAutoExplorationTimeoutStartsOnlyAtDispatch(t *testing.T) {
	parent := context.Background()
	ctx, guard := NewAutoExplorationAttempt(parent, 20*time.Millisecond)
	defer guard.Finish()
	select {
	case <-ctx.Done():
		t.Fatal("local validation must not consume the upstream wait budget")
	case <-time.After(40 * time.Millisecond):
	}
	info := &RelayInfo{AutoExplorationAttempt: guard}
	info.BeginAttempt()
	info.MarkUpstreamDispatch()
	select {
	case <-ctx.Done():
		require.ErrorIs(t, context.Cause(ctx), ErrAutoExplorationTimeout)
		require.NoError(t, parent.Err(), "attempt timeout must leave the client context alive for fallback")
	case <-time.After(time.Second):
		t.Fatal("unverified zero-output attempt did not expire")
	}
}

func TestAutoExplorationTimeoutStopsOnCommittedOutput(t *testing.T) {
	_, recorder, info, writer := newAutoAttemptStagingTest(t, true)
	ctx, guard := NewAutoExplorationAttempt(context.Background(), 30*time.Millisecond)
	defer guard.Finish()
	info.AutoExplorationAttempt = guard
	writer.SetExplorationAttempt(guard)
	info.BeginAttempt()
	info.MarkUpstreamDispatch()
	defer writer.Restore()
	_, err := writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	require.NoError(t, err)
	require.True(t, writer.Committed())
	require.Contains(t, recorder.Body.String(), "hello")
	select {
	case <-ctx.Done():
		t.Fatal("valid output must protect the rest of a slow stream")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestAutoExplorationTimeoutRejectsLateFirstOutput(t *testing.T) {
	_, recorder, info, writer := newAutoAttemptStagingTest(t, true)
	ctx, guard := NewAutoExplorationAttempt(context.Background(), time.Millisecond)
	defer guard.Finish()
	info.AutoExplorationAttempt = guard
	writer.SetExplorationAttempt(guard)
	info.BeginAttempt()
	info.MarkUpstreamDispatch()
	defer writer.Restore()
	select {
	case <-ctx.Done():
		require.ErrorIs(t, context.Cause(ctx), ErrAutoExplorationTimeout)
	case <-time.After(time.Second):
		t.Fatal("attempt deadline did not expire")
	}
	_, err := writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"too late\"}}]}\n\n"))
	require.ErrorIs(t, err, ErrAutoExplorationTimeout)
	require.False(t, writer.Committed(), "expired upstream output must remain retryable")
	require.Empty(t, recorder.Body.String(), "a canceled partial stream must never reach the client")
	require.True(t, guard.Finish(), "finish must report expiry atomically even if the handler returns at the deadline")
}

func TestAutoExplorationCompletedAttemptCanCommitStagedBody(t *testing.T) {
	_, recorder, info, writer := newAutoAttemptStagingTest(t, false)
	_, guard := NewAutoExplorationAttempt(context.Background(), time.Second)
	info.AutoExplorationAttempt = guard
	writer.SetExplorationAttempt(guard)
	info.BeginAttempt()
	info.MarkUpstreamDispatch()
	defer writer.Restore()
	// An unclassified response can stay staged until its handler returns.
	_, err := writer.Write([]byte(`{"result":"unclassified"}`))
	require.NoError(t, err)
	require.False(t, writer.Committed())
	require.False(t, guard.Finish(), "completed attempt must win against a timer that has not expired")
	info.AutoExplorationAttempt = nil
	require.NoError(t, writer.Commit(), "finishing an unexpired attempt must preserve staged delivery")
	require.Equal(t, `{"result":"unclassified"}`, recorder.Body.String())
}
