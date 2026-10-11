package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

// ListAutoModelCooldowns 必须如实反映当前生效的冷却:分组、模型、渠道、原因、
// 等级、是否永久、剩余时长。没有它,管理员在故障时看不到 auto 到底挡住了谁。
func TestListAutoModelCooldownsReportsActiveRows(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()

	RecordAutoModelPermanentFailure("default", "ghost-model", 3, "模型不存在(model_not_found)")
	applyAutoModelCooldown("default", "flaky-model", 7, false, 30000)

	rows := ListAutoModelCooldowns(now)
	require.Len(t, rows, 2)

	byModel := make(map[string]AutoModelCooldownInfo, len(rows))
	for _, row := range rows {
		byModel[row.Model] = row
	}

	ghost, ok := byModel["ghost-model"]
	require.True(t, ok)
	require.Equal(t, "default", ghost.Group)
	require.Equal(t, 3, ghost.ChannelID)
	require.True(t, ghost.Permanent, "永久级冷却必须能被看出来")
	require.Equal(t, "模型不存在(model_not_found)", ghost.Reason)
	require.Equal(t, 1, ghost.Level)
	require.Greater(t, ghost.RemainingSeconds, int64(23*3600), "首犯永久冷却至少 23 小时")

	flaky, ok := byModel["flaky-model"]
	require.True(t, ok)
	require.False(t, flaky.Permanent, "抖动冷却不该被标成永久")
	require.Equal(t, 7, flaky.ChannelID)
	require.LessOrEqual(t, flaky.RemainingSeconds, int64(15*60), "抖动冷却窗口 15 分钟")
	require.Greater(t, flaky.RemainingSeconds, int64(0))
}

// 已到期的记录不是"当前冷却",不该出现在列表里(否则管理员会去清一个不存在的东西)。
func TestListAutoModelCooldownsOmitsExpiredRows(t *testing.T) {
	setupAutoModelCooldownTestDB(t)

	// 直接写一条已过期的行,绕过 trip 的窗口计算。
	require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
		Group: "default", Model: "stale-model", ChannelId: 4,
		Until: time.Now().Add(-time.Minute).Unix(), Level: 1, Reason: "请求失败",
		UpdatedAt: time.Now().Unix(),
	}))
	LoadAutoModelCooldowns()

	require.Empty(t, ListAutoModelCooldowns(time.Now()))
}

// DeleteAutoModelCooldown 只清掉指定的那一条:同一个模型在别的渠道上的冷却必须留着。
func TestDeleteAutoModelCooldownRemovesOnlyRequestedPair(t *testing.T) {
	setupAutoModelCooldownTestDB(t)

	RecordAutoModelPermanentFailure("default", "ghost-model", 3, "模型不存在")
	RecordAutoModelPermanentFailure("default", "ghost-model", 9, "模型不存在")

	require.NoError(t, DeleteAutoModelCooldown("default", "ghost-model", 3))

	rows := ListAutoModelCooldowns(time.Now())
	require.Len(t, rows, 1, "只该删掉 (ghost-model, 渠道 3)")
	require.Equal(t, 9, rows[0].ChannelID)

	// 选择器必须立刻停止排除被删的那条。
	require.False(t, GetAutoModelCoolingChannelIDs("default", "ghost-model")[3])
	require.True(t, GetAutoModelCoolingChannelIDs("default", "ghost-model")[9])

	// 库里也不能留下。
	stored := cooldownRows(t)
	require.Len(t, stored, 1)
	require.Equal(t, 9, stored[0].ChannelId)
}

// 删除不存在的键不是错误,但也不能顺手清掉别人。
func TestDeleteUnknownAutoModelCooldownKeepsOthers(t *testing.T) {
	setupAutoModelCooldownTestDB(t)

	RecordAutoModelPermanentFailure("default", "ghost-model", 3, "模型不存在")

	require.NoError(t, DeleteAutoModelCooldown("default", "ghost-model", 99))

	rows := ListAutoModelCooldowns(time.Now())
	require.Len(t, rows, 1)
	require.Equal(t, 3, rows[0].ChannelID)
}

// 参数不全时不能误删(空分组/空模型/非法渠道一律拒绝)。
func TestDeleteAutoModelCooldownRejectsIncompleteKey(t *testing.T) {
	setupAutoModelCooldownTestDB(t)

	RecordAutoModelPermanentFailure("default", "ghost-model", 3, "模型不存在")

	for _, tc := range []struct {
		name      string
		group     string
		model     string
		channelID int
	}{
		{"empty-group", "", "ghost-model", 3},
		{"empty-model", "default", "", 3},
		{"zero-channel", "default", "ghost-model", 0},
		{"negative-channel", "default", "ghost-model", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, DeleteAutoModelCooldown(tc.group, tc.model, tc.channelID))
		})
	}

	require.Len(t, ListAutoModelCooldowns(time.Now()), 1, "拒绝的请求不能删掉任何东西")
}