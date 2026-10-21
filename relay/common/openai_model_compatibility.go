package common

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/openaimodel"
)

// NormalizeOpenAIRequest operates on the final, overridden JSON, preserving
// fields it does not own (including custom fields introduced by an override).
// Callers gate it on ChannelTypeOpenAI and bypass it for raw body passthrough.
// The bool result says a Chat request requires the Responses wire protocol.
func NormalizeOpenAIRequest(body []byte, responses bool) ([]byte, string, bool, error) {
	var identity struct {
		Model string `json:"model"`
	}
	if err := common.Unmarshal(body, &identity); err != nil {
		return nil, "", false, err
	}

	// namespace 工具展开必须在这里、且不受"模型是否已知"影响。
	//
	// Codex 桌面端声明工具时用的是 Responses 的 namespace 形态
	// ({"type":"namespace","name":"mcp","tools":[...]}),而绝大多数上游只认
	// 扁平化的 function 工具。原样转发就会得到:
	//   [400] Invalid namespace tool: name: Invalid input: expected string, received undefined
	// 这是"客户端发的形态在本机没被翻译",不是渠道故障,更不是客户端写错了——
	// 官方仓库有对应的 toolconv 实现,本仓库此前完全没有,所以这条路径一直是坏的。
	//
	// **Chat Completions 与 Responses 两条路径都走这里**:
	//   - Responses:NormalizeOpenAIRequest(..., responses=true)
	//   - Chat:relay.NormalizeOfficialChatRequest → NormalizeOpenAIRequest(..., false)
	// chat 形态下工具名嵌在 function 里,由展开函数自行兼容两种形态。
	//
	// 放在模型解析之前:模型未知时早退会跳过后面的规范化,畸形工具定义就原样打给
	// 上游了;而 namespace 展开与模型能力无关,任何时候都该做。
	//
	// 无法翻译时退化为"丢弃这些工具并继续",而不是让整条请求 400:官方同样把
	// 无法表示的 Responses 工具丢成诊断项再继续。
	expanded, err := ExpandNamespaceTools(body)
	if err != nil {
		return nil, "", false, err
	}
	body = expanded

	model, suffixEffort, capabilities, known := openaimodel.Resolve(identity.Model)
	if !known {
		return body, identity.Model, false, nil
	}
	var fields map[string]json.RawMessage
	if err := common.Unmarshal(body, &fields); err != nil {
		return nil, "", false, err
	}
	if model != identity.Model {
		fields["model"], _ = common.Marshal(model)
	}
	var effort string
	var hasTools bool
	if responses {
		var request dto.OpenAIResponsesRequest
		if err := common.Unmarshal(body, &request); err != nil {
			return nil, "", false, err
		}
		if request.Reasoning != nil {
			effort = request.Reasoning.Effort
		}
		if suffixEffort != "" {
			var reasoning map[string]json.RawMessage
			if raw := fields["reasoning"]; len(raw) > 0 {
				if err := common.Unmarshal(raw, &reasoning); err != nil {
					return nil, "", false, err
				}
			}
			if reasoning == nil {
				reasoning = make(map[string]json.RawMessage)
			}
			effort = suffixEffort
			reasoning["effort"], _ = common.Marshal(effort)
			fields["reasoning"], _ = common.Marshal(reasoning)
		}
		if clamped := capabilities.ClampEffort(effort); clamped != effort {
			effort = clamped
			var reasoning map[string]json.RawMessage
			if raw := fields["reasoning"]; len(raw) > 0 {
				if err := common.Unmarshal(raw, &reasoning); err != nil {
					return nil, "", false, err
				}
			}
			if reasoning == nil {
				reasoning = make(map[string]json.RawMessage)
			}
			reasoning["effort"], _ = common.Marshal(effort)
			fields["reasoning"], _ = common.Marshal(reasoning)
		}
	} else {
		var request dto.GeneralOpenAIRequest
		if err := common.Unmarshal(body, &request); err != nil {
			return nil, "", false, err
		}
		effort = request.ReasoningEffort
		if suffixEffort != "" {
			effort = suffixEffort
			fields["reasoning_effort"], _ = common.Marshal(effort)
		}
		if clamped := capabilities.ClampEffort(effort); clamped != effort {
			effort = clamped
			fields["reasoning_effort"], _ = common.Marshal(effort)
		}
		if capabilities.CompletionTokens {
			if request.MaxCompletionTokens == nil && request.MaxTokens != nil {
				fields["max_completion_tokens"] = fields["max_tokens"]
			}
			delete(fields, "max_tokens")
		}
		if capabilities.DeveloperRole && len(request.Messages) > 0 && request.Messages[0].Role == "system" {
			var messages []map[string]json.RawMessage
			if err := common.Unmarshal(fields["messages"], &messages); err != nil {
				return nil, "", false, err
			}
			messages[0]["role"] = json.RawMessage(`"developer"`)
			fields["messages"], _ = common.Marshal(messages)
		}
		hasTools = hasOpenAIChatTools(&request)
	}
	if capabilities.RemoveSampling(effort) {
		for _, name := range []string{"temperature", "top_p", "logprobs", "top_logprobs"} {
			delete(fields, name)
		}
		if responses && fields["include"] != nil {
			var include []string
			if err := common.Unmarshal(fields["include"], &include); err != nil {
				return nil, "", false, err
			}
			filtered := make([]string, 0, len(include))
			for _, value := range include {
				if value != "message.output_text.logprobs" {
					filtered = append(filtered, value)
				}
			}
			if len(filtered) != len(include) {
				fields["include"], _ = common.Marshal(filtered)
			}
		}
	}
	result, err := common.Marshal(fields)
	return result, model, !responses && capabilities.RequiresResponses(effort, hasTools), err
}

