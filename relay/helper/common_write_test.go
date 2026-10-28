package helper

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type failingEventWriter struct {
	*httptest.ResponseRecorder
	failWrite int
	writes    int
	err       error
}

func (w *failingEventWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes == w.failWrite {
		return 0, w.err
	}
	return w.ResponseRecorder.Write(data)
}

func TestStringDataReturnsEventWriteFailure(t *testing.T) {
	for _, failWrite := range []int{1, 2} {
		t.Run(fmt.Sprintf("write=%d", failWrite), func(t *testing.T) {
			writer := &failingEventWriter{ResponseRecorder: httptest.NewRecorder(), failWrite: failWrite, err: errors.New("client event write failed")}
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			require.ErrorIs(t, StringData(ctx, `{"answer":"local"}`), writer.err)
			require.False(t, writer.Flushed, "a failed event must not be treated as successfully flushed")
		})
	}
}

func TestStringDataKeepsWireFormatAndCapture(t *testing.T) {
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	capture := relaycommon.NewConversationCapture()
	relaycommon.SetConversationCapture(ctx, capture)
	const payload = `{"answer":"local"}`
	require.NoError(t, StringData(ctx, payload))
	want := "data: " + payload + "\n\n"
	require.Equal(t, want, writer.Body.String())
	require.Equal(t, want, string(capture.Snapshot().ClientResponseBody))
	require.True(t, writer.Flushed)
	require.Equal(t, "text/event-stream", writer.Header().Get("Content-Type"))
}
