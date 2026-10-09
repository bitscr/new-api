package common

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	basecommon "github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Keep the captured content literal, including its surrounding newlines.
const creditErrorBanner = "\n" + `[Upstream Error: 402 "You have no remaining credits. Purchase pre-paid credits to continue using Inference Providers. Alternatively, subscribe to PRO to get monthly included credits."]` + "\n"

func creditErrorPayload(t testing.TB, choices ...map[string]any) []byte {
	t.Helper()
	payload, err := basecommon.Marshal(map[string]any{
		"model":   "moonshotai/Kimi-K2.7-Code",
		"choices": choices,
	})
	require.NoError(t, err)
	return payload
}

func observeCreditErrorFragments(t testing.TB, info *RelayInfo, index any, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		choice := map[string]any{"delta": map[string]any{"content": fragment}}
		if index != nil {
			choice["index"] = index
		}
		payload := creditErrorPayload(t, choice)
		info.ObserveClientAnswer(append(append([]byte("data: "), payload...), '\n', '\n'))
	}
}

func requireCreditErrorVerdict(t testing.TB, info *RelayInfo, want bool) {
	t.Helper()
	unusable, reason := info.ClientAnswerUnusable()
	require.Equal(t, want, unusable, "reason=%s", reason)
	if want {
		require.NotEmpty(t, reason)
	} else {
		require.Empty(t, reason)
	}
}

func TestCreditErrorBannerDelivered(t *testing.T) {
	info := newAnswerTestInfo()
	observeCreditErrorFragments(t, info, 0, creditErrorBanner)
	info.ObserveClientAnswer([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))

	unusable, reason := info.ClientAnswerUnusable()
	require.True(t, unusable, "HTTP 200 delivering only the captured credit-error banner must be unusable: %s", reason)
	require.Contains(t, reason, "Upstream Error")
}

func TestCreditErrorBannerSplitDelta(t *testing.T) {
	info := newAnswerTestInfo()
	observeCreditErrorFragments(t, info, 0, creditErrorBanner[:4], creditErrorBanner[4:])
	info.ObserveClientAnswer([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))

	unusable, reason := info.ClientAnswerUnusable()
	require.True(t, unusable, "splitting the credit-error label across deltas must not make it a usable answer: %s", reason)
	require.Contains(t, reason, "Upstream Error")
}

func TestCreditErrorBannerEverySplit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index any
	}{
		{"indexed", 0},
		{"missing_single_index", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for cut := 0; cut <= len(creditErrorBanner); cut++ {
				info := newAnswerTestInfo()
				observeCreditErrorFragments(t, info, tc.index, creditErrorBanner[:cut], creditErrorBanner[cut:])
				unusable, reason := info.ClientAnswerUnusable()
				require.True(t, unusable, "cut=%d reason=%s", cut, reason)
			}
			info := newAnswerTestInfo()
			for i := range len(creditErrorBanner) {
				observeCreditErrorFragments(t, info, tc.index, creditErrorBanner[i:i+1])
			}
			requireCreditErrorVerdict(t, info, true)
			t.Logf("verified all %d cuts and %d single-byte deltas", len(creditErrorBanner)+1, len(creditErrorBanner))
		})
	}
}

func TestCreditErrorBannerNonStreamAndParts(t *testing.T) {
	for _, content := range []any{
		creditErrorBanner,
		[]map[string]any{
			{"type": "text", "text": creditErrorBanner[:4]},
			{"type": "text", "text": creditErrorBanner[4:]},
		},
	} {
		info := newAnswerTestInfo()
		info.ObserveClientAnswer(creditErrorPayload(t, map[string]any{
			"index": 0, "message": map[string]any{"content": content}, "finish_reason": "stop",
		}))
		requireCreditErrorVerdict(t, info, true)
	}
}

