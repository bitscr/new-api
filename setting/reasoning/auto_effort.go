package reasoning

import "strings"

// MapAutoEffort 把请求里的思考档位适配到目标模型支持的档位。
//
// 规则：
//   - 模型明确不支持思考（kimi 旧版、minimax、mistral 等）→ 省略该参数；
//   - none 永远表示“关闭思考”，原样保留，不参与最高档收敛；
//   - 传入值在模型官方固定档位表中 → 原样保留；
//   - 不在表中 → 取该表最高档（不做任何别名映射）；
//   - 未知模型 → 兜底固定档位 low/high/max，不在表中时取最高档 max。
//
// 已知 OpenAI 模型优先使用 pkg/openaimodel 的能力表；其余主流模型使用
// resolveEffortProfile 中的本地表。表在部署机器上静态生效，不依赖运行时探测。
func MapAutoEffort(effort, upstreamModel, requestedModel string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return ""
	}
	modelName := upstreamModel
	if strings.TrimSpace(modelName) == "" {
		modelName = requestedModel
	}
	profile := resolveEffortProfile(modelName)
	if profile.omit {
		return ""
	}
	// none 是“关闭思考”的固定语义，不能被抬成最高思考档。
	if effort == "none" {
		return "none"
	}
	if profile.known && len(profile.efforts) == 0 {
		// 已知模型但官方未列出档位（例如部分 codex 模型）：不擅自改写。
		return effort
	}
	return clampEfforts(profile.efforts, effort)
}

// clampEfforts：命中固定档位保留，否则取最高档。没有别名映射。
func clampEfforts(efforts []string, effort string) string {
	for _, supported := range efforts {
		if supported == effort {
			return effort
		}
	}
	return efforts[len(efforts)-1]
}

// autoEffortFamily 返回模型终端名称里的家族 token。
// 它只负责识别家族；档位表由 resolveEffortProfile 负责。
func autoEffortFamily(modelName string) string {
	modelName = strings.ToLower(strings.TrimSpace(modelName))
	// Consume leading bracket tags at every segment boundary before looking
	// for namespace separators: slashes and colons inside tags are not delimiters.
	var segments []string
	for {
		if strings.HasPrefix(modelName, "[") {
			end := strings.IndexByte(modelName, ']')
			if end < 0 {
				segments = append(segments, modelName)
				break
			}
			modelName = strings.TrimSpace(modelName[end+1:])
			continue
		}
		separator := strings.IndexAny(modelName, "/:")
		if separator < 0 {
			segments = append(segments, strings.TrimSpace(modelName))
			break
		}
		if modelName[separator] == '/' {
			// A slash starts a new namespace leaf, even when that leaf is numeric
			// or preview. Only colon-separated suffixes can be revision metadata.
			segments = segments[:0]
		} else {
			segments = append(segments, strings.TrimSpace(modelName[:separator]))
		}
		modelName = strings.TrimSpace(modelName[separator+1:])
	}
	// Revision metadata belongs to the preceding model. Peel from the right
	// so stacked preview/date tags do not hide that model's actual family.
	for len(segments) > 1 {
		suffix := segments[len(segments)-1]
		numeric := strings.ContainsAny(suffix, "0123456789") && strings.Trim(suffix, "0123456789.-_") == ""
		if suffix != "preview" && !numeric {
			break
		}
		segments = segments[:len(segments)-1]
	}
	modelName = segments[len(segments)-1]
	parts := strings.FieldsFunc(modelName, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	// The rightmost family token wins over a separated channel/prefix label.
	for i := len(parts) - 1; i >= 0; i-- {
		part := parts[i]
		switch part {
		case "gpt", "claude", "gemini", "deepseek", "glm", "qwen3", "qwen", "grok",
			"llama", "mistral", "mixtral", "cohere", "command", "ernie", "hunyuan",
			"doubao", "step", "yi", "internlm":
			return part
		}
		if strings.HasPrefix(part, "kimi") {
			return "kimi"
		}
		if strings.HasPrefix(part, "minimax") {
			return "minimax"
		}
		if strings.HasPrefix(part, "internlm") {
			return "internlm"
		}
	}
	return ""
}
