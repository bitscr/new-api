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