func TestCreditErrorBannerFiniteGrammar(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		unusable   bool
	}{
		{"literal", creditErrorBanner, true},
		{"case_and_spacing", "\u2003[  uPsTrEaM\t ErRoR : 402 unavailable ]\n", true},
		{"fullwidth_brackets_and_colon", "【Upstream Error：402 unavailable】", true},
		{"unicode_case_fold", "[Upſtream Error: 402 unavailable]", true},
		{"non_grammar_space", "[\u00a0Upstream Error: 402 unavailable]", false},
		{"non_credit_error", "[Upstream Error: 503 unavailable]", true},
		{"multiple_banners", creditErrorBanner + "[Gateway Warning: unavailable]\n" + creditErrorBanner, true},
		{"unknown_bracket_label", "[Error: You have no remaining credits.]", false},
		{"unbracketed", "Upstream Error: 402 You have no remaining credits.", false},
		{"missing_colon", "[Upstream Error 402 no remaining credits]", false},
		{"different_label", "[Upstream Errors: 402 no remaining credits]", false},
		{"extended_label", "[Upstream Error Code: 402 no remaining credits]", false},
		{"unclosed_upstream_label", "[Upstream Error: 402 unavailable", true},
		{"unclosed_multiline", "[Upstream Error: 402 unavailable\ncontinued", false},
		{"partial_label", "[Upstream Err", false},
		{"ordinary_credit_prose", "You have no remaining credits. Here is how to configure the account.", false},
		{"prose_before", "The upstream reported: " + creditErrorBanner, false},
		{"prose_after", creditErrorBanner + "This is a quoted example, not a failure.", false},
		{"quote_only", `"` + strings.TrimSpace(creditErrorBanner) + `"`, false},
		{"inline_code", "`" + strings.TrimSpace(creditErrorBanner) + "`", false},
		{"fenced_code", "```text\n" + creditErrorBanner + "\n```", false},
		{"markdown_quote", "> " + strings.TrimSpace(creditErrorBanner), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := newAnswerTestInfo()
			observeCreditErrorFragments(t, info, 0, tc.text)
			requireCreditErrorVerdict(t, info, tc.unusable)
			info = newAnswerTestInfo()
			for _, r := range tc.text {
				observeCreditErrorFragments(t, info, 0, string(r))
			}
			requireCreditErrorVerdict(t, info, tc.unusable)
		})
	}
}

func TestCreditErrorBannerUnterminatedDelivery(t *testing.T) {
	for index, text := range []string{
		strings.TrimSuffix(strings.TrimSpace(creditErrorBanner), "]"),
		"【Upstream Error：402 unavailable",
	} {
		for _, finish := range []string{"stop", "length", "content_filter"} {
			t.Run(fmt.Sprintf("%d/%s", index, finish), func(t *testing.T) {
				for _, fragmented := range []bool{false, true} {
					info := newAnswerTestInfo()
					if fragmented {
						for _, r := range text {
							observeCreditErrorFragments(t, info, 0, string(r))
						}
					} else {
						observeCreditErrorFragments(t, info, 0, text)
					}
					info.ObserveClientAnswer(creditErrorPayload(t, map[string]any{
						"index": 0, "delta": map[string]any{}, "finish_reason": finish,
					}))
					requireCreditErrorVerdict(t, info, finish == "stop")
				}
			})
		}
	}
}

