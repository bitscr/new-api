package model

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 缓存命中不查库;TTL 过期后重新查询;失效钩子立即生效;
// 失败不缓存(见 TestAutoModelRoutingTargetsDatabaseFailure)。
func TestAutoModelRoutingTargetsCacheBehavior(t *testing.T) {
	db := setupAutoModelRoutingSnapshotTestDB(t)
	common.MemoryCacheEnabled = true

	priority, weight := int64(5), uint(2)
	require.NoError(t, db.Create(&Channel{
		Id: 1, Key: "a", Status: common.ChannelStatusEnabled, Group: "g", Models: "m",
		Priority: &priority, Weight: &weight,
	}).Error)
	require.NoError(t, db.Create(&Ability{Group: "g", Model: "m", ChannelId: 1, Enabled: true}).Error)

	queries := 0
	const callbackName = "test:auto_model_routing_cache_query_count"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		queries++
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callbackName)) })

	// 1. 首次查询 → 命中一次库;随后的调用全部走缓存。
	first, err := GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Len(t, first["m"], 1)
	require.Equal(t, 1, queries)

	for i := 0; i < 3; i++ {
		again, err := GetAutoModelRoutingTargets("g")
		require.NoError(t, err)
		require.Equal(t, first, again)
	}
	require.Equal(t, 1, queries, "TTL 内重复调用不得再次查库")

	// 2. 数据变更但未失效、未过期:仍返回缓存快照。
	require.NoError(t, db.Create(&Channel{
		Id: 2, Key: "b", Status: common.ChannelStatusEnabled, Group: "g", Models: "m",
	}).Error)
	require.NoError(t, db.Create(&Ability{Group: "g", Model: "m", ChannelId: 2, Enabled: true}).Error)
	stale, err := GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Len(t, stale["m"], 1, "TTL 窗口内允许陈旧,超过再由 TTL 兜底")
	require.Equal(t, 1, queries)

	// 3. 失效钩子:立即反映新渠道。
	InvalidateAutoModelRoutingTargetsCache()
	fresh, err := GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Len(t, fresh["m"], 2, "失效后必须重新查询并看到新渠道")
	require.Equal(t, 2, queries)

	// 4. TTL 过期:改短 TTL 验证过期后自动刷新,无需显式失效。
	oldTTL := autoModelRoutingTargetsCacheTTL
	autoModelRoutingTargetsCacheTTL = 30 * time.Millisecond
	t.Cleanup(func() { autoModelRoutingTargetsCacheTTL = oldTTL })

	require.NoError(t, db.Create(&Channel{
		Id: 3, Key: "c", Status: common.ChannelStatusEnabled, Group: "g", Models: "m",
	}).Error)
	require.NoError(t, db.Create(&Ability{Group: "g", Model: "m", ChannelId: 3, Enabled: true}).Error)
	before := queries
	_, err = GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Equal(t, before, queries, "TTL 未到,不应查询")

	time.Sleep(60 * time.Millisecond)
	expired, err := GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Len(t, expired["m"], 3, "TTL 过期后必须重新查询")
	require.Equal(t, before+1, queries)
}

// 禁用渠道(状态翻转)经 UpdateAbilityStatus 失效缓存,立即从路由目标中消失。
func TestAutoModelRoutingTargetsCacheInvalidatedByStatusFlip(t *testing.T) {
	db := setupAutoModelRoutingSnapshotTestDB(t)
	common.MemoryCacheEnabled = true

	require.NoError(t, db.Create(&Channel{
		Id: 1, Key: "a", Status: common.ChannelStatusEnabled, Group: "g", Models: "m",
	}).Error)
	require.NoError(t, db.Create(&Ability{Group: "g", Model: "m", ChannelId: 1, Enabled: true}).Error)

	first, err := GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Len(t, first["m"], 1)

	// 渠道被禁用(与 auto 自动禁用同一条路径:update status + ability)。
	require.NoError(t, db.Model(&Channel{}).Where("id = ?", 1).
		Update("status", common.ChannelStatusManuallyDisabled).Error)
	require.NoError(t, UpdateAbilityStatus(1, false))

	after, err := GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Empty(t, after["m"], "禁用后的渠道必须立刻退出路由目标")
}
