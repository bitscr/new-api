package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRecordAutoModelUnusableAnswerCoolsModelOnly 锁定新语义：
// "200 但没有有效回答"（空正文或上游网关的告警横幅）按失败折算分数，
// 但只冷却"该渠道下的该模型"，不连累该上游的其它模型。
func TestRecordAutoModelUnusableAnswerCoolsModelOnly(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	RecordAutoModelUnusableAnswer(group, "banner-model", 7, "响应正文只是上游网关的告警横幅：[Gateway Warning: 当前渠道负")

	rows := cooldownRows(t)
	require.Len(t, rows, 1, "只该有一条冷却记录")
	require.Equal(t, "banner-model", rows[0].Model, "必须是模型级冷却，不能是渠道级（Model 为空）")
	require.Equal(t, 7, rows[0].ChannelId)
	require.Contains(t, rows[0].Reason, "告警横幅", "冷却原因要带上判定依据")
	require.True(t, autoModelCooldownActive(group, "banner-model", []int{7}))
	require.False(t, autoModelCooldownActive(group, "other-model", []int{7}),
		"同渠道的其它模型不该被连带冷却")

	outcome, ok := autoModelHealth.Get(autoModelHealthKey(group, "banner-model", 7))
	require.True(t, ok, "应该记下这次观测")
	require.InDelta(t, 1, outcome.Observations, 1e-9)
	require.Less(t, outcome.Score, 0.5, "不计成功：分数要向失败方向移动")

	// 再犯一次：等级递增（窗口翻倍），仍然是模型级
	RecordAutoModelUnusableAnswer(group, "banner-model", 7, "响应正文只是上游网关的告警横幅")
	rows = cooldownRows(t)
	require.Len(t, rows, 1, "等级递增不该新增记录")
	require.Equal(t, 2, rows[0].Level)
}
