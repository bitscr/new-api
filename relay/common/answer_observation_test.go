package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func newAnswerTestInfo() *RelayInfo {
	return &RelayInfo{answer: &answerObservation{}}
}

// TestClientAnswerUnusable 锁定"200 但没有有效回答"的判定：
// 宁可漏判也不能误判，所以只有"整段正文为空"或"整段正文只是网关告警横幅"才算。
func TestClientAnswerUnusable(t *testing.T) {
	cases := []struct {
		name     string
		payloads []string
		unusable bool
	}{
		{
			name: "正常流式回答",
			payloads: []string{
				`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"你好"}}]}`,
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
				`data: [DONE]`,
			},
		},
		{
			// 实测样本：channel #5 那条 200 的正文就是这个（客户端侧被中转截断）
			name: "正文只是网关告警横幅",
			payloads: []string{
				`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`data: {"choices":[{"index":0,"delta":{"content":"\n\n[Gateway Warning: 当前渠道负"}}]}`,
			},
			unusable: true,
		},
		{
			name: "横幅之外还有正文",
			payloads: []string{
				`data: {"choices":[{"delta":{"content":"[Gateway Warning: 当前渠道负载过高]\n\n这是答案"}}]}`,
			},
		},
		{
			name: "中文告警横幅",
			payloads: []string{
				`data: {"choices":[{"delta":{"content":"【网关告警：当前渠道负载过高，请稍后重试】"}}]}`,
			},
			unusable: true,
		},
		{
			name: "只有角色和结束、没有正文",
			payloads: []string{
				`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			},
			unusable: true,
		},
		{
			name: "只有工具调用",
			payloads: []string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"run","arguments":"{}"}}]}}]}`,
			},
		},
		{
			name: "只有思考内容",
			payloads: []string{
				`data: {"choices":[{"delta":{"reasoning_content":"让我先想想"}}]}`,
			},
		},
		{
			name: "被 max_tokens 截断：不下结论",
			payloads: []string{
				`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			},
		},
		{
			name: "非 OpenAI 格式（Claude 流）",
			payloads: []string{
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			},
		},
		{
			name:     "非流式空正文",
			payloads: []string{`{"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`},
			unusable: true,
		},
		{
			name:     "非流式正常回答",
			payloads: []string{`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
		},
		{
			name:     "非流式分片正文",
			payloads: []string{`{"choices":[{"message":{"content":[{"type":"text","text":"ok"}]},"finish_reason":"stop"}]}`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := newAnswerTestInfo()
			for _, payload := range tc.payloads {
				info.ObserveClientAnswer([]byte(payload))
			}
			unusable, reason := info.ClientAnswerUnusable()
			require.Equal(t, tc.unusable, unusable, "reason=%s", reason)
		})
	}
}

func TestClientAnswerProtocolError(t *testing.T) {
	info := newAnswerTestInfo()
	info.ObserveClientAnswer([]byte(`data: {"error":{"message":"upstream overloaded","type":"upstream_error","code":"overloaded"}}`))
	unusable, reason := info.ClientAnswerUnusable()
	require.True(t, unusable, "HTTP 200 with an explicit protocol error must not count as a usable answer")
	require.NotEmpty(t, reason)
}

func TestClientAnswerProtocolErrorRecognition(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		unusable      bool
	}{
		{"message", `{"error":{"message":"upstream failed"}}`, true},
		{"type", `{"error":{"type":"server_error"}}`, true},
		{"string code", `{"error":{"code":"quota_exceeded"}}`, true},
		{"numeric code", `{"error":{"code":429}}`, true},
		{"string error", `{"error":"upstream failed"}`, true},
		{"message with zero code", `{"error":{"message":"upstream failed","code":0}}`, true},
		{"type with success code", `{"error":{"type":"server_error","code":"ok"}}`, true},
		{"empty choices", `{"choices":[],"error":{"message":"upstream failed"}}`, true},
		{"invalid choices", `{"choices":false,"error":{"message":"upstream failed"}}`, true},
		{"same frame content", `{"choices":[{"delta":{"content":"partial"}}],"error":{"message":"upstream failed"}}`, true},
		{"absent", `{}`, false},
		{"null", `{"error":null}`, false},
		{"empty string", `{"error":""}`, false},
		{"blank string", `{"error":" \n\t "}`, false},
		{"empty object", `{"error":{}}`, false},
		{"blank fields", `{"error":{"message":" \n ","type":" ","code":" "}}`, false},
		{"null fields", `{"error":{"message":null,"type":null,"code":null}}`, false},
		{"unknown metadata", `{"error":{"request_id":"req-1","param":"model"}}`, false},
		{"false", `{"error":false}`, false},
		{"true", `{"error":true}`, false},
		{"array", `{"error":[{"message":"upstream failed"}]}`, false},
		{"number", `{"error":500}`, false},
		{"wrong field types", `{"error":{"message":true,"type":500,"code":{}}}`, false},
		{"array fields", `{"error":{"message":["failed"],"type":{},"code":[500]}}`, false},
		{"boolean code", `{"error":{"code":true}}`, false},
		{"zero code", `{"error":{"code":0}}`, false},
		{"200 code", `{"error":{"code":200}}`, false},
		{"zero string code", `{"error":{"code":"0"}}`, false},
		{"200 string code", `{"error":{"code":"200"}}`, false},
		{"success code", `{"error":{"code":" Success "}}`, false},
		{"ok code", `{"error":{"code":" OK "}}`, false},
		{"nested error", `{"metadata":{"error":{"message":"example"}}}`, false},
		{"message without error", `{"message":"upstream failed"}`, false},
		{"quoted content", `{"choices":[{"delta":{"content":"{\"error\":{\"message\":\"example\"}}"}}]}`, false},
		{"tool arguments", `{"choices":[{"delta":{"tool_calls":[{"function":{"name":"example","arguments":"{\"error\":{\"message\":\"example\"}}"}}]}}]}`, false},
		{"malformed JSON", `{"error":{"message":"upstream failed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, prefix := range []string{"", "data: "} {
				info := newAnswerTestInfo()
				info.ObserveClientAnswer([]byte(prefix + tc.payload))
				unusable, reason := info.ClientAnswerUnusable()
				require.Equal(t, tc.unusable, unusable, "prefix=%q reason=%s", prefix, reason)
				if tc.unusable {
					require.NotEmpty(t, reason)
				} else {
					require.Empty(t, reason)
				}
			}
			info := newAnswerTestInfo()
			info.ObserveUpstreamError([]byte(tc.payload))
			unusable, reason := info.ClientAnswerUnusable()
			require.Equal(t, tc.unusable, unusable, "raw error recognition must match client recognition: %s", reason)
		})
	}
}

