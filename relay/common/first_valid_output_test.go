package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newFirstValidOutputInfo() *RelayInfo {
	info := &RelayInfo{IsStream: true, answer: &answerObservation{}}
	info.BeginAttempt()
	return info
}

func TestObserveClientAnswerMarksFirstValidOutputOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
		want  bool
	}{
		{"role", `data: {"choices":[{"delta":{"role":"assistant"}}]}`, false},
		{"heartbeat", `data: {"type":"ping"}`, false},
		{"finish", `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`, false},
		{"empty reasoning shape", `data: {"choices":[{"delta":{"reasoning":{"type":"summary","summary":[]}}}]}`, false},
		{"reasoning text object", `data: {"choices":[{"delta":{"reasoning":{"text":"hmm"}}}]}`, true},
		{"banner", `data: {"choices":[{"delta":{"content":"[Gateway Warning: upstream]"}}]}`, false},
		{"openai content", `data: {"choices":[{"delta":{"content":"hello"}}]}`, true},
		{"claude text delta", `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, true},
		{"claude thinking delta", `data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}`, true},
		{"claude message metadata", `data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"usage":{"input_tokens":3}}}`, false},
		{"claude tool definition", `data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"t","name":"","input":{}}}`, false},
		{"claude tool call", `data: {"type":"content_block_start","content_block":{"type":"tool_use","id":"t","name":"lookup","input":{}}}`, true},
		{"gemini text", `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, true},
		{"gemini function call", `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{}}}]}}]}`, true},
		{"gemini prompt feedback", `data: {"promptFeedback":{"blockReasonMessage":"not output"}}`, false},
		{"responses created metadata", `data: {"type":"response.created","response":{"id":"r","object":"response","output":[],"metadata":{"text":"not output"}}}`, false},
		{"responses created input echo", `data: {"type":"response.created","response":{"id":"r","output":[],"input":[{"role":"user","content":[{"type":"input_text","text":"echo"}]}]}}`, false},
		{"responses text delta", `data: {"type":"response.output_text.delta","delta":"hi"}`, true},
		{"responses reasoning delta", `data: {"type":"response.reasoning_summary_text.delta","delta":"hmm"}`, true},
		{"responses function delta", `data: {"type":"response.function_call_arguments.delta","delta":"{\"x\":"}`, true},
		{"responses completed empty", `data: {"type":"response.completed","response":{"id":"r","output":[]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := newFirstValidOutputInfo()
			info.ObserveClientAnswer([]byte(tc.frame))
			got := !info.AttemptFirstValidOutputTime.IsZero()
			require.Equal(t, tc.want, got)
		})
	}
}

func TestNativeSplitBannerDoesNotMarkFirstValidOutput(t *testing.T) {
	cases := []struct {
		name    string
		banners []string
		answer  string
	}{
		{
			name: "claude",
			banners: []string{
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"[Gateway Warn"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ing: busy]"}}`,
			},
			answer: `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The answer."}}`,
		},
		{
			name: "gemini",
			banners: []string{
				`data: {"candidates":[{"content":{"parts":[{"text":"[Gateway Warn"}]}}]}`,
				`data: {"candidates":[{"content":{"parts":[{"text":"ing: busy]"}]}}]}`,
			},
			answer: `data: {"candidates":[{"content":{"parts":[{"text":"The answer."}]}}]}`,
		},
		{
			name: "responses",
			banners: []string{
				`data: {"type":"response.output_text.delta","item_id":"msg_1","content_index":0,"delta":"[Gateway Warn"}`,
				`data: {"type":"response.output_text.delta","item_id":"msg_1","content_index":0,"delta":"ing: busy]"}`,
			},
			answer: `data: {"type":"response.output_text.delta","item_id":"msg_1","content_index":0,"delta":"The answer."}`,
		},
		{
			name: "claude space split",
			banners: []string{
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"[Gateway"}}`,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" Warning: busy]"}}`,
			},
			answer: `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The answer."}}`,
		},
		{
			name: "gemini whitespace packet",
			banners: []string{
				`data: {"candidates":[{"content":{"parts":[{"text":"[Gateway"}]}}]}`,
				`data: {"candidates":[{"content":{"parts":[{"text":" "}]}}]}`,
				`data: {"candidates":[{"content":{"parts":[{"text":"Warning: busy]"}]}}]}`,
			},
			answer: `data: {"candidates":[{"content":{"parts":[{"text":"The answer."}]}}]}`,
		},
		{
			name: "responses whitespace packet",
			banners: []string{
				`data: {"type":"response.output_text.delta","item_id":"msg_2","content_index":0,"delta":"[Gateway"}`,
				`data: {"type":"response.output_text.delta","item_id":"msg_2","content_index":0,"delta":" "}`,
				`data: {"type":"response.output_text.delta","item_id":"msg_2","content_index":0,"delta":"Warning: busy]"}`,
			},
			answer: `data: {"type":"response.output_text.delta","item_id":"msg_2","content_index":0,"delta":"The answer."}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := newFirstValidOutputInfo()
			for _, banner := range tc.banners {
				info.ObserveClientAnswer([]byte(banner))
			}
			require.True(t, info.AttemptFirstValidOutputTime.IsZero(),
				"a split banner is not a valid first output")
			info.ObserveClientAnswer([]byte(tc.answer))
			require.False(t, info.AttemptFirstValidOutputTime.IsZero(),
				"text outside the banner starts the first-output clock")
		})
	}
}

func TestBannerFollowedByAnswerMarksFirstValidOutput(t *testing.T) {
	info := newFirstValidOutputInfo()
	info.ObserveClientAnswer([]byte(`data: {"choices":[{"index":0,"delta":{"content":"[Gateway Warning: busy]"}}]}`))
	require.True(t, info.AttemptFirstValidOutputTime.IsZero(), "a closed banner alone is not an answer")
	info.ObserveClientAnswer([]byte(`data: {"choices":[{"index":0,"delta":{"content":"The answer is 42."}}]}`))
	require.False(t, info.AttemptFirstValidOutputTime.IsZero(),
		"text outside a completed banner is a real first output")

	multiple := newFirstValidOutputInfo()
	for _, fragment := range []string{"[Gateway Warning: a]", "[Gateway Warning: b]", "answer"} {
		multiple.ObserveClientAnswer([]byte(`data: {"choices":[{"index":0,"delta":{"content":"` + fragment + `"}}]}`))
	}
	require.False(t, multiple.AttemptFirstValidOutputTime.IsZero(),
		"answer after consecutive banners still counts")
}

func TestAttemptFirstValidOutputLatencyResetsPerAttempt(t *testing.T) {
	info := newFirstValidOutputInfo()
	start := time.Now()
	info.AttemptStartTime = start
	info.MarkFirstValidOutput()
	first, ok := info.AttemptFirstValidOutputLatency()
	require.True(t, ok)
	require.GreaterOrEqual(t, first, time.Duration(0))
	info.EndAttempt()
	info.BeginAttempt()
	_, ok = info.AttemptFirstValidOutputLatency()
	require.False(t, ok, "a new attempt must not inherit the previous valid-output timestamp")
}
