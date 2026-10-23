package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func callUpdateOption(t *testing.T, body string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/option/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	UpdateOption(c)
	var response map[string]any
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func TestUpdateOptionRejectsUnreachableMinObservations(t *testing.T) {
	response := callUpdateOption(t, `{"key":"AutoModelMinObservations","value":101}`)
	require.Equal(t, false, response["success"])
	message, _ := response["message"].(string)
	require.Contains(t, message, "1～70")
}

func TestUpdateOptionRequiresPairedExploreRatios(t *testing.T) {
	response := callUpdateOption(t, `{"key":"AutoModelExploreRatio","value":0.02}`)
	require.Equal(t, false, response["success"])
	message, _ := response["message"].(string)
	require.Contains(t, message, "成对")
}
