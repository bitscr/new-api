package service

import (
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
)

func TestFilterAuthorizedAutoModelCandidates(t *testing.T) {
	candidates := []string{"denied", "gpt-4-gizmo-custom", "allowed"}
	tests := []struct {
		name    string
		enabled bool
		limits  any
		want    []string
	}{
		{"unrestricted", false, nil, candidates},
		{"missing whitelist", true, nil, []string{}},
		{"invalid whitelist", true, "allowed", []string{}},
		{"empty whitelist", true, map[string]bool{}, []string{}},
		{"rank and normalized match", true, map[string]bool{"allowed": true, "gpt-4-gizmo-*": false}, []string{"gpt-4-gizmo-custom", "allowed"}},
		{"auto name grants no concrete access", true, map[string]bool{"auto": true}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, tt.enabled)
			if tt.limits != nil {
				common.SetContextKey(c, constant.ContextKeyTokenModelLimit, tt.limits)
			}
			got := FilterAuthorizedAutoModelCandidates(c, candidates)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("filtered = %v, want %v", got, tt.want)
			}
			if candidates[0] != "denied" || candidates[1] != "gpt-4-gizmo-custom" {
				t.Fatal("input candidate snapshot mutated")
			}
		})
	}
}
