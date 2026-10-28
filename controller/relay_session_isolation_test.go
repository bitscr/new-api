package controller

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

// Both machines share authentication and the last question, but their complete
// contexts are different. Force their upstream responses to overlap and verify
// isolation through authentication, conversion, SSE delivery and all four
// conversation-capture stages, rather than just testing independent buffers.
func TestRelayConcurrentMachinesWithSameKeyKeepContextsIsolated(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			const question = "调用工具查询本机配置，详细完整点"
			const machineA = "machine_A_windows_E_workspace"
			const machineB = "machine_B_linux_app_other_workspace"
			var arrived atomic.Int32
			bothArrived := make(chan struct{})
			upstreamErrors := make(chan error, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					} `json:"messages"`
					PromptCacheKey string `json:"prompt_cache_key"`
				}
				if err := common.DecodeJson(r.Body, &body); err != nil {
					upstreamErrors <- err
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(body.Messages) != 2 || body.Messages[0].Role != "system" ||
					body.Messages[1].Content != question || body.PromptCacheKey != "shared_cache_prefix" {
					upstreamErrors <- fmt.Errorf("unexpected upstream context: %+v", body)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				machine := body.Messages[0].Content
				if machine != machineA && machine != machineB {
					upstreamErrors <- fmt.Errorf("unexpected machine: %q", machine)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"%s\",\"model\":\"jev-latest\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"%s\"}}]}\n\n", machine, machine)
					w.(http.Flusher).Flush()
				}
				if arrived.Add(1) == 2 {
					close(bothArrived)
				}
				select {
				case <-bothArrived:
				case <-r.Context().Done():
					return
				case <-time.After(5 * time.Second):
					upstreamErrors <- fmt.Errorf("requests did not overlap")
					return
				}
				if stream {
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"%s\",\"model\":\"jev-latest\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"_configuration\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\ndata: [DONE]\n\n", machine)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"id":"%s","model":"jev-latest","choices":[{"index":0,"message":{"role":"assistant","content":"%s_configuration"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`, machine, machine)
				}
			}))
			defer upstream.Close()
			db, engine, channel := setupUsageLogGateway(t, upstream.URL)
			resetAutoModelCooldownState(db)
			channel.SetOtherSettings(dto.ChannelOtherSettings{ConversationLogEnabled: true})
			require.NoError(t, channel.Update())

			type result struct {
				machine  string
				response *httptest.ResponseRecorder
			}
			results := make(chan result, 2)
			for _, machine := range []string{machineA, machineB} {
				payload, err := common.Marshal(map[string]any{
					"model": "jev-latest", "stream": stream, "prompt_cache_key": "shared_cache_prefix",
					"messages": []map[string]string{{"role": "system", "content": machine}, {"role": "user", "content": question}},
				})
				require.NoError(t, err)
				go func(machine string, payload []byte) {
					request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Authorization", "Bearer "+typeSafeGatewayKey)
					response := httptest.NewRecorder()
					engine.ServeHTTP(response, request)
					results <- result{machine, response}
				}(machine, payload)
			}
			requestIDs := make(map[string]string)
			for i := 0; i < 2; i++ {
				select {
				case result := <-results:
					require.Equal(t, http.StatusOK, result.response.Code, result.response.Body.String())
					require.Contains(t, result.response.Body.String(), result.machine)
					otherMachine := machineA
					if result.machine == machineA {
						otherMachine = machineB
					}
					require.NotContains(t, result.response.Body.String(), otherMachine)
					requestIDs[result.machine] = result.response.Header().Get(common.RequestIdKey)
				case <-time.After(10 * time.Second):
					t.Fatal("gateway requests did not complete")
				}
			}
			close(upstreamErrors)
			for err := range upstreamErrors {
				require.NoError(t, err)
			}
			require.EqualValues(t, 2, arrived.Load())
			require.NotEmpty(t, requestIDs[machineA])
			require.NotEqual(t, requestIDs[machineA], requestIDs[machineB])
			var captures []model.ConversationLog
			require.NoError(t, db.Find(&captures).Error)
			require.Len(t, captures, 2)
			for _, capture := range captures {
				machine, otherMachine := machineA, machineB
				if strings.Contains(capture.ClientRequestBody, machineB) {
					machine, otherMachine = machineB, machineA
				}
				require.Equal(t, requestIDs[machine], capture.RequestId)
				require.Equal(t, 1, capture.TokenId)
				for stage, body := range map[string]string{
					"client_request": capture.ClientRequestBody, "upstream_request": capture.UpstreamRequestBody,
					"upstream_response": capture.UpstreamResponseBody, "client_response": capture.ClientResponseBody,
				} {
					require.Contains(t, body, machine, stage)
					require.NotContains(t, body, otherMachine, stage)
				}
				require.Equal(t, machine+"_configuration", capture.DerivedAssistantText)
				var metadata map[string]any
				require.NoError(t, common.UnmarshalJsonStr(capture.Metadata, &metadata))
				require.Equal(t, "checked=true", metadata["upstream_session_headers"], "the export must distinguish absent known session headers from missing evidence")
			}
		})
	}
}
