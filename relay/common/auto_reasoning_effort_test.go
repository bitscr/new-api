package common

import (
	"bytes"
	"io"
	"testing"

	basecommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAutoEffortPassthroughOmitsUnsupportedFields(t *testing.T) {
	for _, effort := range []string{`"max"`, `""`, `null`} {
		t.Run(effort, func(t *testing.T) {
			body := []byte(`{"model":"kimi-k3","reasoning_effort":` + effort + `,"reasoning":{"effort":` + effort + `},"output_config":{"effort":` + effort + `,"keep":false},"thinking":{"type":"enabled"},"large":9007199254740993,"zero":0}`)
			storage, err := basecommon.CreateBodyStorage(body)
			require.NoError(t, err)
			t.Cleanup(func() { _ = storage.Close() })
			c := &gin.Context{}
			basecommon.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
			info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "kimi-k3"}}
			reader, err := PrepareEffortPassthrough(c, info, storage)
			require.NoError(t, err)
			mapped, err := io.ReadAll(reader)
			require.NoError(t, err)
			for _, path := range []string{"reasoning_effort", "reasoning", "output_config.effort"} {
				require.False(t, gjson.GetBytes(mapped, path).Exists(), string(mapped))
			}
			require.Equal(t, "false", gjson.GetBytes(mapped, "output_config.keep").Raw)
			require.Equal(t, "enabled", gjson.GetBytes(mapped, "thinking.type").String())
			require.Equal(t, "9007199254740993", gjson.GetBytes(mapped, "large").Raw)
			require.Equal(t, "0", gjson.GetBytes(mapped, "zero").Raw)
			original, err := storage.Bytes()
			require.NoError(t, err)
			require.Equal(t, body, original)
		})
	}
}

func TestAutoEffortPassthroughRetryUsesPrivateBody(t *testing.T) {
	body := []byte(` {"model":"gpt-public","reasoning_effort":"max","reasoning":{"effort":"max","enabled":false,"budget_tokens":0},"large":9007199254740993} `)
	original := bytes.Clone(body)
	storage, err := basecommon.CreateBodyStorage(body)
	require.NoError(t, err)
	t.Cleanup(func() { _ = storage.Close() })
	c := &gin.Context{}
	basecommon.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	info := &RelayInfo{
		RelayFormat: types.RelayFormatOpenAI,
		ChannelMeta: &ChannelMeta{ApiType: constant.APITypeGemini, UpstreamModelName: "gemini-3.1-pro"},
	}
	firstReader, err := PrepareEffortPassthrough(c, info, storage)
	require.NoError(t, err)
	first, err := io.ReadAll(firstReader)
	require.NoError(t, err)
	for _, path := range []string{"reasoning_effort", "reasoning.effort"} {
		require.Equal(t, "high", gjson.GetBytes(first, path).String())
	}
	require.Equal(t, "high", info.ReasoningEffort)
	retained, err := storage.Bytes()
	require.NoError(t, err)
	require.Equal(t, original, retained)
	require.Equal(t, "false", gjson.GetBytes(first, "reasoning.enabled").Raw)
	require.Equal(t, "0", gjson.GetBytes(first, "reasoning.budget_tokens").Raw)
	require.Equal(t, "9007199254740993", gjson.GetBytes(first, "large").Raw)

	// The next channel uses body.model, not the previous Gemini URL model.
	info.ApiType, info.UpstreamModelName = constant.APITypeOpenAI, "gpt-free"
	secondReader, err := PrepareEffortPassthrough(c, info, storage)
	require.NoError(t, err)
	second, err := io.ReadAll(secondReader)
	require.NoError(t, err)
	require.Equal(t, original, second)
	require.Equal(t, "max", info.ReasoningEffort)
	require.Equal(t, "high", gjson.GetBytes(first, "reasoning_effort").String(), "attempt bodies must not alias each other")
	retained, err = storage.Bytes()
	require.NoError(t, err)
	require.Equal(t, original, retained)
}

func TestAutoEffortObjectOmitsUnsupportedEmptyAndNull(t *testing.T) {
	for _, effort := range []string{`"max"`, `""`, `null`} {
		body := []byte(`{"effort":` + effort + `,"enabled":false}`)
		mapped, err := mapAutoEffortObject(body, "minimax-m3", "")
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(mapped, "effort").Exists(), string(mapped))
		require.Equal(t, "false", gjson.GetBytes(mapped, "enabled").Raw)
	}
}

func TestAutoEffortRequestGuards(t *testing.T) {
	request := &dto.GeneralOpenAIRequest{ReasoningEffort: "max"}
	require.NoError(t, ApplyAutoReasoningEffort(nil, nil, request))
	c := &gin.Context{}
	info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "qwen3.8"}}
	require.NoError(t, ApplyAutoReasoningEffort(c, info, request))
	require.Equal(t, "max", request.ReasoningEffort)
	basecommon.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	for _, req := range []dto.Request{(*dto.GeneralOpenAIRequest)(nil), (*dto.OpenAIResponsesRequest)(nil), (*dto.ClaudeRequest)(nil), (*dto.GeminiChatRequest)(nil), nil} {
		require.NoError(t, ApplyAutoReasoningEffort(c, info, req))
	}
}
