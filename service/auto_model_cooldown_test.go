package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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

// TestAutoModelCooldownIsPerModel 锁定当前语义：冷却是"该渠道上的该模型"的事。
// 一个渠道下试挂了 2 个模型，不代表剩下 3 个也不行，所以不再冷却整条渠道
// （早期版本会额外写一条 model=” 的渠道级记录，让同上游的其它模型一起避让）。
func TestLateFailureCannotReCoolRecoveredPair(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group, modelName, channelID := "cooldown-recovery-race", "model-a", 41
	now := time.Now()
	tripAutoModelCooldown(group, modelName, channelID, "seed", now.Add(-time.Minute))
	_, active := GetAutoModelCooldown(group, modelName, channelID, now)
	require.True(t, active)

	// A fast success observed now recovers the pair and confirms the DB delete.
	recordAutoModelOutcome(group, modelName, channelID, true, false, true, 100, 100, false, "", now)
	_, active = GetAutoModelCooldown(group, modelName, channelID, now)
	require.False(t, active)
	autoModelCooldownMutex.Lock()
	recoveredAt, ok := autoModelCooldownRecoveries[autoModelHealthKey(group, modelName, channelID)]
	autoModelCooldownMutex.Unlock()
	require.True(t, ok, "the recovery barrier must remain after a successful delete")
	require.False(t, recoveredAt.IsZero())

	// The failure was observed before recovery and published late.
	recordAutoModelOutcome(group, modelName, channelID, false, false, false, 0, 500, false, "late failure", now.Add(-time.Minute))
	_, active = GetAutoModelCooldown(group, modelName, channelID, now)
	require.False(t, active, "a pre-recovery failure must not re-cool the route")

	// A genuinely newer failure still creates a fresh cooldown.
	recordAutoModelOutcome(group, modelName, channelID, false, false, false, 0, 500, false, "new failure", now.Add(time.Second))
	cooldown, active := GetAutoModelCooldown(group, modelName, channelID, now.Add(time.Second))
	require.True(t, active)
	require.Equal(t, 1, cooldown.Level, "the ladder restarts after recovery")
}

func TestOutOfOrderRecoveryDoesNotMoveBarrierBackwards(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group, modelName, channelID := "cooldown-recovery-order", "model-a", 42
	now := time.Now()
	t1, t2, t3 := now.Add(-2*time.Minute), now.Add(-time.Minute), now

	// Real order: t1 success -> t2 failure -> t3 success.
	// Publication order: t3 success -> t1 success -> t2 failure.
	require.NoError(t, clearAutoModelCooldownWithResult(group, modelName, channelID, t3))
	require.NoError(t, clearAutoModelCooldownWithResult(group, modelName, channelID, t1))
	tripAutoModelCooldown(group, modelName, channelID, "late failure", t2)
	_, active := GetAutoModelCooldown(group, modelName, channelID, t3)
	require.False(t, active, "an old success must not move the t3 barrier back to t1")

	t4 := now.Add(time.Second)
	tripAutoModelCooldown(group, modelName, channelID, "genuine new failure", t4)
	cooldown, active := GetAutoModelCooldown(group, modelName, channelID, t4)
	require.True(t, active)
	require.Equal(t, 1, cooldown.Level, "a genuinely new failure starts at the first tier")
}

func TestCanceledCooldownBudgetLeavesPendingWriteAndReleasesLock(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group, modelName, channelID := "cooldown-budget", "model-a", 43
	now := time.Now()
	tripAutoModelCooldown(group, modelName, channelID, "seed", now)
	require.Len(t, cooldownRows(t), 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := clearAutoModelCooldownWithResultContext(ctx, model.DB, group, modelName, channelID, now.Add(time.Second))
	require.Error(t, err, "an exhausted budget must fail instead of waiting for SQL")

	key := autoModelHealthKey(group, modelName, channelID)
	autoModelCooldownMutex.Lock()
	state, ok := autoModelCooldowns.Get(key)
	autoModelCooldownMutex.Unlock()
	require.True(t, ok)
	require.NotNil(t, state.pending)
	require.True(t, state.pending.delete)

	require.NoError(t, RetryPendingAutoModelCooldownWrites(context.Background(), model.DB))
	require.Empty(t, cooldownRows(t), "the background retry finishes after the budget is restored")
}

func TestAutoModelCooldownIsPerModel(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	// 慢失败（总耗时 110 秒）→ 只冷却这个模型
	applyAutoModelCooldown(group, "slow-model", 1, false, 110000)
	rows := cooldownRows(t)
	require.Len(t, rows, 1, "只该有一条模型级冷却")
	require.Equal(t, "slow-model", rows[0].Model)
	require.True(t, autoModelCooldownActive(group, "slow-model", []int{1}))
	require.False(t, autoModelCooldownActive(group, "other-model", []int{1}),
		"同渠道的其它模型不该被连累——它可能又快又稳")

	// 同渠道的第二个模型也失败 → 各记一条，渠道级依旧不出现
	applyAutoModelCooldown(group, "second-slow-model", 1, false, 110000)
	rows = cooldownRows(t)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.NotEmpty(t, row.Model, "不该再出现渠道级（model 为空）记录")
	}

	// 快速失败（鉴权/余额这类）→ 同样只记模型级
	applyAutoModelCooldown(group, "dead-model", 2, false, 0)
	require.Len(t, cooldownRows(t), 3)

	// 正常速度的成功 → 清掉该组合的模型级冷却，其它模型不受影响
	applyAutoModelCooldown(group, "slow-model", 1, true, 1200)
	_, ok := autoModelCooldowns.Get(autoModelHealthKey(group, "slow-model", 1))
	require.False(t, ok, "模型级冷却应被清除")
	require.True(t, autoModelCooldownActive(group, "second-slow-model", []int{1}))
}