func TestCreditErrorBannerUnterminatedLineBreaks(t *testing.T) {
	for _, lineBreak := range []struct{ name, text string }{
		{"LF", "\n"}, {"CR", "\r"}, {"CRLF", "\r\n"},
	} {
		for _, position := range []struct{ name, before, after string }{
			{"after_opening_bracket", "[", "Upstream Error: 402 unavailable"},
			{"between_label_words", "[Upstream", "Error: 402 unavailable"},
			{"before_colon", "[Upstream Error", ": 402 unavailable"},
			{"after_colon", "[Upstream Error:", "402 unavailable"},
			{"in_body", "[Upstream Error: 402 unavailable", "continued"},
		} {
			for _, fullwidth := range []bool{false, true} {
				for _, closed := range []bool{false, true} {
					text := position.before + lineBreak.text + position.after
					if closed {
						text += "]"
					}
					if fullwidth {
						text = strings.NewReplacer("[", "【", "]", "】", ":", "：").Replace(text)
					}
					for _, delivery := range []string{"nonstream", "whole_delta", "fragmented"} {
						t.Run(fmt.Sprintf("%s/%s/fullwidth=%t/closed=%t/%s", lineBreak.name, position.name, fullwidth, closed, delivery), func(t *testing.T) {
							info := newAnswerTestInfo()
							switch delivery {
							case "nonstream":
								info.ObserveClientAnswer(creditErrorPayload(t, map[string]any{
									"index": 0, "message": map[string]any{"content": text}, "finish_reason": "stop",
								}))
							case "whole_delta":
								observeCreditErrorFragments(t, info, 0, text)
							case "fragmented":
								// Deliver CR and LF separately, including inside the label.
								for _, r := range text {
									observeCreditErrorFragments(t, info, 0, string(r))
								}
							}
							unusable, reason := info.ClientAnswerUnusable()
							require.Equal(t, closed, unusable, "internal line breaks require a closed banner: text=%q reason=%s", text, reason)
							if !closed {
								require.Empty(t, reason)
								info.ObserveUpstreamError([]byte(`{"error":{"message":"explicit protocol failure"}}`))
								requireCreditErrorVerdict(t, info, true)
							}
							// The closed cases keep the prefix gate a multiline superset.
						})
					}
				}
			}
		}
	}
}

func TestCreditErrorBannerUnterminatedWhitespaceControls(t *testing.T) {
	for _, text := range []string{
		"\n\r\n\t\u2003[Upstream Error: 402 unavailable",
		"[ \tuPsTrEaM\t\f ErRoR \t\f: 402 unavailable",
		"\r\n【\tUpstream\tError\t：402 unavailable",
	} {
		for _, fragmented := range []bool{false, true} {
			info := newAnswerTestInfo()
			if fragmented {
				for _, r := range text {
					observeCreditErrorFragments(t, info, 0, string(r))
				}
			} else {
				observeCreditErrorFragments(t, info, 0, text)
			}
			unusable, reason := info.ClientAnswerUnusable()
			require.True(t, unusable, "single-line banner with leading external whitespace must remain recognized: text=%q fragmented=%t reason=%s", text, fragmented, reason)
		}
	}
}

func TestCreditErrorBannerMultilineLabelCanCloseLater(t *testing.T) {
	info := newAnswerTestInfo()
	observeCreditErrorFragments(t, info, 0, "[Upstream\nError: 402 unavailable")
	requireCreditErrorVerdict(t, info, false)
	observeCreditErrorFragments(t, info, 0, "]")
	requireCreditErrorVerdict(t, info, true)
}

func TestCreditErrorBannerLegacyWarnings(t *testing.T) {
	for _, tc := range []struct {
		text     string
		unusable bool
	}{
		{"\n\n[Gateway Warning: 当前渠道负", true},
		{"【网关告警：当前渠道负", true},
		{"[Warning: unavailable]", true},
		{"[Notice: unavailable]", true},
		{"[Gateway Alert: unavailable]", true},
		{"Warning: unavailable", true},
		{"Gateway Warn: unavailable", true},
		{"[Warning: first\nsecond]", true},
		{"[Warning: first\nsecond", false},
		{"[Gateway Warning: unavailable]\nHere is the answer.", false},
	} {
		info := newAnswerTestInfo()
		for _, r := range tc.text {
			observeCreditErrorFragments(t, info, 0, string(r))
		}
		unusable, reason := info.ClientAnswerUnusable()
		require.Equal(t, tc.unusable, unusable, "text=%q reason=%s", tc.text, reason)
	}
}

