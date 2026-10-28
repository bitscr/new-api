package common

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestUpstreamProtocolErrorPreservesOriginalFields(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"message":"Model not found","type":"invalid_request_error","param":"model","code":"model_not_found"}}`,
		`{"type":"response.failed","response":{"error":{"message":"Model not found","type":"invalid_request_error","param":"model","code":"model_not_found"}}}`,
		`{"type":"response.error","message":"Model not found","param":"model","code":"model_not_found"}`,
		`{"type":"error","message":"Model not found","param":"model","code":"model_not_found"}`,
		`{"\u0065rror":{"message":"Model not found","param":"model","code":"model_not_found"}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			info := newAnswerTestInfo()
			info.ObserveUpstreamError([]byte(payload))
			err := info.UpstreamProtocolError()
			require.NotNil(t, err)
			detail := err.ToOpenAIError()
			require.Equal(t, "Model not found", detail.Message)
			require.Equal(t, "model_not_found", detail.Code)
			require.Equal(t, "model", detail.Param)
			unusable, _ := info.ClientAnswerUnusable()
			require.True(t, unusable)
			info.ObserveUpstreamError([]byte(`{"error":{"message":"later converted error"}}`))
			require.Equal(t, "Model not found", info.UpstreamProtocolError().Error(), "lossy conversion cannot overwrite original evidence")
			info.BeginAttempt()
			require.Nil(t, info.UpstreamProtocolError(), "a retry must not inherit an earlier model's rejection")
		})
	}
}

func TestUpstreamProtocolErrorRejectsMetadataAndBoundsFields(t *testing.T) {
	for _, payload := range []string{
		`{"metadata":{"error":{"message":"model not found"}}}`,
		`{"choices":[{"delta":{"content":"model not found"}}]}`,
		`{"type":"response.completed","response":{"error":{"message":"model not found"}}}`,
		`{"type":"response.error"}`,
		`{"error":{"code":"success"}}`,
	} {
		info := newAnswerTestInfo()
		info.ObserveUpstreamError([]byte(payload))
		require.Nil(t, info.UpstreamProtocolError(), payload)
	}
	info := newAnswerTestInfo()
	info.ObserveUpstreamError([]byte(`{"error":{"message":"` + strings.Repeat("模", 3000) + `","type":"` + strings.Repeat("a", 600) + `"}}`))
	detail := info.UpstreamProtocolError().ToOpenAIError()
	require.LessOrEqual(t, len(detail.Message), answerContentLimit)
	require.LessOrEqual(t, len(detail.Type), 256)
	require.True(t, utf8.ValidString(detail.Message))
}
