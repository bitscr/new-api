package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestClearAutoModelNegativeEffectsBatch(t *testing.T) {
	openAutoModelRouteControllerTestDB(t)
	group := "batch-clear-" + t.Name()
	service.RecordAutoModelOutcome(group, "a", 910001, false, 100, 100)
	service.RecordAutoModelOutcome(group, "b", 910002, false, 100, 100)

	body := `{"items":[` +
		`{"group":"` + group + `","model":"a","channel_id":910001},` +
		`{"group":"` + group + `","model":"a","channel_id":910001},` +
		`{"group":"` + group + `","model":"b","channel_id":910002},` +
		`{"group":"` + group + `","model":"ghost","channel_id":910003}` +
		`]}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/option/auto_model_scores/batch", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	ClearAutoModelNegativeEffectsBatch(c)

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			ClearedCount int `json:"cleared_count"`
			SkippedCount int `json:"skipped_count"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	require.Equal(t, 2, response.Data.ClearedCount, "duplicate rows must count once")
	require.Equal(t, 1, response.Data.SkippedCount, "a row without observations is skipped")

	for _, row := range service.ListAutoModelScores(time.Now()) {
		if row.Group != group {
			continue
		}
		require.InDelta(t, 0.5, row.Score, 1e-9, "cleared rows return to neutral")
		require.False(t, row.Cooling, "cleared rows must not remain cooling")
	}
}