// 慢成功只是延迟证据，不是失败：它必须清除冷却，也不能继续标记"待恢复"。
// 否则唯一能用的上游会因为首 token 慢被停用，请求只能去抽挂死的路由。
func TestAutoModelSlowSuccessIsRecoveryNotFailure(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group, modelName, channelID := "slow-success", "slow-model", 7
	key := autoModelHealthKey(group, modelName, channelID)

	tripAutoModelCooldown(group, modelName, channelID, "seed", time.Now().Add(-time.Minute))
	require.True(t, autoModelCooldownActive(group, modelName, []int{channelID}))

	autoModelScoreMutex.Lock()
	autoModelHealth.Delete(key)
	autoModelScoreMutex.Unlock()
	t.Cleanup(func() {
		autoModelScoreMutex.Lock()
		autoModelHealth.Delete(key)
		autoModelScoreMutex.Unlock()
	})

	recordAutoModelOutcome(group, modelName, channelID, true, true, false, 0, 120000, false, "", time.Now())

	require.False(t, autoModelCooldownActive(group, modelName, []int{channelID}),
		"慢成功必须清除冷却，而不是新建冷却")
	autoModelScoreMutex.Lock()
	outcome, ok := autoModelHealth.Get(key)
	autoModelScoreMutex.Unlock()
	require.True(t, ok)
	require.False(t, outcome.PendingRecovery, "慢成功不能把路由留在待恢复状态")
}

func TestAutoModelSuccessWithoutTimingClearsFailedRecovery(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group, name, channelID := "unknown-timing-recovery", "slow", 45
	key := autoModelHealthKey(group, name, channelID)
	now := time.Now()
	t.Cleanup(func() { autoModelHealth.Delete(key) })
	recordAutoModelOutcome(group, name, channelID, false, true, false, 0, 1000, false, "failed", now.Add(-time.Second))
	outcome, ok := autoModelHealth.Get(key)
	require.True(t, ok)
	require.True(t, outcome.PendingRecovery)
	require.True(t, autoModelCooldownActive(group, name, []int{channelID}))
	recordAutoModelOutcome(group, name, channelID, true, true, false, 0, 0, false, "", now)
	outcome, ok = autoModelHealth.Get(key)
	require.True(t, ok)
	require.False(t, outcome.PendingRecovery)
	require.Zero(t, outcome.StreamLatencySamples, "unknown latency must stay unknown")
	require.Zero(t, outcome.StreamLatencyMS)
	require.False(t, autoModelCooldownActive(group, name, []int{channelID}))
	require.Empty(t, cooldownRows(t))
	// Older success without timing still cannot clear a newer failure.
	recordAutoModelOutcome(group, name, channelID, false, true, false, 0, 1000, false, "new failure", now.Add(time.Second))
	recordAutoModelOutcome(group, name, channelID, true, true, false, 0, 0, false, "", now)
	outcome, _ = autoModelHealth.Get(key)
	require.True(t, outcome.PendingRecovery)
	require.True(t, autoModelCooldownActive(group, name, []int{channelID}))
}

func TestAutoModelCooldownKeepsFailureReason(t *testing.T) {
	setupAutoModelCooldownTestDB(t)

	applyAutoModelCooldownWithReason("default", "dead-model", 9, false, 120, "insufficient_balance(HTTP 402)")

	rows := cooldownRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, "insufficient_balance(HTTP 402)", rows[0].Reason)
}

