package service

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
)

// auto 候选冷却：把"这个 (分组, 模型, 渠道) 刚才慢或者挂了"记住一段明确的时间，
// 并且落库，重启后依然生效。
//
// 只有一层，粒度是 (分组, 模型, 渠道)：只挡这一个组合。
// 早先还有一层"渠道级"（键里的模型名为空，一并挡掉该渠道上的所有模型），已废弃：
// 一个渠道下 5 个模型，试挂了 2 个不代表剩下 3 个也不行——它们可能又快又稳，
// 整条避让就把它们错过了。代价是某条渠道整体排队时，auto 会把它挂着的模型各试一次
// （每个模型各付一次慢请求）才全部进冷却；换来的是不再"一竿子打翻一船模型"。
// 历史上的 model='' 记录会在载入时清库。
//
// 为什么需要它：内存里的健康分只做短期反应（约 10 分钟线性衰减归零），重启或者
// 空闲一会儿就全没了，于是 auto 又会去挑那个排在最前面、但上游正在排队几十秒的
// 候选，客户端等不到响应就报错。冷却窗口不衰减，只有到期才会被重新尝试；
// 重复犯错时窗口逐级翻倍（15m → 30m → 1h → 2h → 4h → 6h 封顶）。
//
// 判定规则（elapsedMs 是本次请求的总耗时）：
//   - 正常速度的成功 → 清除该组合的冷却（别的模型偶然成功不代表这个组合已经好了）。
//   - 慢（超过 autoModelSlowLatencyMs）的成功或失败 → 该组合进冷却。
//   - 快速失败（例如 401、余额不足）→ 同样只记该组合。
//   - 没有耗时数据 → 不动冷却，避免用猜测覆盖已知状态。

const (
	// autoModelSlowLatencyMs 超过它就算"慢"。流式请求量到的是首字节时间；
	// 非流式量不到首字节，用总耗时兜底（会包含生成长文的时间）。
	autoModelSlowLatencyMs int64 = 20000
	autoModelCooldownBase        = 15 * time.Minute
	autoModelCooldownMax         = 6 * time.Hour
)

// autoModelCooldownState 是一条冷却记录的内存态。
type autoModelCooldownState struct {
	Until     time.Time
	Level     int
	Reason    string
	UpdatedAt time.Time
	// Pending writes live with the cached state so clearing the map also resets
	// recovery state. A pending deletion is a tombstone with a zero Until.
	pending *autoModelCooldownPendingWrite
}

type autoModelCooldownPendingWrite struct {
	group     string
	name      string
	channelID int
	delete    bool
}

var (
	autoModelCooldowns        = types.NewRWMap[string, autoModelCooldownState]()
	autoModelCooldownLoadOnce sync.Once
	// 同一把锁覆盖内存读改写及数据库操作，避免并发升级丢失、写入/清除乱序，
	// 以及重新载入旧快照覆盖刚收到的请求结果。锁顺序始终是本锁 → RWMap。
	autoModelCooldownMutex sync.Mutex
)

// LoadAutoModelCooldowns 从数据库重新载入冷却记录，过期的顺手清库。
// 可重复调用（测试也用它）；生产路径只缓存成功的首次载入。
func LoadAutoModelCooldowns() {
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	if loadAutoModelCooldownsLocked() {
		autoModelCooldownLoadOnce.Do(func() {})
	}
	flushPendingAutoModelCooldownsLocked()
}

// loadAutoModelCooldownsLocked 的调用方必须持有 autoModelCooldownMutex。
// 失败时保留已有内存状态，且不把这次尝试当成成功初始化。
func loadAutoModelCooldownsLocked() bool {
	if model.DB == nil {
		return false
	}
	rows, err := model.GetAutoModelCooldowns()
	if err != nil {
		common.SysError("failed to load auto model cooldowns: " + err.Error())
		return false
	}
	now := time.Now()
	loaded := make(map[string]autoModelCooldownState, len(rows))
	for _, row := range rows {
		if row.Model == "" {
			// 渠道级冷却已废弃（见 applyAutoModelCooldown）：顺手清掉历史残留，
			// 否则旧记录还会继续让 auto 整条渠道避让，把该渠道里健康的模型一起错过。
			// 按主键删除，避免 struct Where 忽略空 Model 而误删同渠道其它模型。
			if err := model.DB.Delete(&row).Error; err != nil {
				common.SysError("failed to purge legacy channel-level auto model cooldown: " + err.Error())
			}
			continue
		}
		until := time.Unix(row.Until, 0)
		if !until.After(now) {
			if err := model.DB.Delete(&row).Error; err != nil {
				common.SysError("failed to purge expired auto model cooldown: " + err.Error())
			}
			continue
		}
		loaded[autoModelHealthKey(row.Group, row.Model, row.ChannelId)] = autoModelCooldownState{
			Until:     until,
			Level:     row.Level,
			Reason:    row.Reason,
			UpdatedAt: time.Unix(row.UpdatedAt, 0),
		}
	}
	// A successful retry must not replace feedback accepted while the database
	// was unavailable with an older database snapshot. Tombstones win as well:
	// a locally recovered model must not be resurrected by a stale stored row.
	for key, state := range autoModelCooldowns.ReadAll() {
		if state.pending != nil {
			loaded[key] = state
		}
	}
	autoModelCooldowns.Clear()
	autoModelCooldowns.AddAll(loaded)
	return true
}

