package reasoning

import "testing"

func TestMapAutoEffortMaxRouting(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  string
	}{
		{"openai/gpt-6-luna", "max"},
		{"deepseek-ai/deepseek-v4-flash-0731", "max"},
		{"nim/nvidia/glm-5.3-flash", "max"},
		{"intern/inkstone/qwen3.8-flash", "xhigh"},
		{"xai/grok-4.6", "xhigh"},
		{"moonshotai/kimi-k2.7-code", ""},
		{"MiniMaxAI/MiniMax-M3", ""},
		{"google/gemini-3.1-pro-preview", "high"},
		{"provider/unknown-model", "high"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := MapAutoEffort("max", tc.model, "auto"); got != tc.want {
				t.Fatalf("MapAutoEffort(max, %q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestMapAutoEffortModelNames(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"[次]deepseek-v4.1-flash", "max"},
		{"DeepSeek-V4-Flash-0731", "max"},
		{"deepseek-v4-flash:0731", "max"},
		{"channel:Qwen3.8-Flash", "xhigh"},
		{"[tier]GROK-4", "xhigh"},
		{"[gpt]gemini-3.1-pro", "high"},
		{"[kimi][tier]Qwen3.8-Flash", "xhigh"},
		{"gpt-proxy/qwen3.8-flash", "xhigh"},
		{"gpt-proxy/opaque", "high"},
		{"notdeepseek-v4", "high"},
		{"notgpt-6", "high"},
		{"notgrok-4", "high"},
		{"qwen30", "high"},
		{"qwen3", "xhigh"},
		{"prefix-qwen3.8-flash", "xhigh"},
		{"vendor/KimiK3", ""},
		{"vendor/MiniMaxM3", ""},
		{"gpt-free", "max"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := MapAutoEffort("max", tc.model, ""); got != tc.want {
				t.Fatalf("model %q mapped to %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestMapAutoEffortMisleadingPrefix(t *testing.T) {
	for _, label := range []string{"gpt", "kimi", "minimax"} {
		for _, terminal := range []string{"gemini-3.1-pro", "unknown-model"} {
			for _, model := range []string{label + ":" + terminal, label + "/" + terminal, "[" + label + "]" + terminal} {
				t.Run(model, func(t *testing.T) {
					for _, effort := range []string{"max", "medium", "low", "", "custom-level"} {
						want := effort
						if effort == "max" {
							want = "high"
						}
						if got := MapAutoEffort(effort, model, "auto"); got != want {
							t.Errorf("MapAutoEffort(%q, %q, auto) = %q, want %q; the terminal model must own the family", effort, model, got, want)
						}
					}
				})
			}
		}
	}
}

func TestMapAutoEffortBracketTagSlashRegression(t *testing.T) {
	for _, tc := range []struct{ effort, want string }{{"max", "high"}, {"medium", "medium"}} {
		if got := MapAutoEffort(tc.effort, "[tenant/kimi]gemini-3.1-pro", "auto"); got != tc.want {
			t.Errorf("MapAutoEffort(%q, [tenant/kimi]gemini-3.1-pro, auto) = %q, want %q", tc.effort, got, tc.want)
		}
	}
}

func TestMapAutoEffortBracketNamespacePrecedence(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"GpT-5.6", "max"}, {"deepseek-v4-flash", "max"}, {"glm-5.3", "max"},
		{"Qwen3.8-Flash", "xhigh"}, {"GROK-4", "xhigh"}, {"KimiK3", ""},
		{"MiniMaxM3", ""}, {"gemini-3.1-pro", "high"}, {"unknown-model", "high"},
	} {
		for _, prefix := range []string{
			"[tenant/kimi]",
			"[tenant:gpt/kimi]",
			"[gpt][tenant:kimi/minimax]",
			"[tenant/kimi]provider/",
			"provider/[tenant/kimi]",
			"[tenant/kimi]provider/[tier:minimax/gpt]",
			"gpt:[tenant/kimi]",
			"kimi:provider/[tenant/gpt]",
			"provider/[tenant/kimi]minimax:[tier:gpt/kimi]",
			"minimax:[scope:qwen3/gpt]provider/[tenant:kimi/gpt][tier:deepseek/minimax]",
			"provider:[tenant/kimi]gpt:[tier/minimax]",
			"[tenant/gpt]provider:[tier/minimax]kimi/[scope:grok/kimi]",
			"\t[tenant:gpt/kimi]  provider / [tier:minimax/kimi] ",
		} {
			t.Run(prefix+tc.model, func(t *testing.T) {
				for _, suffix := range []string{
					"", ":0731", ":2026-07-31", ":preview", ":PREVIEW:0731", ":0731:preview",
					":[tier/kimi]preview:[rev:gpt/minimax]2026.07_31",
				} {
					model := prefix + tc.model + suffix
					for _, effort := range []string{"max", "medium", "", "MAX", "custom-level"} {
						want := tc.want
						if effort != "max" && want != "" {
							want = effort
						}
						if got := MapAutoEffort(effort, model, "auto"); got != want {
							t.Errorf("MapAutoEffort(%q, %q, auto) = %q, want %q; bracket tags and namespace labels must not own the terminal model", effort, model, got, want)
						}
					}
				}
			})
		}
	}
}

func TestMapAutoEffortSlashDoesNotIntroduceRevision(t *testing.T) {
	for _, prefix := range []string{
		"gpt", "kimi", "minimax", "qwen3", "provider/[scope:gpt/kimi]gpt", "gpt:[tenant/kimi]kimi",
	} {
		for _, leaf := range []string{
			"0731", "2026-07-31", "preview", "0731:preview", "preview:0731", "[scope/gpt]0731:[rev/kimi]preview",
		} {
			model := prefix + "/" + leaf
			t.Run(model, func(t *testing.T) {
				for _, effort := range []string{"max", "medium", "", "custom-level"} {
					want := effort
					if effort == "max" {
						want = "high"
					}
					if got := MapAutoEffort(effort, model, "auto"); got != want {
						t.Errorf("MapAutoEffort(%q, %q, auto) = %q, want %q; slash leaves are not colon revision metadata", effort, model, got, want)
					}
				}
			})
		}
	}
}

func TestMapAutoEffortPreviewRevisionControls(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"GpT-5.6", "max"}, {"deepseek-v4-flash", "max"}, {"glm-5.3", "max"},
		{"Qwen3.8-Flash", "xhigh"}, {"grok-4", "xhigh"}, {"KimiK3", ""},
		{"MiniMaxM3", ""}, {"gemini-3.1-pro", "high"}, {"unknown-model", "high"},
	} {
		for _, prefix := range []string{"", "[Fast] vendor/", "gpt:", "kimi:[tier:gpt]"} {
			for _, suffix := range []string{":preview", ":preview:0731", ":0731:preview"} {
				model := prefix + tc.model + suffix
				t.Run(model, func(t *testing.T) {
					for _, effort := range []string{"max", "medium"} {
						want := tc.want
						if effort != "max" && want != "" {
							want = effort
						}
						if got := MapAutoEffort(effort, model, "auto"); got != want {
							t.Errorf("MapAutoEffort(%q, %q) = %q, want %q; revision tags must preserve the terminal model family", effort, model, got, want)
						}
					}
				})
			}
		}
	}
}

func TestMapAutoEffortNumericRevisionRequiresDigit(t *testing.T) {
	for _, family := range []struct{ label, wantMax string }{
		{"gpt", "max"}, {"deepseek", "max"}, {"glm", "max"},
		{"qwen3", "xhigh"}, {"grok", "xhigh"}, {"kimi", ""}, {"minimax", ""},
	} {
		for _, suffix := range []struct {
			value      string
			isRevision bool
		}{
			{"", false}, {"-", false}, {".", false}, {"_", false}, {"---", false}, {".-_", false},
			{"---:preview", false}, {"preview:_", false}, {"_:0731", false}, {"0731:", false},
			{"١", false}, {"１", false}, {"1a", false}, {"1+2", false}, {"1 2", false},
			{"0", true}, {"0123456789", true}, {"0731", true}, {"2026-07-31", true},
			{"2026.07_31", true}, {".-_0_-.", true},
			{"preview", true}, {"PREVIEW", true}, {"preview:0731", true}, {"0731:preview", true},
		} {
			model := family.label + ":" + suffix.value
			t.Run(model, func(t *testing.T) {
				wantMax, wantMedium := "high", "medium"
				if suffix.isRevision {
					wantMax = family.wantMax
					if wantMax == "" {
						wantMedium = ""
					}
				}
				for _, tc := range []struct{ effort, want string }{{"max", wantMax}, {"medium", wantMedium}} {
					if got := MapAutoEffort(tc.effort, model, "auto"); got != tc.want {
						t.Errorf("MapAutoEffort(%q, %q, auto) = %q, want %q", tc.effort, model, got, tc.want)
					}
				}
			})
		}
	}
}

func TestMapAutoEffortNamespaceVersionControls(t *testing.T) {
	for _, tc := range []struct{ model, want string }{
		{"deepseek-v4-flash:0731", "max"},
		{"channel:deepseek-v4-flash:0731", "max"},
		{"[kimi:tier]DeepSeek-V4-Flash:0731", "max"},
		{"gpt:deepseek-v4-flash:2026-07-31", "max"},
		{"kimi:Qwen3.8-Flash:0731", "xhigh"},
		{"gpt:[kimi][tier]GROK-4:0731", "xhigh"},
		{"channel:KimiK3:0731", ""},
		{"channel:MiniMaxM3:0731", ""},
		{"kimi:gpt-free:0731", "max"},
		{"minimax:[gpt]gemini-3.1-pro", "high"},
		{"gpt:[kimi]unknown-model", "high"},
		{"minimax:channel:gemini-3.1-pro:0731", "high"},
		{"kimi:unknown-model:0731", "high"},
		{"gpt:deployment-42", "high"},
		{"kimi:2026-unknown", "high"},
	} {
		t.Run(tc.model, func(t *testing.T) {
			if got := MapAutoEffort("max", tc.model, "auto"); got != tc.want {
				t.Errorf("MapAutoEffort(max, %q, auto) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestMapAutoEffortUpstreamPriority(t *testing.T) {
	for _, tc := range []struct{ upstream, requested, want string }{
		{"xai/grok-4.6", "deepseek-v4", "xhigh"},
		{"openai/gpt-6-luna", "qwen3.8", "max"},
		{"google/gemini-3", "gpt-6-luna", "high"},
		{"opaque", "gpt-6-luna", "high"},
		{"", "Qwen3.8-Flash", "xhigh"},
	} {
		if got := MapAutoEffort("max", tc.upstream, tc.requested); got != tc.want {
			t.Errorf("upstream=%q request=%q got=%q want=%q", tc.upstream, tc.requested, got, tc.want)
		}
	}
}

func TestMapAutoEffortNonMax(t *testing.T) {
	for _, model := range []string{"gpt-6-luna", "deepseek-v4", "glm-5", "qwen3.8", "grok-4.6", "gemini-3", "unknown", "kimi-k3", "minimax-m3"} {
		for _, effort := range []string{"", "minimal", "low", "medium", "high", "xhigh", "none", "MAX", "custom-level"} {
			want := effort
			if model == "kimi-k3" || model == "minimax-m3" {
				want = ""
			}
			if got := MapAutoEffort(effort, model, ""); got != want {
				t.Errorf("model=%q effort=%q got=%q want=%q", model, effort, got, want)
			}
		}
	}
}

func TestMapAutoEffortFamilyLevels(t *testing.T) {
	efforts := []string{"minimal", "low", "medium", "high", "xhigh", "max"}
	tests := []struct {
		name  string
		model string
		want  []string
	}{
		{
			name:  "DeepSeek",
			model: "deepseek-v4-flash-0731",
			want:  []string{"minimal", "low", "medium", "high", "xhigh", "max"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for i, effort := range efforts {
				t.Run(effort, func(t *testing.T) {
					if got := MapAutoEffort(effort, tt.model, ""); got != tt.want[i] {
						t.Errorf("MapAutoEffort(%q, %q, %q) = %q, want %q", effort, tt.model, "", got, tt.want[i])
					}
				})
			}
		})
	}
}