// defaultResponsesToolNamespace 是 Codex 声明普通工具时用的命名空间名。
// 官方对它的处理是"不加前缀"(保持裸名),因为上游按裸名回调用。
const defaultResponsesToolNamespace = "functions"

// ExpandNamespaceTools 把 type=namespace 的工具展开成扁平的 function 定义。
//
// 两种入参形态都支持,并按各自形态写回:
//   - Responses 扁平形态:{"type":"function","name":"x","description":"..."}
//   - chat 形态:        {"type":"function","function":{"name":"x","description":"..."}}
//
// 名字用 "<namespace>__<name>"(默认命名空间不加前缀),命名空间描述并入每个
// 成员描述 —— 与官方 toolconv 的做法一致。
//
// 为什么 chat 路径也需要它:Codex 等客户端在 Chat Completions 模式下同样会带
// namespace 工具,而 dto.ToolCallRequest 只能表达 ID/Type/Function/Custom,
// 反序列化会把 name 与成员数组整个丢掉,重序列化只剩 {"type":"namespace"}——
// 上游那句 "Invalid namespace tool: name: expected string, received undefined"
// 正是这么来的。所以必须在反序列化之前的原始 JSON 上处理。
//
// 不做的事(避免引入比问题更大的风险):
//   - 不改 tool_choice;
//   - 不维护反向映射:回包里的名字与发给上游的一致;
//   - 不改动非 namespace 的工具,原样保留。
//
// 幂等:没有 namespace 工具时原样返回,不重排。
func ExpandNamespaceTools(body []byte) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := common.Unmarshal(body, &envelope); err != nil {
		return body, nil //解码失败交给后面的路径报错,这里不改变行为
	}
	rawTools, ok := envelope["tools"]
	if !ok || len(rawTools) == 0 {
		return body, nil
	}
	var tools []map[string]json.RawMessage
	if err := common.Unmarshal(rawTools, &tools); err != nil {
		return body, nil
	}

	var (
		expanded  []json.RawMessage
		hasExpand bool
	)
	for _, tool := range tools {
		var typeName string
		if raw, ok := tool["type"]; ok {
			_ = common.Unmarshal(raw, &typeName)
		}
		if strings.TrimSpace(typeName) != "namespace" {
			raw, err := common.Marshal(tool)
			if err != nil {
				return body, err
			}
			expanded = append(expanded, raw)
			continue
		}

		hasExpand = true
		namespaceName, namespaceDescription, _ := toolNameAndDescription(tool)

		var members []map[string]json.RawMessage
		if raw, ok := tool["tools"]; ok {
			_ = common.Unmarshal(raw, &members)
		}

		// name 为空、或成员取不到名字:这组工具无法翻译成上游认得的 function 形态。
		// 丢弃并继续,而不是让整条请求失败——否则只是把"上游 400"换成"本机 400"。
		untranslatable := namespaceName == ""
		if !untranslatable {
			for _, member := range members {
				memberName, _, _ := toolNameAndDescription(member)
				if memberName == "" {
					untranslatable = true
					break
				}
			}
		}
		if untranslatable {
			common.SysLog(fmt.Sprintf(
				"drop untranslatable namespace tool (name=%q, members=%d): upstream needs flat function tools",
				namespaceName, len(members)))
			continue
		}

		for _, member := range members {
			memberName, memberDescription, memberNested := toolNameAndDescription(member)
			if namespaceName != defaultResponsesToolNamespace {
				setToolName(member, memberNested, namespaceName+"__"+memberName)
			}
			if description := strings.TrimSpace(namespaceDescription + "\n\n" + strings.TrimSpace(memberDescription)); description != "" {
				setToolDescription(member, memberNested, description)
			}
			raw, err := common.Marshal(member)
			if err != nil {
				return body, err
			}
			expanded = append(expanded, raw)
		}
	}

	if !hasExpand {
		return body, nil
	}
	encoded, err := common.Marshal(expanded)
	if err != nil {
		return body, err
	}
	envelope["tools"] = encoded
	return common.Marshal(envelope)
}

