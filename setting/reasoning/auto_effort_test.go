package reasoning

import "testing"

func TestMapAutoEffortKnownFamilies(t *testing.T) {
	for _, tc := range []struct {
		model  string
		effort string
		want   string
	}{
		// OpenAI 精确能力表优先于家族兜底。
		{"gpt-6-luna", "max", "max"},
		{"gpt-5.5", "max", "xhigh"},
		{"gpt-5.5", "none", "none"},
		{"gpt-5", "max", "high"},
		{"gpt-free", "max", "max"},

		// DeepSeek：固定 none/low/high/max；不在表中的值取最高档 max。
		{"[次]deepseek-v4.1-flash", "max", "max"},
		{"deepseek-v4-flash:0731", "medium", "max"},
		{"deepseek-v4-flash:0731", "minimal", "max"},
		{"deepseek-v4-flash:0731", "xhigh", "max"},
		{"DeepSeek-V4-Flash-0731", "none", "none"},

		// GLM 版本差异。
		{"glm-5.1", "max", "xhigh"},
		{"glm-5.2", "max", "max"},
		{"glm-5.3", "max", "max"},
		{"glm-5.3", "medium", "max"},
		{"glm-5.3", "xhigh", "max"},
		{"glm-5.3", "none", "none"},
		{"glm-5.3", "minimal", "max"},
		{"glm-5.2", "low", "max"},
		{"glm-5.2", "xhigh", "max"},

		// Qwen3.8 不接受 high，适配到 xhigh。
		{"intern/inkstone/qwen3.8-flash", "max", "xhigh"},
		{"channel:Qwen3.8-Flash", "high", "xhigh"},
		{"qwen3.7-max", "high", "high"},
		{"qwen3.8-flash-next", "high", "xhigh"},
		{"qwen3.8-flash-next", "medium", "medium"},
		{"qwen3.8-flash-next", "xhigh", "xhigh"},
		{"qwen3", "max", "xhigh"},

		// Gemini 不支持 max；3.1 Pro 只有 low/high。
		{"google/gemini-3.1-pro-preview", "max", "high"},
		{"google/gemini-3.1-pro-preview", "medium", "high"},
		{"gemini-3.7-flash", "max", "high"},
		{"gemini-3.6-flash", "minimal", "minimal"},

		// Claude。
		{"claude-opus-5-free", "max", "max"},
		{"claude-opus-5-free", "xhigh", "max"},
		{"claude-sonnet-5-free", "xhigh", "max"},

		// Kimi K3 支持 low/high/max；K2 及更早不接受 effort。
		{"vendor/KimiK3", "max", "max"},
		{"moonshotai/kimi-k2.7-code", "max", ""},
		{"MiniMaxAI/MiniMax-M3", "max", ""},
		{"codestral-mistral", "max", ""},
		{"cohere/north-mini-code:free", "max", ""},

		// Grok。
		{"xai/grok-4.6", "max", "xhigh"},

		// 未知模型：low/high/max 兜底，不匹配取 max。
		{"provider/unknown-model", "max", "max"},
		{"provider/unknown-model", "medium", "max"},
		{"provider/unknown-model", "none", "none"},
	} {
		t.Run(tc.model+"_"+tc.effort, func(t *testing.T) {
			if got := MapAutoEffort(tc.effort, tc.model, "auto"); got != tc.want {
				t.Fatalf("MapAutoEffort(%q, %q) = %q, want %q", tc.effort, tc.model, got, tc.want)
			}
		})
	}
}