func TestCreditErrorBannerGenuineOutput(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"prose":             {"content": "This is a real answer."},
		"reasoning_content": {"reasoning_content": "I will examine the supplied error."},
		"reasoning":         {"reasoning": "I will examine the supplied error."},
		"tools":             {"tool_calls": []any{map[string]any{"function": map[string]any{"name": "lookup", "arguments": "{}"}}}},
		"function":          {"function_call": map[string]any{"name": "lookup", "arguments": "{}"}},
	} {
		t.Run(name, func(t *testing.T) {
			for _, before := range []bool{true, false} {
				info := newAnswerTestInfo()
				payload := creditErrorPayload(t, map[string]any{"index": 0, "delta": body})
				if before {
					info.ObserveClientAnswer(payload)
				}
				observeCreditErrorFragments(t, info, 0, creditErrorBanner[:4], creditErrorBanner[4:])
				if !before {
					info.ObserveClientAnswer(payload)
				}
				requireCreditErrorVerdict(t, info, false)
				info.ObserveUpstreamError([]byte(`{"error":{"message":"explicit protocol failure"}}`))
				requireCreditErrorVerdict(t, info, true)
			}
		})
	}
	for _, finish := range []string{"length", "content_filter"} {
		info := newAnswerTestInfo()
		observeCreditErrorFragments(t, info, 0, creditErrorBanner)
		info.ObserveClientAnswer(creditErrorPayload(t, map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}))
		requireCreditErrorVerdict(t, info, false)
		info.ObserveClientAnswer([]byte(`{"error":{"message":"explicit protocol failure"}}`))
		requireCreditErrorVerdict(t, info, true)
	}
}

func TestCreditErrorBannerReadDoesNotFreezePartialContent(t *testing.T) {
	info := newAnswerTestInfo()
	observeCreditErrorFragments(t, info, 0, creditErrorBanner[:4])
	requireCreditErrorVerdict(t, info, false)
	observeCreditErrorFragments(t, info, 0, creditErrorBanner[4:])
	requireCreditErrorVerdict(t, info, true)
	observeCreditErrorFragments(t, info, 0, "The banner above is just an example.")
	requireCreditErrorVerdict(t, info, false)
}

func TestCreditErrorBannerChoiceIsolation(t *testing.T) {
	type fragment struct {
		index any
		text  string
	}
	for _, tc := range []struct {
		name      string
		fragments []fragment
		unusable  bool
	}{
		{"different_indices_cannot_form_label", []fragment{{0, creditErrorBanner[:4]}, {1, creditErrorBanner[4:]}}, false},
		{"different_indices_cannot_form_body", []fragment{{0, creditErrorBanner[:30]}, {1, creditErrorBanner[30:]}}, false},
		{"sparse_interleaved_indices", []fragment{{7, creditErrorBanner[:4]}, {1000000, creditErrorBanner[:9]}, {1000000, creditErrorBanner[9:]}, {7, creditErrorBanner[4:]}}, true},
		{"all_choices_banners", []fragment{{0, creditErrorBanner}, {1, creditErrorBanner}}, true},
		{"other_choice_has_prose", []fragment{{0, creditErrorBanner}, {1, "Here is an answer."}}, false},
		{"missing_then_explicit_zero", []fragment{{nil, creditErrorBanner[:4]}, {0, creditErrorBanner[4:]}}, true},
		{"explicit_zero_then_missing", []fragment{{0, creditErrorBanner[:4]}, {nil, creditErrorBanner[4:]}}, true},
		{"missing_and_nonzero_are_distinct", []fragment{{nil, creditErrorBanner[:4]}, {1, creditErrorBanner[4:]}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := newAnswerTestInfo()
			for _, piece := range tc.fragments {
				observeCreditErrorFragments(t, info, piece.index, piece.text)
			}
			requireCreditErrorVerdict(t, info, tc.unusable)
		})
	}
	t.Run("different_choices_in_same_frame", func(t *testing.T) {
		info := newAnswerTestInfo()
		info.ObserveClientAnswer(creditErrorPayload(t,
			map[string]any{"index": 0, "delta": map[string]any{"content": creditErrorBanner[:4]}},
			map[string]any{"index": 1, "delta": map[string]any{"content": creditErrorBanner[4:]}},
		))
		requireCreditErrorVerdict(t, info, false)
	})
	t.Run("multiple_missing_indices_are_ambiguous", func(t *testing.T) {
		info := newAnswerTestInfo()
		info.ObserveClientAnswer(creditErrorPayload(t,
			map[string]any{"delta": map[string]any{"content": creditErrorBanner}},
			map[string]any{"delta": map[string]any{"content": creditErrorBanner}},
		))
		requireCreditErrorVerdict(t, info, false)
	})
}

