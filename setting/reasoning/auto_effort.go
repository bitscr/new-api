package reasoning

import "strings"

// MapAutoEffort adapts only max, except for families that do not accept effort.
// A supplied upstream model is authoritative, including an unknown alias; only
// an absent upstream name falls back to the selected request model.
func MapAutoEffort(effort, upstreamModel, requestedModel string) string {
	modelName := upstreamModel
	if strings.TrimSpace(modelName) == "" {
		modelName = requestedModel
	}
	family := autoEffortFamily(modelName)
	switch family {
	case "kimi", "minimax":
		return ""
	case "gpt", "deepseek", "glm":
		return effort
	}
	if effort != "max" {
		return effort
	}
	if family == "qwen3" || family == "grok" {
		return "xhigh"
	}
	return "high"
}

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
		case "gpt", "deepseek", "glm", "qwen3", "grok":
			return part
		}
		if strings.HasPrefix(part, "kimi") {
			return "kimi"
		}
		if strings.HasPrefix(part, "minimax") {
			return "minimax"
		}
	}
	return ""
}
