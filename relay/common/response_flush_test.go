package common

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type flushErrorRecorder struct {
	*httptest.ResponseRecorder
	err        error
	flushes    int
	errorReads int
}

func (w *flushErrorRecorder) Flush() {
	w.flushes++
	w.ResponseRecorder.Flush()
}

func (w *flushErrorRecorder) FlushError() error {
	w.errorReads++
	return w.err
}

func TestRelayFlushReportsDeliveryFailureAfterSuccessfulWrite(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "successful_flush"
		if fail {
			name = "failed_flush"
		}
		t.Run(name, func(t *testing.T) {
			writer := &flushErrorRecorder{ResponseRecorder: httptest.NewRecorder()}
			if fail {
				writer.err = errors.New("client gone during flush")
			}
			c, _ := gin.CreateTestContext(writer)
			info := &RelayInfo{answer: &answerObservation{}}
			SetRelayInfo(c, info)
			InstallRelayResponseWriter(c)
			_, err := c.Writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"))
			require.NoError(t, err, "a buffered write alone cannot reveal network failure")
			require.False(t, info.ClientDeliveryBroken())
			err = FlushResponseWriter(c.Writer)
			if fail {
				require.ErrorIs(t, err, writer.err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, fail, info.ClientDeliveryBroken())
			require.Equal(t, 1, writer.flushes)
			require.Equal(t, 1, writer.errorReads)
		})
	}
}

func TestErrorAwareFlushDoesNotCommitUnvalidatedAutoAttempt(t *testing.T) {
	writer := &flushErrorRecorder{ResponseRecorder: httptest.NewRecorder(), err: errors.New("client flush failed")}
	c, _ := gin.CreateTestContext(writer)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &RelayInfo{IsStream: true, answer: &answerObservation{}}
	SetRelayInfo(c, info)
	InstallRelayResponseWriter(c)
	info.BeginAttempt()
	staged := InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	_, err := staged.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	require.NoError(t, err)
	require.NoError(t, FlushResponseWriter(c.Writer))
	require.Zero(t, writer.flushes, "flush must not bypass the staging writer")
	require.Zero(t, writer.errorReads)
	require.False(t, staged.Committed())
	require.False(t, writer.Flushed)
	require.Zero(t, writer.Body.Len())
	require.False(t, info.ClientDeliveryBroken())

	_, err = staged.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"healthy answer\"}}]}\n\n"))
	require.NoError(t, err)
	require.True(t, staged.Committed())
	require.ErrorIs(t, FlushResponseWriter(c.Writer), writer.err)
	require.True(t, info.ClientDeliveryBroken())
	require.Equal(t, 1, writer.flushes)
	require.Equal(t, 1, writer.errorReads)
}

func TestLegacyGinFlushStillWritesPendingStatus(t *testing.T) {
	writer := &flushErrorRecorder{ResponseRecorder: httptest.NewRecorder()}
	c, _ := gin.CreateTestContext(writer)
	c.Writer.WriteHeader(http.StatusAccepted)
	require.False(t, c.Writer.Written())
	require.NoError(t, FlushResponseWriter(c.Writer))
	require.True(t, c.Writer.Written())
	require.Equal(t, http.StatusAccepted, writer.Code)
	require.Equal(t, 1, writer.flushes)
	require.Equal(t, 1, writer.errorReads)
}