func TestCreditErrorBannerUnknownChoiceIndex(t *testing.T) {
	for _, index := range []any{-1, 1.5, "0", true, map[string]any{}, uint64(1 << 63)} {
		info := newAnswerTestInfo()
		observeCreditErrorFragments(t, info, 0, creditErrorBanner)
		observeCreditErrorFragments(t, info, index, creditErrorBanner)
		requireCreditErrorVerdict(t, info, false)
		info.ObserveUpstreamError([]byte(`{"error":{"message":"explicit protocol failure"}}`))
		requireCreditErrorVerdict(t, info, true)
	}
	info := newAnswerTestInfo()
	info.ObserveClientAnswer(creditErrorPayload(t, map[string]any{"index": nil, "message": map[string]any{"content": creditErrorBanner}}))
	requireCreditErrorVerdict(t, info, true)
}

func TestCreditErrorBannerRawChoicesAreNotDelivered(t *testing.T) {
	for _, content := range []string{creditErrorBanner, "This text was never sent to the client."} {
		info := newAnswerTestInfo()
		info.ObserveUpstreamError(creditErrorPayload(t, map[string]any{"index": 0, "delta": map[string]any{"content": content}}))
		requireCreditErrorVerdict(t, info, false)
		require.Empty(t, info.answer.choices)
		observeCreditErrorFragments(t, info, 0, creditErrorBanner[:4], creditErrorBanner[4:])
		requireCreditErrorVerdict(t, info, true)
	}
	info := newAnswerTestInfo()
	info.ObserveUpstreamError(creditErrorPayload(t, map[string]any{"index": 0, "delta": map[string]any{"content": creditErrorBanner[:4]}}))
	observeCreditErrorFragments(t, info, 0, creditErrorBanner[4:])
	requireCreditErrorVerdict(t, info, false)
}

func creditErrorSizedBanner(size int) string {
	const prefix = "[Upstream Error: "
	return prefix + strings.Repeat("x", size-len(prefix)-1) + "]"
}

func TestCreditErrorBannerBoundedContent(t *testing.T) {
	info := newAnswerTestInfo()
	text := creditErrorSizedBanner(answerContentLimit)
	for i := range len(text) {
		observeCreditErrorFragments(t, info, 0, text[i:i+1])
	}
	requireCreditErrorVerdict(t, info, true)
	require.Len(t, info.answer.choices, 1)
	require.Len(t, info.answer.choices[0].content, answerContentLimit)
	require.LessOrEqual(t, cap(info.answer.choices[0].content), answerContentLimit)
	t.Logf("at limit: retained=%d capacity=%d", len(info.answer.choices[0].content), cap(info.answer.choices[0].content))

	observeCreditErrorFragments(t, info, 0, " ")
	requireCreditErrorVerdict(t, info, false)
	require.Empty(t, info.answer.choices, "overflow must discard evidence, not classify a retained/truncated banner")
	observeCreditErrorFragments(t, info, 0, creditErrorBanner)
	requireCreditErrorVerdict(t, info, false)
	require.Empty(t, info.answer.choices, "do not resume retention after overflow")
	info.ObserveUpstreamError([]byte(`{"error":{"message":"explicit protocol failure"}}`))
	requireCreditErrorVerdict(t, info, true)
	t.Log("after overflow: retained=0; undecidable until explicit protocol error")

	for _, oversized := range []string{
		creditErrorSizedBanner(answerContentLimit + 1),
		"[Warning: " + strings.Repeat("x", answerContentLimit),
		"[" + strings.Repeat(" ", answerContentLimit) + "Upstream Error: 402]",
	} {
		info := newAnswerTestInfo()
		observeCreditErrorFragments(t, info, 0, oversized)
		requireCreditErrorVerdict(t, info, false)
		require.Empty(t, info.answer.choices)
	}
}

