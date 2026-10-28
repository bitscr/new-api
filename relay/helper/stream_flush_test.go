package helper

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type failingFlushRecorder struct {
	*httptest.ResponseRecorder
	err error
}

func (w *failingFlushRecorder) FlushError() error {
	return w.err
}

func TestStringDataReturnsNetworkFlushErrorThroughRelay(t *testing.T) {
	writer := &failingFlushRecorder{ResponseRecorder: httptest.NewRecorder(), err: errors.New("client disconnected during flush")}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	relaycommon.InstallRelayResponseWriter(c)
	require.ErrorIs(t, StringData(c, `{"choices":[{"delta":{"content":"answer"}}]}`), writer.err)
	require.Contains(t, writer.Body.String(), "answer", "the preceding write succeeded")
}

func TestFlushWriterReturnsNetworkFlushErrorThroughBareGin(t *testing.T) {
	writer := &failingFlushRecorder{ResponseRecorder: httptest.NewRecorder(), err: errors.New("client disconnected during flush")}
	c, _ := gin.CreateTestContext(writer)
	require.ErrorIs(t, FlushWriter(c), writer.err)
}
