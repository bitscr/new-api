package service

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Called after adaptor/passthrough/header overrides have produced the actual
// upstream headers. A differing hash identifies a mapping or override without
// printing the identifier or treating a shared cache hint as proof of leakage.
func LogConversationUpstreamHeaderDiagnostic(ctx *gin.Context, info *relaycommon.RelayInfo, headers http.Header) {
	if ctx == nil || ctx.Request == nil || info == nil || info.ConversationCapture == nil {
		return
	}
	fields := "checked=true"
	if headerFields := conversationHeaderDiagnosticFields(ctx.Request.Header, headers); headerFields != "" {
		fields += " " + headerFields
	}
	info.ConversationCapture.SetUpstreamSessionHeaderDiagnostic(fields)
	logger.LogInfo(ctx, fmt.Sprintf("conversation diag: stage=upstream_headers token_id=%d channel_id=%d %s", info.TokenId, info.ChannelId, fields))
}

func conversationHeaderDiagnosticFields(client, upstream http.Header) string {
	var fields []string
	for _, header := range []string{"Session_id", "X-Grok-Conv-Id", "X-Session-Id", "X-Conversation-Id"} {
		clientValue, upstreamValue := client.Get(header), upstream.Get(header)
		if clientValue == "" && upstreamValue == "" {
			continue
		}
		name := strings.ToLower(strings.ReplaceAll(header, "-", "_"))
		fields = append(fields, fmt.Sprintf("header_%s_client_sha256=%s header_%s_upstream_sha256=%s header_%s_changed=%t",
			name, conversationBodyFingerprint([]byte(clientValue)), name, conversationBodyFingerprint([]byte(upstreamValue)), name, clientValue != upstreamValue))
	}
	return strings.Join(fields, " ")
}

// Reuse the opt-in four-stage capture: diagnostics must neither enable prompt
// storage implicitly nor allocate a second stream-sized response buffer.
func logConversationDiagnostic(ctx *gin.Context, info *relaycommon.RelayInfo, snapshot relaycommon.ConversationCaptureSnapshot) {
	upstreamText, upstreamTools := deriveConversationLogPayload(snapshot.UpstreamResponseBody, info.GetFinalRequestRelayFormat(), info.IsStream)
	clientText, clientTools := deriveConversationLogPayload(snapshot.ClientResponseBody, info.RelayFormat, info.IsStream)
	logger.LogInfo(ctx, fmt.Sprintf("conversation diag: stage=complete token_id=%d channel_id=%d stream=%t system_prompt_overwritten=%t %s upstream_assistant_sha256=%s client_assistant_sha256=%s upstream_tools_sha256=%s client_tools_sha256=%s",
		info.TokenId, info.ChannelId, info.IsStream,
		common.GetContextKeyBool(ctx, constant.ContextKeySystemPromptOverride),
		conversationDiagnosticFields(snapshot), conversationBodyFingerprint([]byte(upstreamText)), conversationBodyFingerprint([]byte(clientText)),
		conversationBodyFingerprint([]byte(upstreamTools)), conversationBodyFingerprint([]byte(clientTools))))
}

func conversationDiagnosticFields(snapshot relaycommon.ConversationCaptureSnapshot) string {
	var fields []string
	for _, stage := range []struct {
		name string
		body []byte
	}{
		{"client_request", snapshot.ClientRequestBody},
		{"upstream_request", snapshot.UpstreamRequestBody},
		{"upstream_response", snapshot.UpstreamResponseBody},
		{"client_response", snapshot.ClientResponseBody},
	} {
		fields = append(fields, fmt.Sprintf("%s_bytes=%d %s_sha256=%s", stage.name, len(stage.body), stage.name, conversationBodyFingerprint(stage.body)))
	}
	// The field name records which side supplied the identifier. Values are
	// fingerprints only; the admin capture retains the evidence for inspection.
	for _, stage := range []struct {
		name string
		body []byte
	}{{"client", snapshot.ClientRequestBody}, {"upstream", snapshot.UpstreamRequestBody}} {
		for _, path := range []string{"prompt_cache_key", "previous_response_id", "conversation", "session_id", "user", "metadata.session_id", "metadata.conversation_id", "metadata.user_id"} {
			value := gjson.GetBytes(stage.body, path)
			if value.Exists() && value.Type != gjson.Null {
				fields = append(fields, fmt.Sprintf("%s_%s_sha256=%s", stage.name, strings.ReplaceAll(path, ".", "_"), conversationBodyFingerprint([]byte(value.String()))))
			}
		}
	}
	return strings.Join(fields, " ")
}

func conversationBodyFingerprint(body []byte) string {
	if len(body) == 0 {
		return "?"
	}
	return fmt.Sprintf("%x", sha256.Sum256(body))
}