func TestCreditErrorBannerBoundedChoices(t *testing.T) {
	info := newAnswerTestInfo()
	for index := range answerChoiceLimit {
		observeCreditErrorFragments(t, info, index*1000000, creditErrorSizedBanner(answerContentLimit))
	}
	requireCreditErrorVerdict(t, info, true)
	require.Len(t, info.answer.choices, answerChoiceLimit)
	require.LessOrEqual(t, cap(info.answer.choices), answerChoiceLimit)
	retained, capacity, prefixes := 0, 0, 0
	for _, choice := range info.answer.choices {
		retained += len(choice.content)
		capacity += cap(choice.content)
		prefixes += len(choice.prefix)
		require.LessOrEqual(t, cap(choice.content), answerContentLimit)
		require.LessOrEqual(t, len(choice.prefix), 64, "a partial label is finite, not another answer buffer")
	}
	require.Equal(t, answerChoiceLimit*answerContentLimit, retained)
	require.LessOrEqual(t, capacity, answerChoiceLimit*answerContentLimit)
	t.Logf("choices=%d choice_capacity=%d retained=%d capacity=%d prefix_bytes=%d", len(info.answer.choices), cap(info.answer.choices), retained, capacity, prefixes)

	observeCreditErrorFragments(t, info, -1, creditErrorBanner)
	requireCreditErrorVerdict(t, info, false)
	require.Empty(t, info.answer.choices)
	info.BeginAttempt()
	for index := 0; index <= answerChoiceLimit; index++ {
		observeCreditErrorFragments(t, info, index, creditErrorBanner)
	}
	requireCreditErrorVerdict(t, info, false)
	require.Empty(t, info.answer.choices, "the extra choice must make the bounded evidence inconclusive")
	info.ObserveClientAnswer([]byte(`{"error":{"code":402}}`))
	requireCreditErrorVerdict(t, info, true)
	t.Log("choice overflow: retained=0; explicit protocol error still dominates")
}

func TestCreditErrorBannerOrdinaryProseIsNotRetained(t *testing.T) {
	for _, prose := range []string{"Here is the answer.", "Weather is good.", "[Not an error]", "```text\n" + creditErrorBanner + "```"} {
		info := newAnswerTestInfo()
		observeCreditErrorFragments(t, info, 0, prose)
		for range 100 {
			observeCreditErrorFragments(t, info, 0, strings.Repeat("x", answerContentLimit))
		}
		requireCreditErrorVerdict(t, info, false)
		require.True(t, info.answer.textSeen)
		require.Empty(t, info.answer.choices, "ordinary completions must not be retained")
	}
}

