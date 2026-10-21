package reasoning

import "testing"

func TestParseDeepSeekV4ThinkingSuffix(t *testing.T) {
	for _, tc := range []struct {
		model        string
		base         string
		thinkingType string
		effort       string
		ok           bool
	}{
		{"deepseek-flash-none", "deepseek-flash", "disabled", "", true},
		{"deepseek-flash-max", "deepseek-flash", "enabled", "max", true},
		{"deepseek-v4-flash-max", "deepseek-v4-flash", "enabled", "max", true},
		{"deepseek-v4.1-flash-max", "deepseek-v4.1-flash", "enabled", "max", true},
		{"DeepSeek-V4.1-Flash-none", "DeepSeek-V4.1-Flash", "disabled", "", true},
		{"deepseek-v4.1-flash-high", "deepseek-v4.1-flash-high", "", "", false},
		{"deepseek-chat-max", "deepseek-chat-max", "", "", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			base, thinkingType, effort, ok := ParseDeepSeekV4ThinkingSuffix(tc.model)
			if base != tc.base || thinkingType != tc.thinkingType || effort != tc.effort || ok != tc.ok {
				t.Fatalf("ParseDeepSeekV4ThinkingSuffix(%q) = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					tc.model, base, thinkingType, effort, ok, tc.base, tc.thinkingType, tc.effort, tc.ok)
			}
		})
	}
}