func TestEscapedProtocolErrorOverridesContent(t *testing.T) {
	const payload = `{"\u0065rror":{"message":"upstream failed"}}`
	for _, tc := range []struct {
		name, prefix string
		observe      func(*RelayInfo, []byte)
	}{
		{"raw", "", (*RelayInfo).ObserveUpstreamError},
		{"client JSON", "", (*RelayInfo).ObserveClientAnswer},
		{"client SSE", "data: ", (*RelayInfo).ObserveClientAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := newAnswerTestInfo()
			info.ObserveClientAnswer([]byte(`data: {"choices":[{"delta":{"content":"partial"}}]}`))
			unusable, _ := info.ClientAnswerUnusable()
			require.False(t, unusable, "control: partial content is usable without an error")

			tc.observe(info, []byte(tc.prefix+payload))
			unusable, reason := info.ClientAnswerUnusable()
			require.True(t, unusable, "escaped error key must override partial content: %s", reason)
			require.NotEmpty(t, reason)
		})
	}
}

func TestObserveUpstreamErrorDoesNotCreditUnsentChoices(t *testing.T) {
	for _, payload := range []string{
		`{"choices":[{"delta":{"content":"not sent"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"not sent"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"lookup","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"content":"[Gateway Warning: not sent]"}}]}`,
	} {
		info := newAnswerTestInfo()
		info.ObserveUpstreamError([]byte(payload))
		unusable, reason := info.ClientAnswerUnusable()
		require.False(t, unusable, "raw choices alone must not be judged: %s", payload)
		require.Empty(t, reason)
		info.ObserveClientAnswer([]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
		unusable, _ = info.ClientAnswerUnusable()
		require.True(t, unusable, "unsent raw choices must not count as a delivered answer")
	}
}

func TestObserveUpstreamErrorNilAndEmpty(t *testing.T) {
	for _, info := range []*RelayInfo{nil, {}, newAnswerTestInfo()} {
		for _, payload := range [][]byte{nil, {}, []byte(" \n ")} {
			info.ObserveUpstreamError(payload)
			unusable, reason := info.ClientAnswerUnusable()
			require.False(t, unusable)
			require.Empty(t, reason)
		}
	}
	for _, info := range []*RelayInfo{nil, {}} {
		info.ObserveUpstreamError([]byte(`{"error":{"message":"upstream failed"}}`))
		unusable, reason := info.ClientAnswerUnusable()
		require.False(t, unusable, "do not create an observer for hand-built RelayInfo")
		require.Empty(t, reason)
	}
}

func TestClientAnswerProtocolErrorDominates(t *testing.T) {
	protocolError := []byte(`data: {"error":{"message":"upstream failed"}}`)
	for _, payload := range []string{
		`data: {"choices":[{"delta":{"content":"partial"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"thinking"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"lookup","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
	} {
		info := newAnswerTestInfo()
		info.ObserveClientAnswer([]byte(payload))
		unusable, _ := info.ClientAnswerUnusable()
		require.False(t, unusable, "control: %s", payload)
		info.ObserveClientAnswer(protocolError)
		unusable, reason := info.ClientAnswerUnusable()
		require.True(t, unusable, "explicit error must override %s", payload)
		info.ObserveClientAnswer([]byte(payload))
		info.ObserveClientAnswer(protocolError)
		stillUnusable, repeatedReason := info.ClientAnswerUnusable()
		require.True(t, stillUnusable)
		require.Equal(t, reason, repeatedReason)

		info.BeginAttempt()
		unusable, reason = info.ClientAnswerUnusable()
		require.False(t, unusable, "a new attempt must not inherit the error")
		require.Empty(t, reason)
		info.ObserveClientAnswer([]byte(payload))
		unusable, _ = info.ClientAnswerUnusable()
		require.False(t, unusable)
	}
}

func TestClientAnswerUnusableNilSafe(t *testing.T) {
	var nilInfo *RelayInfo
	unusable, reason := nilInfo.ClientAnswerUnusable()
	require.False(t, unusable)
	require.Empty(t, reason)

	// 非 genBaseRelayInfo 构造的 RelayInfo（没有观察器）：不观察，也不下判断
	info := &RelayInfo{}
	info.ObserveClientAnswer([]byte(`data: {"choices":[{"delta":{"content":"[Gateway Warning: x]"}}]}`))
	unusable, _ = info.ClientAnswerUnusable()
	require.False(t, unusable)
}