// toolNameAndDescription 读取工具定义的名字与描述,兼容扁平与 chat 两种形态。
// 第三个返回值指出名字是否嵌在 function 里,写回时据此保持同一形态。
func toolNameAndDescription(doc map[string]json.RawMessage) (name, description string, nested bool) {
	if raw, ok := doc["function"]; ok && len(raw) > 0 {
		var fn map[string]json.RawMessage
		if err := common.Unmarshal(raw, &fn); err == nil && fn != nil {
			if v, ok := fn["name"]; ok {
				_ = common.Unmarshal(v, &name)
			}
			if v, ok := fn["description"]; ok {
				_ = common.Unmarshal(v, &description)
			}
			return strings.TrimSpace(name), description, true
		}
	}
	if v, ok := doc["name"]; ok {
		_ = common.Unmarshal(v, &name)
	}
	if v, ok := doc["description"]; ok {
		_ = common.Unmarshal(v, &description)
	}
	return strings.TrimSpace(name), description, false
}

func setToolName(doc map[string]json.RawMessage, nested bool, name string) {
	encoded, err := common.Marshal(name)
	if err != nil {
		return
	}
	if !nested {
		doc["name"] = encoded
		return
	}
	var fn map[string]json.RawMessage
	_ = common.Unmarshal(doc["function"], &fn)
	if fn == nil {
		fn = map[string]json.RawMessage{}
	}
	fn["name"] = encoded
	if v, err := common.Marshal(fn); err == nil {
		doc["function"] = v
	}
}

func setToolDescription(doc map[string]json.RawMessage, nested bool, description string) {
	encoded, err := common.Marshal(description)
	if err != nil {
		return
	}
	if !nested {
		doc["description"] = encoded
		return
	}
	var fn map[string]json.RawMessage
	_ = common.Unmarshal(doc["function"], &fn)
	if fn == nil {
		fn = map[string]json.RawMessage{}
	}
	fn["description"] = encoded
	if v, err := common.Marshal(fn); err == nil {
		doc["function"] = v
	}
}

func hasOpenAIChatTools(request *dto.GeneralOpenAIRequest) bool {
	if len(request.Tools) > 0 || hasJSONValue(request.Functions) || hasJSONValue(request.FunctionCall) {
		return true
	}
	for _, message := range request.Messages {
		if message.Role == "tool" || message.Role == "function" ||
			hasJSONValue(message.ToolCalls) || hasJSONValue(message.FunctionCall) {
			return true
		}
	}
	return false
}

func hasJSONValue(value json.RawMessage) bool {
	return len(value) > 0 && string(value) != "null" && string(value) != "[]"
}
