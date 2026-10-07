package service

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupAutoModelCooldownTestDB 按 service 包既有做法临时替换 model.DB（内存 sqlite）。
func setupAutoModelCooldownTestDB(t *testing.T) {
	t.Helper()
	oldDB := model.DB
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared&_pragma=busy_timeout(5000)"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AutoModelCooldown{}))
	require.NoError(t, db.Exec("DELETE FROM auto_model_cooldowns").Error)
	model.DB = db
	autoModelCooldowns.Clear()
	autoModelCooldownLoadOnce = sync.Once{}
	LoadAutoModelCooldowns()
	t.Cleanup(func() {
		db.Exec("DELETE FROM auto_model_cooldowns")
		model.DB = oldDB
		autoModelCooldowns.Clear()
		autoModelCooldownLoadOnce = sync.Once{}
	})
}

func cooldownRows(t *testing.T) []model.AutoModelCooldown {
	t.Helper()
	var rows []model.AutoModelCooldown
	require.NoError(t, model.DB.Find(&rows).Error)
	return rows
}

// TestAutoModelCooldownTripsOnFailureAndSlowSuccess 锁定核心行为：
// 慢事件同时冷却"这个模型"和"整个渠道"；快速失败只算模型自己的问题；
// 正常速度的成功能把模型级冷却清掉。
func TestAutoModelCooldownTripsOnFailureAndSlowSuccess(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	// 慢成功（总耗时 110 秒）→ 模型级 + 渠道级
	applyAutoModelCooldown(group, "slow-model", 1, true, 110000)
	require.Len(t, cooldownRows(t), 2, "慢事件应写入模型级和渠道级两条冷却")
	require.True(t, autoModelCooldownActive(group, "slow-model", []int{1}), "慢模型应进入冷却")
	require.True(t, autoModelCooldownActive(group, "other-model", []int{1}), "同渠道的其它模型也应被连带冷却")
	require.False(t, autoModelCooldownActive(group, "other-model", []int{1, 2}), "还有别的渠道就不该整体剔除")

	// 快速失败（鉴权/余额这类）→ 只记模型级，不连累渠道
	applyAutoModelCooldown(group, "dead-model", 2, false, 0)
	require.Len(t, cooldownRows(t), 3)
	require.True(t, autoModelCooldownActive(group, "dead-model", []int{2}))
	require.False(t, autoModelCooldownActive(group, "other-model", []int{2}), "快速失败不该冷却整个渠道")

	// 正常速度的成功 → 清掉该组合的模型级冷却（渠道级只按到期时间失效）
	applyAutoModelCooldown(group, "slow-model", 1, true, 1200)
	_, ok := autoModelCooldowns.Get(autoModelHealthKey(group, "slow-model", 1))
	require.False(t, ok, "模型级冷却应被清除")
	require.True(t, autoModelCooldownActive(group, "slow-model", []int{1}), "渠道级冷却未到期前，该渠道仍整体避让")
	rows := cooldownRows(t)
	require.Len(t, rows, 2, "渠道级冷却按自己的到期时间失效，不由别的模型成功清除")
	channelLevel := 0
	for _, row := range rows {
		if row.Model == "" {
			channelLevel++
			require.Equal(t, 1, row.ChannelId, "渠道级冷却记录的模型名为空")
		}
	}
	require.Equal(t, 1, channelLevel)
}

// 连续犯错时冷却窗口逐级翻倍，最长 6 小时。
func TestAutoModelCooldownEscalatesAndCaps(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()

	prev := autoModelCooldownState{}
	windows := []time.Duration{}
	for i := 0; i < 8; i++ {
		prev = nextAutoModelCooldown(prev, "请求失败", now)
		windows = append(windows, prev.Until.Sub(now))
	}

	require.Equal(t, 15*time.Minute, windows[0])
	require.Equal(t, 30*time.Minute, windows[1])
	require.Equal(t, time.Hour, windows[2])
	require.Equal(t, 2*time.Hour, windows[3])
	require.Equal(t, 4*time.Hour, windows[4])
	require.Equal(t, autoModelCooldownMax, windows[5])
	require.Equal(t, autoModelCooldownMax, windows[7], "封顶 6 小时")
	require.Equal(t, 8, prev.Level)
}

// 冷却是要跨重启的：内存表清空后必须能从数据库里重新读回来。
func TestAutoModelCooldownSurvivesReload(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	applyAutoModelCooldown(group, "slow-model", 1, true, 110000)
	require.True(t, autoModelCooldownActive(group, "slow-model", []int{1}))

	// 模拟进程重启：内存全丢，重新从库里载入
	autoModelCooldowns.Clear()
	require.False(t, autoModelCooldownActive(group, "slow-model", []int{1}), "清空内存后暂时读不到")
	LoadAutoModelCooldowns()
	require.True(t, autoModelCooldownActive(group, "slow-model", []int{1}), "重启后仍应记得这个模型在冷却")
}

// 过期的冷却不该继续挡路，而且载入时应顺手清库。
func TestAutoModelCooldownExpiry(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
		Group: group, Model: "old-model", ChannelId: 1,
		Until: time.Now().Add(-time.Minute).Unix(), Level: 1, Reason: "请求失败",
		UpdatedAt: time.Now().Add(-time.Hour).Unix(),
	}))
	LoadAutoModelCooldowns()
	require.False(t, autoModelCooldownActive(group, "old-model", []int{1}), "过期冷却应失效")
	require.Empty(t, cooldownRows(t), "过期记录应被清掉")
}

// 候选过滤：只剔除"所有渠道都在冷却"的模型；全部都被剔除时 fail-open。
func TestExcludeCoolingCandidates(t *testing.T) {
	candidates := []string{"model-a", "slow-model", "model-c"}

	filtered := excludeCoolingCandidates(candidates, map[string]bool{"slow-model": true})
	require.Equal(t, []string{"model-a", "model-c"}, filtered)

	require.Equal(t, candidates, excludeCoolingCandidates(candidates, map[string]bool{}), "没有冷却时原样返回")

	allCooling := map[string]bool{"model-a": true, "slow-model": true, "model-c": true}
	require.Equal(t, candidates, excludeCoolingCandidates(candidates, allCooling), "全部冷却时 fail-open")

	require.Empty(t, excludeCoolingCandidates([]string{}, map[string]bool{}))
}
