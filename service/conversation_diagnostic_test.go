package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestConversationDiagnosticFingerprintsFullContextAndSessionSources(t *testing.T) {
	client := []byte(`{"messages":[{"role":"system","content":"private_machine_A"},{"role":"user","content":"same_question"}],"prompt_cache_key":"private_shared_cache","metadata":{"session_id":"private_client_session"}}`)
	upstream := []byte(`{"messages":[{"role":"system","content":"private_machine_A"},{"role":"user","content":"same_question"}],"prompt_cache_key":"private_shared_cache","metadata":{"session_id":"private_override_session"}}`)
	fields := conversationDiagnosticFields(relaycommon.ConversationCaptureSnapshot{
		ClientRequestBody: client, UpstreamRequestBody: upstream,
		UpstreamResponseBody: []byte("private_upstream_answer"), ClientResponseBody: []byte("private_delivered_answer"),
	})
	for _, private := range []string{"private_machine_A", "same_question", "private_shared_cache", "private_client_session", "private_override_session", "private_upstream_answer", "private_delivered_answer"} {
		require.NotContains(t, fields, private)
	}
	require.NotContains(t, fields, "\n")
	require.Contains(t, fields, "client_request_sha256="+conversationBodyFingerprint(client))
	require.Contains(t, fields, "upstream_request_sha256="+conversationBodyFingerprint(upstream))
	shared := conversationBodyFingerprint([]byte("private_shared_cache"))
	require.Contains(t, fields, "client_prompt_cache_key_sha256="+shared)
	require.Contains(t, fields, "upstream_prompt_cache_key_sha256="+shared)
	require.Contains(t, fields, "client_metadata_session_id_sha256="+conversationBodyFingerprint([]byte("private_client_session")))
	require.Contains(t, fields, "upstream_metadata_session_id_sha256="+conversationBodyFingerprint([]byte("private_override_session")))
	otherClient := []byte(strings.ReplaceAll(string(client), "private_machine_A", "private_machine_B"))
	require.NotEqual(t, conversationBodyFingerprint(client), conversationBodyFingerprint(otherClient), "same last question must not hide different complete contexts")
}

func TestConversationDiagnosticMissingStageIsUnknown(t *testing.T) {
	fields := conversationDiagnosticFields(relaycommon.ConversationCaptureSnapshot{})
	require.Contains(t, fields, "upstream_response_bytes=0 upstream_response_sha256=?")
	require.NotContains(t, fields, "session_id_sha256=")
}

func TestConversationHeaderDiagnosticTracksFinalValuesWithoutSecrets(t *testing.T) {
	client := http.Header{"Session_id": {"private_client_session"}, "Authorization": {"Bearer private_secret"}}
	upstream := http.Header{"Session_id": {"private_static_override"}, "X-Grok-Conv-Id": {"private_client_session"}, "Authorization": {"Bearer private_upstream_secret"}}
	fields := conversationHeaderDiagnosticFields(client, upstream)
	require.Contains(t, fields, "header_session_id_changed=true")
	require.Contains(t, fields, "header_session_id_client_sha256="+conversationBodyFingerprint([]byte("private_client_session")))
	require.Contains(t, fields, "header_session_id_upstream_sha256="+conversationBodyFingerprint([]byte("private_static_override")))
	require.Contains(t, fields, "header_x_grok_conv_id_changed=true")
	require.NotContains(t, fields, "private_")
	require.NotContains(t, fields, "Authorization")
	require.Empty(t, conversationHeaderDiagnosticFields(http.Header{}, http.Header{}))
}

func TestConversationHeaderDiagnosticRetainedForExport(t *testing.T) {
	for _, withSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("session=%t", withSession), func(t *testing.T) {
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			capture := relaycommon.NewConversationCapture()
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}, ConversationCapture: capture}
			upstream := http.Header{"Authorization": {"Bearer private_upstream_secret"}}
			if withSession {
				ctx.Request.Header.Set("X-Session-Id", "private_client_session")
				upstream.Set("X-Session-Id", "private_static_override")
			}
			LogConversationUpstreamHeaderDiagnostic(ctx, info, upstream)
			fields := capture.Snapshot().UpstreamSessionHeaderDiagnostic
			require.NotContains(t, fields, "private_")
			require.NotContains(t, fields, "Authorization")
			if withSession {
				require.Contains(t, fields, "checked=true")
				require.Contains(t, fields, "header_x_session_id_changed=true")
				require.Contains(t, fields, "header_x_session_id_upstream_sha256="+conversationBodyFingerprint([]byte("private_static_override")))
			} else {
				require.Equal(t, "checked=true", fields, "an inspected request without session headers must be distinguishable from missing diagnostics")
			}
			capture.SetClientRequestBody([]byte(`{"messages":[]}`))
			require.Equal(t, fields, capture.Snapshot().UpstreamSessionHeaderDiagnostic)
		})
	}
}