// TestLegacyChannelLevelCooldownIsPurged 历史残留的渠道级记录不该继续生效：
// 载入时清库，也不再参与候选过滤（否则又变成"整条渠道被避让"）。
func TestLegacyChannelLevelCooldownIsPurged(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"

	require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
		Group: group, Model: "", ChannelId: 1,
		Until: time.Now().Add(time.Hour).Unix(), Level: 5, Reason: "超时/失败（125343ms）",
		UpdatedAt: time.Now().Unix(),
	}))
	LoadAutoModelCooldowns()

	require.Empty(t, cooldownRows(t), "历史渠道级记录应在载入时被清掉")
	require.False(t, autoModelCooldownActive(group, "any-model", []int{1}), "渠道本身不再被整体避让")
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

	applyAutoModelCooldown(group, "slow-model", 1, false, 110000)
	require.True(t, autoModelCooldownActive(group, "slow-model", []int{1}))

	// 模拟进程重启：内存全丢，重新从库里载入
	autoModelCooldowns.Clear()
	require.False(t, autoModelCooldownActive(group, "slow-model", []int{1}), "清空内存后暂时读不到")
	LoadAutoModelCooldowns()
	require.True(t, autoModelCooldownActive(group, "slow-model", []int{1}), "重启后仍应记得这个模型在冷却")
}

// 过期冷却不再挡路，但保留失败记忆与阶梯，避免重启后从零反复撞错。
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
	require.Len(t, cooldownRows(t), 1, "到期只解除隔离，不应清除失败记忆")
	state, ok := autoModelCooldowns.Get(autoModelHealthKey(group, "old-model", 1))
	require.True(t, ok)
	require.Equal(t, 1, state.Level)
	view := autoModelRouteEvidenceView(autoModelOutcome{}, state, autoModelRequestNonStream, time.Now(), operation_setting.AutoModelRoutingSettingsSnapshot())
	require.True(t, view.PendingRecovery, "重启后评分不存在也不能把失败组合变成未观察候选")
	applyAutoModelCooldown(group, "old-model", 1, false, 100)
	state, ok = autoModelCooldowns.Get(autoModelHealthKey(group, "old-model", 1))
	require.True(t, ok)
	require.Equal(t, 2, state.Level, "重启后的再次失败应继续升级")
}

// GetAutoModelCooldown 暴露给报错日志读取当前冷却等级。
func TestGetAutoModelCooldown(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	group := "default"
	now := time.Now()

	// 抖动冷却:失败一次 → level 1
	applyAutoModelCooldown(group, "m", 1, false, 100)
	info, ok := GetAutoModelCooldown(group, "m", 1, now)
	require.True(t, ok)
	require.Equal(t, 1, info.Level)
	require.Greater(t, info.RemainingSeconds, int64(0))

	// 再失败一次 → level 2
	applyAutoModelCooldown(group, "m", 1, false, 100)
	info, ok = GetAutoModelCooldown(group, "m", 1, now)
	require.True(t, ok)
	require.Equal(t, 2, info.Level)

	// 确定性失败不再产生冷却:命中关键词即直接摘除组合,没有任何记录可查。
	// 这里只验证不存在的组合。
	_, ok = GetAutoModelCooldown(group, "nope", 9, now)
	require.False(t, ok)
}

// 候选过滤：严格剔除"所有渠道都在冷却"的模型；全部被剔除时保持空。
func TestExcludeCoolingCandidates(t *testing.T) {
	candidates := []string{"model-a", "slow-model", "model-c"}

	filtered := excludeCoolingCandidates(candidates, map[string]bool{"slow-model": true})
	require.Equal(t, []string{"model-a", "model-c"}, filtered)

	require.Equal(t, candidates, excludeCoolingCandidates(candidates, map[string]bool{}), "没有冷却时原样返回")

	allCooling := map[string]bool{"model-a": true, "slow-model": true, "model-c": true}
	require.Empty(t, excludeCoolingCandidates(candidates, allCooling), "全部冷却时不允许 fail-open")
	require.Empty(t, excludeCoolingCandidates([]string{"slow-model"}, allCooling), "单候选也必须严格冷却")

	require.Empty(t, excludeCoolingCandidates([]string{}, map[string]bool{}))
}

// 长挂起被取消的尝试要留下冷却证据:派发了却 20 秒+零响应的上游会一直占住
// 计划头部,不记的话每个客户端超时都死在同一个组合上。短取消(<20s)仍不记录。
func TestRecordAutoModelAbandonedAttempt(t *testing.T) {
	setupAutoModelCooldownTestDB(t)

	// 短取消:可能是客户端主动放弃,不算上游的证据
	RecordAutoModelAbandonedAttempt("default", "m", 1, 3000)
	require.Empty(t, ListAutoModelCooldowns(time.Now()))

	// 长挂起:记一次抖动冷却
	RecordAutoModelAbandonedAttempt("default", "m", 1, 120000)
	rows := ListAutoModelCooldowns(time.Now())
	require.Len(t, rows, 1)
	require.Equal(t, 1, rows[0].Level)
	require.Contains(t, rows[0].Reason, "零响应", "冷却原因要说明这是长挂起")
	require.Greater(t, rows[0].RemainingSeconds, int64(0))
}