func TestCreditErrorBannerBeginAttemptResetsState(t *testing.T) {
	info := newAnswerTestInfo()
	observeCreditErrorFragments(t, info, 0, creditErrorBanner[:4])
	info.MarkClientDeliveryBroken()
	info.EndAttempt()
	info.BeginAttempt()
	requireCreditErrorVerdict(t, info, false)
	require.Empty(t, info.answer.choices)
	require.False(t, info.ClientDeliveryBroken())
	observeCreditErrorFragments(t, info, 0, creditErrorBanner[4:])
	requireCreditErrorVerdict(t, info, false)

	info.BeginAttempt()
	observeCreditErrorFragments(t, info, 0, creditErrorSizedBanner(answerContentLimit+1))
	requireCreditErrorVerdict(t, info, false)
	info.BeginAttempt()
	observeCreditErrorFragments(t, info, 0, creditErrorBanner)
	requireCreditErrorVerdict(t, info, true)
	info.BeginAttempt()
	info.ObserveClientAnswer([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
	unusable, reason := info.ClientAnswerUnusable()
	require.True(t, unusable)
	require.NotContains(t, reason, "Upstream Error", "the previous attempt's banner sample must not survive")
}

func TestCreditErrorBannerPrefixStorageIsBounded(t *testing.T) {
	info := newAnswerTestInfo()
	observeCreditErrorFragments(t, info, 0, "[")
	for range answerContentLimit - 64 {
		observeCreditErrorFragments(t, info, 0, " ")
	}
	requireCreditErrorVerdict(t, info, false)
	require.Len(t, info.answer.choices, 1)
	require.Equal(t, "[", info.answer.choices[0].prefix)
	require.LessOrEqual(t, cap(info.answer.choices[0].content), answerContentLimit)
	t.Logf("pending label: retained=%d normalized_prefix_bytes=%d", len(info.answer.choices[0].content), len(info.answer.choices[0].prefix))
	observeCreditErrorFragments(t, info, 0, "Upstream Error: 402]")
	requireCreditErrorVerdict(t, info, true)
}

func TestCreditErrorBannerConcurrentObservation(t *testing.T) {
	info := newAnswerTestInfo()
	var writers sync.WaitGroup
	for index := range 8 {
		var frames [][]byte
		for i := range len(creditErrorBanner) {
			frames = append(frames, creditErrorPayload(t, map[string]any{"index": index, "delta": map[string]any{"content": creditErrorBanner[i : i+1]}}))
		}
		writers.Add(1)
		go func() {
			defer writers.Done()
			for _, frame := range frames {
				info.ObserveClientAnswer(frame)
			}
		}()
	}
	writers.Add(1)
	go func() {
		defer writers.Done()
		for range 128 {
			info.ClientAnswerUnusable()
			info.MarkClientDeliveryBroken()
			info.ClientDeliveryBroken()
		}
	}()
	writers.Wait()
	requireCreditErrorVerdict(t, info, true)
	require.True(t, info.ClientDeliveryBroken())
	require.Len(t, info.answer.choices, 8)
}

func TestCreditErrorBannerWriterPreservesBytes(t *testing.T) {
	previousMode := gin.Mode()
	gin.SetMode(gin.TestMode)
	t.Cleanup(func() { gin.SetMode(previousMode) })
	for _, writeString := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil)
		info := newAnswerTestInfo()
		SetRelayInfo(c, info)
		InstallRelayResponseWriter(c)
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		var delivered strings.Builder
		for _, fragment := range []string{creditErrorBanner[:4], creditErrorBanner[4:]} {
			payload := creditErrorPayload(t, map[string]any{"index": 0, "delta": map[string]any{"content": fragment}})
			frame := "data: " + string(payload) + "\n\n"
			delivered.WriteString(frame)
			var n int
			var err error
			if writeString {
				n, err = c.Writer.WriteString(frame)
			} else {
				n, err = c.Writer.Write([]byte(frame))
			}
			require.NoError(t, err)
			require.Equal(t, len(frame), n)
		}
		require.Equal(t, 200, recorder.Code)
		require.Equal(t, delivered.String(), recorder.Body.String())
		requireCreditErrorVerdict(t, info, true)
		require.False(t, info.ClientDeliveryBroken())
	}
}

func BenchmarkCreditErrorObservationBytewise(b *testing.B) {
	for _, size := range []int{256, 1024, 4096} {
		b.Run(fmt.Sprintf("%d_bytes", size), func(b *testing.B) {
			text := creditErrorSizedBanner(size)
			var frames [][]byte
			for i := range len(text) {
				payload := creditErrorPayload(b, map[string]any{"index": 0, "delta": map[string]any{"content": text[i : i+1]}})
				frames = append(frames, append([]byte("data: "), payload...))
			}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				info := newAnswerTestInfo()
				for _, frame := range frames {
					info.ObserveClientAnswer(frame)
				}
				if unusable, reason := info.ClientAnswerUnusable(); !unusable {
					b.Fatalf("bytewise error-only banner became usable: %s", reason)
				}
			}
		})
	}
}