func ensureAutoModelCooldownsLoaded() {
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked()
}

func ensureAutoModelCooldownsLoadedLocked() bool {
	loaded := true
	autoModelCooldownLoadOnce.Do(func() {
		loaded = loadAutoModelCooldownsLocked()
	})
	if !loaded {
		// sync.Once 仅表示成功载入；数据库未就绪或查询失败时允许下一次重试。
		// Do 和重置都在事务锁内，不会与其它调用并发访问 Once。
		autoModelCooldownLoadOnce = sync.Once{}
	}
	flushPendingAutoModelCooldownsLocked()
	return loaded
}

// autoModelCooldownSnapshot 返回同一时刻的独立快照，调用方不持有事务锁。
func autoModelCooldownSnapshot() map[string]autoModelCooldownState {
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked()
	return autoModelCooldowns.ReadAll()
}

// nextAutoModelCooldown 计算下一次冷却：每犯一次错，窗口翻一倍，封顶 autoModelCooldownMax。
func nextAutoModelCooldown(prev autoModelCooldownState, reason string, now time.Time) autoModelCooldownState {
	level := prev.Level + 1
	window := autoModelCooldownBase
	for i := 1; i < level; i++ {
		if window >= autoModelCooldownMax {
			break
		}
		window *= 2
	}
	if window > autoModelCooldownMax {
		window = autoModelCooldownMax
	}
	return autoModelCooldownState{
		Until:     now.Add(window),
		Level:     level,
		Reason:    reason,
		UpdatedAt: now,
	}
}

// applyAutoModelCooldown 依据一次 auto 请求的结果维护冷却（粒度：模型 + 渠道）。
func applyAutoModelCooldown(group, name string, channelID int, success bool, elapsedMs int64) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return
	}
	if success && elapsedMs <= 0 {
		return // 成功但没有耗时数据：不动冷却
	}
	now := time.Now()
	if success && elapsedMs <= autoModelSlowLatencyMs {
		clearAutoModelCooldown(group, name, channelID, now)
		return
	}

	reason := "请求失败"
	if success {
		reason = fmt.Sprintf("响应过慢（%dms）", elapsedMs)
	} else if elapsedMs > autoModelSlowLatencyMs {
		reason = fmt.Sprintf("超时/失败（%dms）", elapsedMs)
	}
	tripAutoModelCooldown(group, name, channelID, reason, now)

	// 只冷却"这个渠道上的这个模型"。
	// 早期版本在这里额外冷却了整条渠道（同上游的其它模型一并避让），但他的判断更准确：
	// 一个渠道下 5 个模型，试挂了 2 个不代表剩下 3 个也不行——它们可能又快又稳，
	// 整条避让就把它们错过了。所以渠道级冷却已移除（历史上的 model='' 记录在载入时清理）。
}

// tripAutoModelCooldown 让一个 (分组, 模型, 渠道) 组合进入（或加重）冷却。
func tripAutoModelCooldown(group, name string, channelID int, reason string, now time.Time) {
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked()

	key := autoModelHealthKey(group, name, channelID)
	prev, _ := autoModelCooldowns.Get(key)
	if prev.pending != nil && prev.pending.delete {
		// A successful request already reset this combination, even if its
		// database deletion has not succeeded yet. This is a fresh failure.
		prev = autoModelCooldownState{}
	}
	state := nextAutoModelCooldown(prev, reason, now)
	state.pending = &autoModelCooldownPendingWrite{group: group, name: name, channelID: channelID}
	autoModelCooldowns.Set(key, state)
	if !persistAutoModelCooldownLocked(key, state) {
		return
	}
	common.SysLog(fmt.Sprintf("auto model: %s（渠道 %d）进入冷却，%s 后再试，等级 %d，原因：%s",
		name, channelID, state.Until.Sub(now).Round(time.Second), state.Level, reason))
}

// clearAutoModelCooldown 正常速度的成功：清掉这个组合的模型级冷却。
func clearAutoModelCooldown(group, name string, channelID int, now time.Time) {
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	loaded := ensureAutoModelCooldownsLoadedLocked()

	key := autoModelHealthKey(group, name, channelID)
	if _, ok := autoModelCooldowns.Get(key); !ok && loaded {
		return
	}
	// Publish recovery immediately, but retain the deletion intent until the
	// database confirms it. If initial loading failed, even an absent cache key
	// may have an old stored cooldown that this successful request must clear.
	state := autoModelCooldownState{
		UpdatedAt: now,
		pending:   &autoModelCooldownPendingWrite{group: group, name: name, channelID: channelID, delete: true},
	}
	autoModelCooldowns.Set(key, state)
	if !persistAutoModelCooldownLocked(key, state) {
		return
	}
	common.SysLog(fmt.Sprintf("auto model: %s（渠道 %d）恢复正常，清除冷却", name, channelID))
}

