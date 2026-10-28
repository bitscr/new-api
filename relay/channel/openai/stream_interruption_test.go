package openai

import (
	"context"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestOaiStreamHandlerDoesNotCompleteInterruptedStream(t *testing.T) {
	c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatOpenAI}, true)
	resp := observationResponse("", true)
	resp.Body = io.NopCloser(io.MultiReader(
		strings.NewReader("data: "+observationPartial+"\n\n"),
		iotest.ErrReader(io.ErrUnexpectedEOF),
	))
	usage, apiErr := OaiStreamHandler(c, info, resp)
	require.Nil(t, apiErr, "the relay controller owns stream failure handling")
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonScannerErr, info.StreamStatus.EndReason)
	require.ErrorIs(t, info.StreamStatus.EndError, io.ErrUnexpectedEOF)
	require.Contains(t, w.Body.String(), "partial", "retain the last real upstream chunk")
	require.NotContains(t, w.Body.String(), "[DONE]", "a truncated answer must not be presented as complete")
}

func TestOaiStreamHandlerCanceledEmptyStreamDoesNotWrite(t *testing.T) {
	c, w, info := newObservedAnswerContext(t, observationFormat{format: types.RelayFormatOpenAI}, true)
	ctx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	cancel()
	usage, apiErr := OaiStreamHandler(c, info, observationResponse("", true))
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	require.Equal(t, relaycommon.StreamEndReasonClientGone, info.StreamStatus.EndReason)
	require.Empty(t, w.Body.String(), "do not emit a success terminator after client cancellation")
}
