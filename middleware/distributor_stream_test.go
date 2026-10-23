package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGetModelRequestPreservesStreamMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, path, body string
		model            string
		stream           bool
	}{
		{"chat stream", "/v1/chat/completions", `{"model":"gpt-4o","stream":true}`, "gpt-4o", true},
		{"chat non-stream", "/v1/chat/completions", `{"model":"gpt-4o","stream":false}`, "gpt-4o", false},
		{"claude stream", "/v1/messages", `{"model":"claude-sonnet","stream":true}`, "claude-sonnet", true},
		{"responses stream", "/v1/responses", `{"model":"gpt-5","stream":true}`, "gpt-5", true},
		{"playground stream", "/pg/chat/completions", `{"model":"gpt-4o","group":"default","stream":true}`, "gpt-4o", true},
		{"playground non-stream", "/pg/chat/completions", `{"model":"gpt-4o","group":"default"}`, "gpt-4o", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			request, shouldSelect, err := getModelRequest(c)
			require.NoError(t, err)
			require.True(t, shouldSelect)
			require.Equal(t, tc.model, request.Model)
			require.Equal(t, tc.stream, request.Stream)
		})
	}
}
