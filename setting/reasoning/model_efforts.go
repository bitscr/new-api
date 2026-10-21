package reasoning

import (
	"strings"

	"github.com/QuantumNous/new-api/pkg/openaimodel"
)

// effortProfile 描述一个模型支持的思考档位（从低到高）。
// known=false 表示未收录，调用方使用 unknownEffortFallback。
// omit=true 表示该模型不接受 reasoning_effort 参数，应直接省略。
type effortProfile struct {
	efforts []string
	known   bool
	omit    bool
}

// 未收录模型的兜底档位。请求值不在其中时（none/minimal 除外）取最高档 max。
var unknownEffortFallback = []string{"low", "high", "max"}

// 本地思考档位表（2026-10-21 核对）：
//   - OpenAI：优先使用 pkg/openaimodel 的精确模型能力表（gpt-5.5 最高 xhigh，
//     gpt-5.6/gpt-6 支持 max 等）。
//   - Claude：官方固定 low/medium/high/max。
//   - Gemini：3.6 Flash 为 minimal/low/medium/high；3.1 Pro 为 low/high；
//     其余 Gemini 3/4 使用 low/medium/high（不支持 max）。
//   - DeepSeek：固定 none/low/high/max。
//   - Qwen3.8：固定 low/medium/xhigh；旧 Qwen 固定 low/medium/high/xhigh。
//   - GLM 5.1：固定 none/minimal/high/xhigh；
//     5.2：固定 none/minimal/high/max；
//     5.3：固定 low/high/max。
//   - Kimi K3：low/high/max；K2 及更早不接受 effort。
//   - Grok：low/medium/high/xhigh。
//   - MiniMax/Mistral/Llama 等：不接受 effort。
//
// 表里只放官方固定档位。传入值命中就保留；不命中就取该表最高档，
// 不按官方兼容映射值改写（例如 DeepSeek medium 不在固定表中，直接取 max）。
func resolveEffortProfile(modelName string) effortProfile {
	lowerFull := strings.ToLower(strings.TrimSpace(modelName))
	// Cohere 的模型名（如 north-mini-code）不含家族词，只能靠命名空间识别。
	if strings.HasPrefix(lowerFull, "cohere/") || strings.HasPrefix(lowerFull, "command/") {
		return effortProfile{known: true, omit: true}
	}
	leaf := leafModelName(modelName)
	if leaf == "" {
		return effortProfile{efforts: unknownEffortFallback}
	}
	if _, _, capabilities, known := openaimodel.Resolve(leaf); known {
		if !capabilities.Reasoning {
			return effortProfile{known: true, omit: true}
		}
		if capabilities.Efforts == "" {
			// 已知支持思考但官方未列出档位：保留调用方传入值。
			return effortProfile{known: true}
		}
		return effortProfile{known: true, efforts: splitEfforts(capabilities.Efforts)}
	}

	switch autoEffortFamily(leaf) {
	case "gpt":
		return knownEfforts("low", "medium", "high", "xhigh", "max")
	case "claude":
		return knownEfforts("low", "medium", "high", "max")
	case "gemini":
		if strings.Contains(leaf, "3.6-flash") {
			return knownEfforts("minimal", "low", "medium", "high")
		}
		if strings.Contains(leaf, "3.1-pro") || strings.Contains(leaf, "3-pro") {
			return knownEfforts("low", "high")
		}
		return knownEfforts("low", "medium", "high")
	case "deepseek":
		return knownEfforts("none", "low", "high", "max")
	case "qwen3":
		if strings.Contains(leaf, "3.8") {
			return knownEfforts("low", "medium", "xhigh")
		}
		return knownEfforts("low", "medium", "high", "xhigh")
	case "qwen":
		return knownEfforts("low", "medium", "high", "xhigh")
	case "glm":
		if strings.Contains(leaf, "5.1") {
			return knownEfforts("none", "minimal", "high", "xhigh")
		}
		if strings.Contains(leaf, "5.2") {
			return knownEfforts("none", "minimal", "high", "max")
		}
		// 5.3 及未知 GLM 版本。
		return knownEfforts("low", "high", "max")
	case "kimi":
		if strings.Contains(leaf, "k3") {
			return knownEfforts("low", "high", "max")
		}
		return effortProfile{known: true, omit: true}
	case "minimax", "mistral", "mixtral", "llama", "cohere", "command",
		"ernie", "hunyuan", "doubao", "step", "yi", "internlm":
		return effortProfile{known: true, omit: true}
	case "grok":
		return knownEfforts("low", "medium", "high", "xhigh")
	}
	return effortProfile{efforts: unknownEffortFallback}
}

func knownEfforts(efforts ...string) effortProfile {
	return effortProfile{known: true, efforts: efforts}
}

func splitEfforts(value string) []string {
	parts := strings.Split(value, ",")
	efforts := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			efforts = append(efforts, trimmed)
		}
	}
	return efforts
}

// leafModelName 去掉 [标签]、命名空间前缀，返回小写的模型叶子名。
// 冒号保留：它可能是版本后缀（:0731），版本判断需要它。
func leafModelName(modelName string) string {
	name := stripBracketTags(modelName)
	name = strings.TrimSpace(name)
	if separator := strings.LastIndexByte(name, '/'); separator >= 0 {
		name = name[separator+1:]
	}
	return strings.ToLower(strings.TrimSpace(name))
}

func stripBracketTags(name string) string {
	var builder strings.Builder
	depth := 0
	for _, r := range name {
		switch r {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				builder.WriteRune(r)
			}
		}
	}
	return builder.String()
}
