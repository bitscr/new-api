package common

import (
	"bytes"
	"io"
	"strings"

	basecommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/reasoning"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyAutoReasoningEffort adapts only the attempt's cloned request. The original
// Request and BodyStorage retain the caller's effort for later model switches.
func ApplyAutoReasoningEffort(c *gin.Context, info *RelayInfo, request dto.Request) error {
	if c == nil || info == nil || info.ChannelMeta == nil {
		return nil
	}
	if _, auto := basecommon.GetContextKey(c, constant.ContextKeyAutoModelClientName); !auto {
		return nil
	}
	switch request := request.(type) {
	case *dto.GeneralOpenAIRequest:
		if request == nil {
			return nil
		}
		request.ReasoningEffort = reasoning.MapAutoEffort(request.ReasoningEffort, info.UpstreamModelName, info.OriginModelName)
		mapped, err := mapAutoEffortObject(request.Reasoning, info.UpstreamModelName, info.OriginModelName)
		if err != nil {
			return err
		}
		request.Reasoning = mapped
	case *dto.ClaudeRequest:
		if request == nil {
			return nil
		}
		mapped, err := mapAutoEffortObject(request.OutputConfig, info.UpstreamModelName, info.OriginModelName)
		if err != nil {
			return err
		}
		request.OutputConfig = mapped
	case *dto.GeminiChatRequest:
		if request != nil && request.GenerationConfig.ThinkingConfig != nil {
			config := request.GenerationConfig.ThinkingConfig
			config.ThinkingLevel = reasoning.MapAutoEffort(config.ThinkingLevel, info.UpstreamModelName, info.OriginModelName)
		}
	case *dto.OpenAIResponsesRequest:
		if request == nil || request.Reasoning == nil {
			return nil
		}
		original := request.Reasoning.Effort
		request.Reasoning.Effort = reasoning.MapAutoEffort(original, info.UpstreamModelName, info.OriginModelName)
		if original != "" && *request.Reasoning == (dto.Reasoning{}) {
			request.Reasoning = nil
		}
	}
	return nil
}

// PrepareEffortPassthrough keeps named-model traffic byte-for-byte unchanged.
// Auto modifies a private copy only; BodyStorage remains the retry source.
func PrepareEffortPassthrough(c *gin.Context, info *RelayInfo, storage basecommon.BodyStorage) (io.Reader, error) {
	auto := false
	if c != nil && info != nil && info.ChannelMeta != nil {
		_, auto = basecommon.GetContextKey(c, constant.ContextKeyAutoModelClientName)
	}
	body, err := storage.Bytes()
	if err != nil {
		if auto {
			return nil, err
		}
		return basecommon.ReaderOnly(storage), nil
	}
	if auto {
		modelName := info.UpstreamModelName
		// The selected adaptor owns model selection, regardless of ingress format.
		// Native Gemini uses its mapped URL model; body-model adaptors use raw model.
		if info.ApiType != constant.APITypeGemini {
			if model := gjson.GetBytes(body, "model"); model.Type == gjson.String && model.String() != "" {
				modelName = model.String()
			}
		}
		paths := []string{"reasoning_effort", "reasoning.effort", "output_config.effort"}
		for _, config := range []string{"generationConfig", "generation_config"} {
			for _, thinking := range []string{"thinkingConfig", "thinking_config"} {
				for _, level := range []string{"thinkingLevel", "thinking_level"} {
					paths = append(paths, config+"."+thinking+"."+level)
				}
			}
		}
		body, err = mapAutoEffortPaths(body, modelName, info.OriginModelName, paths...)
		if err != nil {
			return nil, err
		}
	}
	SetReasoningEffortFromRequest(info, body)
	SetConversationUpstreamRequest(info, body)
	if auto {
		return bytes.NewReader(body), nil
	}
	return basecommon.ReaderOnly(storage), nil
}

func mapAutoEffortPaths(body []byte, upstreamModel, requestedModel string, paths ...string) ([]byte, error) {
	omit := reasoning.MapAutoEffort("max", upstreamModel, requestedModel) == ""
	for _, path := range paths {
		value := gjson.GetBytes(body, path)
		if !value.Exists() || (!omit && value.Type != gjson.String) {
			continue
		}
		mapped := reasoning.MapAutoEffort(value.String(), upstreamModel, requestedModel)
		if !omit && mapped == value.String() {
			continue
		}
		var err error
		if omit {
			body, err = sjson.DeleteBytes(body, path)
			if dot := strings.LastIndexByte(path, '.'); err == nil && dot >= 0 {
				parent := gjson.GetBytes(body, path[:dot])
				if parent.IsObject() && len(parent.Map()) == 0 {
					body, err = sjson.DeleteBytes(body, path[:dot])
				}
			}
		} else {
			var encoded []byte
			encoded, err = basecommon.Marshal(mapped)
			if err == nil {
				body, err = sjson.SetRawBytes(body, path, encoded)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

// Remove the container only when removing effort made it empty. Other reasoning
// fields and an already-empty user-supplied container retain their semantics.
func mapAutoEffortObject(body []byte, upstreamModel, requestedModel string) ([]byte, error) {
	mapped, err := mapAutoEffortPaths(body, upstreamModel, requestedModel, "effort")
	if err != nil || bytes.Equal(body, mapped) {
		return mapped, err
	}
	if object := gjson.ParseBytes(mapped); object.IsObject() && len(object.Map()) == 0 {
		return nil, nil
	}
	return mapped, nil
}
