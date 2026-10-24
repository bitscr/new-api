package common

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newAutoAttemptStagingTest(t *testing.T, stream bool) (*gin.Context, *httptest.ResponseRecorder, *RelayInfo, *AutoAttemptResponseWriter) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &RelayInfo{IsStream: stream, answer: &answerObservation{}}
	SetRelayInfo(c, info)
	InstallRelayResponseWriter(c)
	info.BeginAttempt()
	staged := InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	return c, recorder, info, staged
}

func TestAutoAttemptStagingDropsBannerAndDeliversHealthyCandidate(t *testing.T) {
	banner := `{"choices":[{"index":0,"message":{"role":"assistant","content":"[Gateway Warning: busy]"},"finish_reason":"stop"}]}`
	healthy := `{"choices":[{"index":0,"message":{"role":"assistant","content":"real answer"},"finish_reason":"stop"}]}`

	c, recorder, info, staged := newAutoAttemptStagingTest(t, false)
	_, err := staged.Write([]byte(banner))
	require.NoError(t, err)
	require.False(t, staged.Committed(), "a banner-only non-stream response must not be committed")
	require.Zero(t, recorder.Body.Len(), "nothing may reach the client before validation")
	unusable, reason := info.ClientAnswerUnusable()
	require.True(t, unusable, reason)

	staged.Discard()
	staged.Restore()
	require.Zero(t, recorder.Body.Len())

	second := InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, second)
	_, err = second.Write([]byte(healthy))
	require.NoError(t, err)
	require.True(t, second.Committed(), "healthy output is committed immediately")
	require.Contains(t, recorder.Body.String(), "real answer")
	require.NotContains(t, recorder.Body.String(), "Gateway Warning")
}

func TestAutoAttemptStagingStreamWaitsForValidOutput(t *testing.T) {
	_, recorder, info, staged := newAutoAttemptStagingTest(t, true)
	write := func(body string) {
		_, err := staged.Write([]byte(body))
		require.NoError(t, err)
	}
	write("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"[Gateway Warning: busy]\"}}]}\n\n")
	require.False(t, staged.Committed(), "role and banner frames must stay staged")
	require.Zero(t, recorder.Body.Len())
	staged.Flush() // must not commit before valid output
	require.Zero(t, recorder.Body.Len())

	write("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
	require.True(t, staged.Committed(), "the first valid output releases the buffered stream")
	require.Contains(t, recorder.Body.String(), "hello")
	require.Contains(t, recorder.Body.String(), "Gateway Warning", "buffered prefix is delivered verbatim")

	write("data: [DONE]\n\n")
	require.True(t, strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n"))
	unusable, reason := info.ClientAnswerUnusable()
	require.False(t, unusable, "a delivered healthy stream remains usable: %s", reason)
}

func TestAutoAttemptStagingBufferLimitCommitsAndPreventsSwitch(t *testing.T) {
	_, recorder, _, staged := newAutoAttemptStagingTest(t, false)
	bigBanner := `{"choices":[{"index":0,"message":{"content":"[Gateway Warning: ` +
		strings.Repeat("x", autoAttemptResponseBufferLimit) + `]"}}]}`
	_, err := staged.Write([]byte(bigBanner))
	require.NoError(t, err)
	require.True(t, staged.Committed(), "the size cap must commit instead of buffering forever")
	require.True(t, staged.Overflow())
	require.NotZero(t, recorder.Body.Len())
}

func TestAutoAttemptStagingDiscardRestoresHeaders(t *testing.T) {
	c, _, _, staged := newAutoAttemptStagingTest(t, false)
	c.Writer.Header().Set("X-Attempt-Only", "1")
	staged.Discard()
	staged.Restore()
	require.Empty(t, c.Writer.Header().Get("X-Attempt-Only"),
		"headers set by a discarded candidate must not leak into the next one")
}

func TestAutoAttemptStagingBufferNeverExceedsLimit(t *testing.T) {
	_, recorder, _, staged := newAutoAttemptStagingTest(t, false)
	prefix := strings.Repeat("a", autoAttemptResponseBufferLimit-10)
	_, err := staged.Write([]byte(prefix))
	require.NoError(t, err)
	require.False(t, staged.Committed())
	require.Equal(t, len(prefix), staged.StagedBytes())

	tail := strings.Repeat("b", autoAttemptResponseBufferLimit*2)
	_, err = staged.Write([]byte(tail))
	require.NoError(t, err)
	require.True(t, staged.Committed())
	require.True(t, staged.Overflow())
	require.Zero(t, staged.StagedBytes(), "the staged prefix is released once the cap is crossed")
	require.Equal(t, len(prefix)+len(tail), recorder.Body.Len(),
		"the overflow chunk is streamed through, never appended to the staging buffer")
}

func TestAutoAttemptStagingDropsKeepAliveBeforeCommit(t *testing.T) {
	_, recorder, _, staged := newAutoAttemptStagingTest(t, true)
	_, err := staged.Write(autoAttemptKeepAliveData)
	require.NoError(t, err)
	require.False(t, staged.Committed())
	require.Zero(t, staged.StagedBytes(), "a keepalive must not consume staging space")
	require.Zero(t, recorder.Body.Len())

	_, err = staged.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	require.NoError(t, err)
	require.True(t, staged.Committed())
	require.NotContains(t, recorder.Body.String(), ": PING",
		"a dropped keepalive must not be replayed with the committed candidate")
	require.Contains(t, recorder.Body.String(), "hello")
}

func TestAutoAttemptStagingSerializesConcurrentWriters(t *testing.T) {
	_, recorder, _, staged := newAutoAttemptStagingTest(t, true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, _ = staged.Write(autoAttemptKeepAliveData)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			_, _ = staged.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n"))
		}
	}()
	wg.Wait()
	require.True(t, staged.Committed())
	require.Contains(t, recorder.Body.String(), "\"content\":\"x\"")
}

func TestAutoAttemptStagingWriteAfterDiscardFails(t *testing.T) {
	_, _, _, staged := newAutoAttemptStagingTest(t, false)
	staged.Discard()
	_, err := staged.Write(autoAttemptKeepAliveData)
	require.Error(t, err, "a stale keepalive must stop instead of writing through")
}

func TestClientAnswerUndeterminedAfterUnclassifiedOutput(t *testing.T) {
	_, recorder, info, staged := newAutoAttemptStagingTest(t, false)
	unknown := strings.Repeat("x", autoAttemptResponseBufferLimit*2)
	_, err := staged.Write([]byte(unknown))
	require.NoError(t, err)
	require.True(t, staged.Committed())
	require.True(t, staged.Overflow())
	require.True(t, info.ClientAnswerUndetermined(),
		"a committed answer the observer cannot classify must not be credited")
	require.False(t, info.ClientAnswerHasValidOutput())
	require.Equal(t, len(unknown), recorder.Body.Len())

	_, err = staged.Write([]byte(`{"choices":[{"index":0,"message":{"content":"real answer"},"finish_reason":"stop"}]}`))
	require.NoError(t, err)
	require.False(t, info.ClientAnswerUndetermined())
	require.True(t, info.ClientAnswerHasValidOutput())
}
