package helper

import (
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// A discarded candidate restores its pre-attempt headers. The next candidate
// must be able to install SSE headers again, which requires SetEventStreamHeaders
// to be idempotent instead of gated by a context flag that survives the discard.
func TestSetEventStreamHeadersAfterDiscardedCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	relaycommon.SetRelayInfo(c, &relaycommon.RelayInfo{IsStream: true})
	relaycommon.InstallRelayResponseWriter(c)

	staged := relaycommon.InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	SetEventStreamHeaders(c)
	require.Equal(t, "text/event-stream", c.Writer.Header().Get("Content-Type"))

	staged.Discard()
	staged.Restore()
	require.Empty(t, c.Writer.Header().Get("Content-Type"),
		"the discarded candidate's SSE headers must be restored away")

	SetEventStreamHeaders(c)
	require.Equal(t, "text/event-stream", c.Writer.Header().Get("Content-Type"),
		"the replacement candidate must be able to set SSE headers again")
}
