package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestListAutoModelScores(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	oldHealth := autoModelHealth.ReadAll()
	t.Cleanup(func() {
		autoModelHealth.Clear()
		autoModelHealth.AddAll(oldHealth)
	})

	now := time.Now()
	autoModelHealth.Clear()
	autoModelHealth.Set(autoModelHealthKey("default", "fast-model", 1), autoModelOutcome{
		Score: 0.9, LatencyMS: 100, Observations: 5, UpdatedAt: now,
	})
	autoModelHealth.Set(autoModelHealthKey("default", "slow-model", 2), autoModelOutcome{
		Score: 0.2, LatencyMS: 2000, Observations: 2, UpdatedAt: now,
	})

	// 给 slow-model 建一条冷却,验证得分明细能带出冷却等级。
	tripAutoModelCooldown("default", "slow-model", 2, "请求失败", now)

	scores := ListAutoModelScores(now)
	require.Len(t, scores, 2)
	require.Equal(t, "fast-model", scores[0].Model, "应按得分从高到低排序")
	require.Greater(t, scores[0].Score, scores[1].Score)

	require.Equal(t, "slow-model", scores[1].Model)
	require.True(t, scores[1].Cooling)
	require.Equal(t, 1, scores[1].CooldownLevel)
	require.False(t, scores[1].Permanent)
}