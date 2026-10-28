package opencode

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type freeFlushRecorder struct {
	*httptest.ResponseRecorder
	flushErr error
	deadline time.Time
	flushes  int
}

func (w *freeFlushRecorder) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
}

func (w *freeFlushRecorder) FlushError() error {
	return w.flushErr
}

func (w *freeFlushRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func TestFreeStreamGuardPreservesFlushPolicyAndNetworkControl(t *testing.T) {
	writer := &freeFlushRecorder{ResponseRecorder: httptest.NewRecorder(), flushErr: errors.New("client flush failed")}
	c, _ := gin.CreateTestContext(writer)
	relaycommon.InstallRelayResponseWriter(c)
	stream := &freeStream{}
	guard := &freeStreamWriter{ResponseWriter: c.Writer, stream: stream}
	deadline := time.Now().Add(time.Second)
	require.NoError(t, http.NewResponseController(guard).SetWriteDeadline(deadline))
	require.Equal(t, deadline, writer.deadline, "the guard must not hide the actual network writer")
	require.ErrorIs(t, relaycommon.FlushResponseWriter(guard), writer.flushErr)
	require.Equal(t, 1, writer.flushes)

	stream.fail(errors.New("upstream protocol failed"))
	require.NoError(t, relaycommon.FlushResponseWriter(guard))
	require.Equal(t, 1, writer.flushes, "a suppressed success terminator must not flush the client")
}
