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

}


// 一键清除负面效果:得分恢复中立 0.5、延迟惩罚清零、冷却解除;
// 没有观测数据的组合是 no-op。
func TestClearAutoModelNegativeEffects(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	// 没有观测数据:无事可做
	require.False(t, ClearAutoModelNegativeEffects("default", "ghost", 9))

	// 制造负面效果:连续失败压低得分并进入冷却
	RecordAutoModelOutcome("default", "bad-model", 3, false, 100, 30000)
	RecordAutoModelOutcome("default", "bad-model", 3, false, 100, 30000)
	scores := ListAutoModelScores(time.Now())
	require.Len(t, scores, 1)
	require.Less(t, scores[0].Score, 0.5, "连续失败后得分应低于中立")
	require.True(t, scores[0].Cooling, "失败应产生冷却")
	require.Greater(t, scores[0].LatencyMS, float64(0), "失败耗时进入延迟 EWMA")

	require.True(t, ClearAutoModelNegativeEffects("default", "bad-model", 3))

	scores = ListAutoModelScores(time.Now())
	require.Len(t, scores, 1, "清除后条目保留")
	require.InDelta(t, 0.5, scores[0].Score, 1e-9, "得分应回到中立 0.5")
	require.Equal(t, 0.0, scores[0].LatencyMS, "延迟惩罚应清零")
	require.False(t, scores[0].Cooling, "冷却应一并解除")
}
