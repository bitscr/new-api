package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

// IsAutoModelCandidateAuthorized applies the same whitelist matching as explicit
// model requests. A whitelist entry's presence, not its bool value, grants access.
func IsAutoModelCandidateAuthorized(c *gin.Context, modelName string) bool {
	if c == nil {
		return false
	}
	if !common.GetContextKeyBool(c, constant.ContextKeyTokenModelLimitEnabled) {
		return true
	}
	value, exists := common.GetContextKey(c, constant.ContextKeyTokenModelLimit)
	if !exists {
		return false
	}
	limits, ok := value.(map[string]bool)
	if !ok {
		return false
	}
	_, allowed := limits[ratio_setting.FormatMatchingModelName(modelName)]
	return allowed
}

// FilterAuthorizedAutoModelCandidates preserves the ranked order without
// modifying the caller's candidate snapshot.
func FilterAuthorizedAutoModelCandidates(c *gin.Context, candidates []string) []string {
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if IsAutoModelCandidateAuthorized(c, candidate) {
			result = append(result, candidate)
		}
	}
	return result
}