// TestMapAutoEffortOfficialFixedLevels 锁定官方固定档位规则：
// 命中固定档位原样保留；不命中取该表最高档，不做别名映射。
func TestMapAutoEffortOfficialFixedLevels(t *testing.T) {
	for _, tc := range []struct {
		model, effort, want string
	}{
		// DeepSeek：固定 none/low/high/max；不在表中的值取最高档 max。
		{"deepseek-v4.1-flash", "none", "none"},
		{"deepseek-v4.1-flash", "low", "low"},
		{"deepseek-v4.1-flash", "high", "high"},
		{"deepseek-v4.1-flash", "max", "max"},
		{"deepseek-v4.1-flash", "minimal", "max"},
		{"deepseek-v4.1-flash", "medium", "max"},
		{"deepseek-v4.1-flash", "xhigh", "max"},
		{"[次]deepseek-v4-flash", "max", "max"},
		// none 永远表示关闭思考，不参与最高档收敛。
		{"claude-fable-5.1-free", "none", "none"},
		{"gemini-3.7-flash", "none", "none"},
		{"glm-5.3-flash-free", "none", "none"},
		{"provider/unknown-model", "none", "none"},
		// Qwen3.8：固定 low/medium/xhigh；high 不在表中，取最高档 xhigh。
		{"qwen3.8-flash-next", "high", "xhigh"},
		{"qwen3.8-flash-next", "medium", "medium"},
		{"qwen3.8-flash-next", "xhigh", "xhigh"},
		// Claude 固定 high 原样保留。
		{"claude-fable-5.1-free", "high", "high"},
		// GLM 5.3：固定 low/high/max；不在表中的值取最高档 max。
		{"glm-5.3-flash-free", "max", "max"},
		{"glm-5.3-flash-free", "medium", "max"},
		{"glm-5.3-flash-free", "xhigh", "max"},
		{"gemini-3.7-flash", "high", "high"},
		{"MiMo-V2.6-Flash", "high", "high"},
		{"hy4-preview-free", "high", "high"},
		{"gpt-6-luna", "max", "max"},
		{"gpt-5.6-luna", "low", "low"},
	} {
		t.Run(tc.model+"_"+tc.effort, func(t *testing.T) {
			if got := MapAutoEffort(tc.effort, tc.model, "auto"); got != tc.want {
				t.Fatalf("MapAutoEffort(%q, %q) = %q, want %q", tc.effort, tc.model, got, tc.want)
			}
		})
	}
}

func TestMapAutoEffortFamilyParsing(t *testing.T) {
	for _, tc := range []struct {
		model  string
		effort string
		want   string
	}{
		{"[tenant/kimi]gemini-3.1-pro", "max", "high"},
		{"gpt:[kimi]qwen3.8-flash", "max", "xhigh"},
		{"provider/[tenant/kimi]deepseek-v4", "max", "max"},
		{"z-ai/glm-5.3", "max", "max"},
		{"notdeepseek-v4", "max", "max"},
		{"notgpt-6", "max", "max"},
		{"notgrok-4", "max", "max"},
		{"qwen30", "max", "max"},
		{"[tier]GROK-4", "max", "xhigh"},
		{"[gpt]gemini-3.1-pro", "max", "high"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := MapAutoEffort(tc.effort, tc.model, "auto"); got != tc.want {
				t.Fatalf("MapAutoEffort(%q, %q) = %q, want %q", tc.effort, tc.model, got, tc.want)
			}
		})
	}
}

func TestMapAutoEffortUpstreamPriority(t *testing.T) {
	for _, tc := range []struct{ upstream, requested, want string }{
		{"xai/grok-4.6", "deepseek-v4", "xhigh"},
		{"openai/gpt-6-luna", "qwen3.8", "max"},
		{"google/gemini-3", "gpt-6-luna", "high"},
		{"opaque", "gpt-6-luna", "max"},
		{"", "Qwen3.8-Flash", "xhigh"},
	} {
		if got := MapAutoEffort("max", tc.upstream, tc.requested); got != tc.want {
			t.Errorf("upstream=%q request=%q got=%q want=%q", tc.upstream, tc.requested, got, tc.want)
		}
	}
}

func TestMapAutoEffortEmptyAndCase(t *testing.T) {
	if got := MapAutoEffort("", "gpt-5.5", ""); got != "" {
		t.Fatalf("empty effort = %q, want empty", got)
	}
	if got := MapAutoEffort(" MAX ", "deepseek-v4", ""); got != "max" {
		t.Fatalf("case/space normalization = %q, want max", got)
	}
}