// persistAutoModelCooldownLocked commits one pending intent. Failures leave it
// in the same map for later synchronous retries; callers hold the transaction lock.
func persistAutoModelCooldownLocked(key string, state autoModelCooldownState) bool {
	pending := state.pending
	if pending == nil {
		return true
	}
	if model.DB == nil {
		return false
	}
	if pending.delete {
		if err := model.DeleteAutoModelCooldown(pending.group, pending.name, pending.channelID); err != nil {
			common.SysError("failed to clear auto model cooldown: " + err.Error())
			return false
		}
		deleteAutoModelCooldownCache(key)
		return true
	}
	row := &model.AutoModelCooldown{
		Group:     pending.group,
		Model:     pending.name,
		ChannelId: pending.channelID,
		Until:     state.Until.Unix(),
		Level:     state.Level,
		Reason:    state.Reason,
		UpdatedAt: state.UpdatedAt.Unix(),
	}
	if err := model.UpsertAutoModelCooldown(row); err != nil {
		common.SysError("failed to save auto model cooldown: " + err.Error())
		return false
	}
	state.pending = nil
	autoModelCooldowns.Set(key, state)
	return true
}

// flushPendingAutoModelCooldownsLocked retries recovery without background jobs.
// Pending metadata is immutable; successful writes replace the cached value.
func flushPendingAutoModelCooldownsLocked() {
	if model.DB == nil {
		return
	}
	for key, state := range autoModelCooldowns.ReadAll() {
		if state.pending != nil {
			persistAutoModelCooldownLocked(key, state)
		}
	}
}

// deleteAutoModelCooldownCache 原子移除一条，不重建或覆盖其它组合。
func deleteAutoModelCooldownCache(key string) {
	autoModelCooldowns.Delete(key)
}

// GetAutoModelCoolingChannelIDs 返回该 (分组, 模型) 当前仍在冷却的渠道集合。
// 返回值是独立快照，可由调用方修改；不包含旧渠道级冷却或已过期记录。
// 本函数仅提供过滤依据，全部渠道冷却时的兜底策略仍由选择器决定。
func GetAutoModelCoolingChannelIDs(group, name string) map[int]bool {
	cooling := make(map[int]bool)
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" {
		return cooling
	}
	snapshot := autoModelCooldownSnapshot()
	now := time.Now()
	prefix := autoModelHealthPrefix(group, name)
	for key, state := range snapshot {
		if !strings.HasPrefix(key, prefix) || !state.Until.After(now) {
			continue
		}
		id, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
		if err == nil && id > 0 {
			cooling[id] = true
		}
	}
	return cooling
}

// autoModelCooldownActive 报告该模型是不是在所有已知渠道上都处于冷却：
// 只要还有一个渠道没在冷却，就算这个模型还能试。
func autoModelCooldownActive(group, name string, channelIDs []int) bool {
	if len(channelIDs) == 0 {
		return false
	}
	snapshot := autoModelCooldownSnapshot()
	return autoModelCooldownActiveInSnapshot(group, name, channelIDs, snapshot, time.Now())
}

func autoModelCooldownActiveInSnapshot(group, name string, channelIDs []int, snapshot map[string]autoModelCooldownState, now time.Time) bool {
	if len(channelIDs) == 0 {
		return false
	}
	for _, id := range channelIDs {
		state, ok := snapshot[autoModelHealthKey(group, name, id)]
		if !ok || !state.Until.After(now) {
			return false
		}
	}
	return true
}

// excludeCoolingCandidates 丢掉所有渠道都在冷却中的候选。
// 如果一个都不剩就原样返回：宁可再试一次，也不要让 auto 直接报"没有可用候选"。
func excludeCoolingCandidates(candidates []string, allCooling map[string]bool) []string {
	kept := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if allCooling[name] {
			continue
		}
		kept = append(kept, name)
	}
	if len(kept) == 0 {
		return candidates
	}
	return kept
}

// excludeCoolingAutoModelCandidates 查询各候选的可用渠道，过滤掉整体都在冷却中的模型。
func excludeCoolingAutoModelCandidates(group string, candidates []string) []string {
	if len(candidates) <= 1 {
		return candidates
	}
	channelIDs, err := model.GetGroupModelChannelIDs(group)
	if err != nil {
		common.SysError("failed to load group model channels for auto cooldown: " + err.Error())
		return candidates
	}
	snapshot := autoModelCooldownSnapshot()
	now := time.Now()
	allCooling := make(map[string]bool, len(candidates))
	dropped := make([]string, 0)
	for _, name := range candidates {
		ids := channelIDs[name]
		if len(ids) == 0 {
			continue
		}
		if autoModelCooldownActiveInSnapshot(group, name, ids, snapshot, now) {
			allCooling[name] = true
			dropped = append(dropped, name)
		}
	}
	if len(dropped) == 0 {
		return candidates
	}
	filtered := excludeCoolingCandidates(candidates, allCooling)
	if len(filtered) < len(candidates) {
		common.SysLog(fmt.Sprintf("auto model: 冷却中跳过候选 %s", strings.Join(dropped, ", ")))
	}
	return filtered
}
