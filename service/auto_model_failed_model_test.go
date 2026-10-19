package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAutoModelFailedModelExclusionIsRequestLocalAndGroupScoped(t *testing.T) {
	c, _ := gin.CreateTestContext(nil)
	MarkAutoModelModelFailed(c, "g", "failed")

	require.True(t, IsAutoModelModelFailed(c, "g", "failed"))
	require.False(t, IsAutoModelModelFailed(c, "g", "healthy"))
	require.False(t, IsAutoModelModelFailed(c, "other", "failed"))

	fresh, _ := gin.CreateTestContext(nil)
	require.False(t, IsAutoModelModelFailed(fresh, "g", "failed"))
	MarkAutoModelModelFailed(c, "", "ignored")
	MarkAutoModelModelFailed(c, "g", "")
	require.True(t, IsAutoModelModelFailed(c, "g", "failed"))
}

func TestAutoModelRouteBuilderDropsFailedModelAcrossChannels(t *testing.T) {
	now := time.Unix(1700000000, 0)
	snapshot := autoModelRouteSnapshot{
		Now: now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"failed":  {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 1}},
			"healthy": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 1}},
		},
		ExcludedModels: map[string]bool{
			autoModelFailedModelKey("g", "failed"): true,
		},
	}

	got := buildAutoModelRoutesFromSnapshot("g", []string{"failed", "healthy"}, snapshot, func() float64 { return 0 })
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "healthy", ChannelID: 1},
		{Group: "g", ModelName: "healthy", ChannelID: 2},
	}, got, "a model-scoped failure must remove every channel route for that model")

	otherGroup := buildAutoModelRoutesFromSnapshot("other", []string{"failed", "healthy"}, snapshot, func() float64 { return 0 })
	require.Len(t, otherGroup, 4, "the exclusion must not leak into another group")
}
