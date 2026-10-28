package helper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type blockedDeadlineWriter struct {
	*httptest.ResponseRecorder
	started  chan struct{}
	unblock  chan struct{}
	start    sync.Once
	release  sync.Once
	mu       sync.Mutex
	deadline time.Time
}

func (w *blockedDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	w.mu.Unlock()
	if !deadline.IsZero() && !deadline.After(time.Now()) {
		w.release.Do(func() { close(w.unblock) })
	}
	return nil
}

func (w *blockedDeadlineWriter) Write(data []byte) (int, error) {
	w.start.Do(func() { close(w.started) })
	<-w.unblock
	return 0, context.DeadlineExceeded
}

func TestStreamWriteCancellationTraversesRelayWrappersAndJoins(t *testing.T) {
	w := &blockedDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), unblock: make(chan struct{})}
	c, _ := gin.CreateTestContext(w)
	requestCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestCtx)
	relaycommon.InstallRelayResponseWriter(c)
	staged := relaycommon.InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	stop := BeginStreamWriteCancellation(c)
	defer stop()
	writeFinished := make(chan error, 1)
	go func() {
		controller := http.NewResponseController(c.Writer)
		err := controller.SetWriteDeadline(time.Now().Add(time.Minute))
		if err == nil {
			_, err = c.Writer.Write([]byte("old machine environment"))
			if err == nil {
				err = staged.Commit()
			}
		}
		writeFinished <- err
	}()
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("deadline controller did not reach the network writer")
	}
	cancel()
	select {
	case err := <-writeFinished:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt the blocked write")
	}
	stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	require.True(t, w.deadline.IsZero(), "canceled attempt must not leave an expired deadline on fallback")
}

func TestPingDataSetsNetworkWriteDeadline(t *testing.T) {
	w := &blockedDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), unblock: make(chan struct{})}
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	relaycommon.InstallRelayResponseWriter(c)
	stop := BeginStreamWriteCancellation(c)
	defer stop()
	finished := make(chan error, 1)
	go func() { finished <- PingData(c) }()
	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("ping did not enter its writer")
	}
	w.mu.Lock()
	deadline := w.deadline
	w.mu.Unlock()
	require.WithinDuration(t, time.Now().Add(streamPingWriteTimeout), deadline, time.Second)
	cancel()
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("canceled ping write survived")
	}
}

func TestExplorationTimeoutDoesNotExpireClientWriteDeadline(t *testing.T) {
	w := &blockedDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), unblock: make(chan struct{})}
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancelCause(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)
	relaycommon.InstallRelayResponseWriter(c)
	stop := BeginStreamWriteCancellation(c)
	cancel(relaycommon.ErrAutoExplorationTimeout)
	stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	require.True(t, w.deadline.IsZero(), "HTTP/2 fallback must retain its live client stream")
	select {
	case <-w.unblock:
		t.Fatal("exploration canceled the client network writer")
	default:
	}
}
