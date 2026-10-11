package service

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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
	// autoModelSlowLatencyMs 超过它就算"慢"。流式请求量到的是首字节时间;
	// 非流式量不到首字节,用总耗时兜底(会包含生成长文的时间)。
	autoModelSlowLatencyMs int64 = 20000
	autoModelCooldownBase        = 15 * time.Minute
	autoModelCooldownMax         = 6 * time.Hour
)

// autoModelPermanentCooldownBase / Max 是"确定性失败"的阶梯:模型不存在、无权、
// 不支持这类错误重试也不会自愈,所以第一次就冷却一整天,重复犯翻倍、封顶 30 天。
// 抖动失败(限流、超时、5xx)仍走 15m/6h,不受影响。
func autoModelPermanentCooldownBase() time.Duration {
	return operation_setting.AutoModelPermanentCooldownBase()
}

func autoModelPermanentCooldownMax() time.Duration {
	return operation_setting.AutoModelPermanentCooldownMax()
}

// autoModelCooldownState 是一条冷却记录的内存态。
type autoModelCooldownState struct {
	Until     time.Time
	Level     int
	Reason    string
	UpdatedAt time.Time
	// Permanent 表示这条冷却来自确定性失败,用的是长阶梯。
	Permanent bool
	// Pending writes live with the cached state so clearing the map also resets
	// recovery state. A pending deletion is a tombstone with a zero Until.
	pending *autoModelCooldownPendingWrite
}

type autoModelCooldownPendingWrite struct {
	group     string
	name      string
	channelID int
	delete    bool
	permanent bool
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
			Permanent: row.Permanent,
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

// nextAutoModelCooldown 计算下一次冷却:每犯一次错,窗口翻一倍,封顶 autoModelCooldownMax。
func nextAutoModelCooldown(prev autoModelCooldownState, reason string, now time.Time) autoModelCooldownState {
	return nextAutoModelCooldownIn(prev, reason, now, autoModelCooldownBase, autoModelCooldownMax)
}

// nextAutoModelPermanentCooldown 是确定性失败的阶梯:首犯 24h,之后翻倍、封顶 30d。
func nextAutoModelPermanentCooldown(prev autoModelCooldownState, reason string, now time.Time) autoModelCooldownState {
	state := nextAutoModelCooldownIn(prev, reason, now, autoModelPermanentCooldownBase(), autoModelPermanentCooldownMax())
	state.Permanent = true
	return state
}

// nextAutoModelCooldownIn 两条阶梯共用同一套计数,只有窗口参数不同。
func nextAutoModelCooldownIn(prev autoModelCooldownState, reason string, now time.Time, base, maximum time.Duration) autoModelCooldownState {
	level := prev.Level + 1
	window := base
	for i := 1; i < level; i++ {
		if window >= maximum {
			break
		}
		window *= 2
	}
	if window > maximum {
		window = maximum
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

// tripAutoModelCooldown 让一个 (分组, 模型, 渠道) 组合进入(或加重)冷却。
func tripAutoModelCooldown(group, name string, channelID int, reason string, now time.Time) {
	tripAutoModelCooldownWith(group, name, channelID, reason, now, false)
}

// RecordAutoModelPermanentFailure 记一次确定性失败(模型不存在/无权/不支持)。
// 与抖动失败共用同一条等级计数,但窗口走 24h 起步的长阶梯,且标志位落库。
func RecordAutoModelPermanentFailure(group, name string, channelID int, reason string) {
	tripAutoModelCooldownWith(strings.TrimSpace(group), strings.TrimSpace(name), channelID,
		strings.TrimSpace(reason), time.Now(), true)
}

func tripAutoModelCooldownWith(group, name string, channelID int, reason string, now time.Time, permanent bool) {
	if group == "" || name == "" || channelID <= 0 {
		return
	}
	if reason == "" {
		reason = "请求失败"
	}
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
	if permanent {
		state = nextAutoModelPermanentCooldown(prev, reason, now)
	}
	state.pending = &autoModelCooldownPendingWrite{group: group, name: name, channelID: channelID, permanent: state.Permanent}
	autoModelCooldowns.Set(key, state)
	if !persistAutoModelCooldownLocked(key, state) {
		return
	}
	if state.Permanent {
		common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)进入永久级冷却,%s 后再试,等级 %d,原因:%s",
			name, channelID, state.Until.Sub(now).Round(time.Second), state.Level, reason))
		return
	}
	common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)进入冷却,%s 后再试,等级 %d,原因:%s",
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
		Permanent: state.Permanent,
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

// deleteAutoModelCooldownCache 原子移除一条,不重建或覆盖其它组合。
func deleteAutoModelCooldownCache(key string) {
	autoModelCooldowns.Delete(key)
}

// AutoModelCooldownInfo 是给管理员看的一条当前生效的冷却。
// 这是只读视图:不含请求内容、凭据或任何上游信息。
type AutoModelCooldownInfo struct {
	Group            string `json:"group"`
	Model            string `json:"model"`
	ChannelID        int    `json:"channel_id"`
	Level            int    `json:"level"`
	Reason           string `json:"reason"`
	Permanent        bool   `json:"permanent"`
	Until            int64  `json:"until"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	UpdatedAt        int64  `json:"updated_at"`
}

// ListAutoModelCooldowns 返回当前仍在冷却的组合,按到期时间从近到远排序。
//
// 为什么要它:冷却阶梯是自我强化的(24h 起、翻倍、封顶 30 天),而清除条件是
// "一次 20 秒内的快速成功"——冷却中的组合根本不会被路由,所以它只能等窗口自然到期。
// 没有这个入口,管理员在故障时看不到 auto 挡住了谁、为什么、还要多久,
// 只能手改数据库。共享同一把读锁,不改动任何状态。
func ListAutoModelCooldowns(now time.Time) []AutoModelCooldownInfo {
	snapshot := autoModelCooldownSnapshot()
	rows := make([]AutoModelCooldownInfo, 0, len(snapshot))
	for key, state := range snapshot {
		if !state.Until.After(now) {
			continue
		}
		group, name, channelID, ok := parseAutoModelHealthKey(key)
		if !ok {
			continue
		}
		rows = append(rows, AutoModelCooldownInfo{
			Group:            group,
			Model:            name,
			ChannelID:        channelID,
			Level:            state.Level,
			Reason:           state.Reason,
			Permanent:        state.Permanent,
			Until:            state.Until.Unix(),
			RemainingSeconds: int64(state.Until.Sub(now).Seconds()),
			UpdatedAt:        state.UpdatedAt.Unix(),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Until != rows[j].Until {
			return rows[i].Until < rows[j].Until
		}
		// 同一时刻必须给出稳定顺序,否则列表在两次刷新间会跳动。
		if rows[i].Group != rows[j].Group {
			return rows[i].Group < rows[j].Group
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].ChannelID < rows[j].ChannelID
	})
	return rows
}

// DeleteAutoModelCooldown 手工解除一条冷却(管理员操作)。
// 只删指定组合;参数不全时拒绝,避免误删整片冷却。
func DeleteAutoModelCooldown(group, name string, channelID int) error {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return fmt.Errorf("无效的冷却标识:分组/模型/渠道都必须给出")
	}
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked()

	key := autoModelHealthKey(group, name, channelID)
	// 先落库,成功后再撤内存:反过来的话,库写失败会让内存以为已恢复。
	if model.DB != nil {
		if err := model.DeleteAutoModelCooldown(group, name, channelID); err != nil {
			return err
		}
	}
	deleteAutoModelCooldownCache(key)
	common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)的冷却被管理员手工清除", name, channelID))
	return nil
}

// parseAutoModelHealthKey 拆回 autoModelHealthKey 的组成部分。
func parseAutoModelHealthKey(key string) (group, name string, channelID int, ok bool) {
	parts := strings.Split(key, "\x00")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", 0, false
	}
	id, err := strconv.Atoi(parts[2])
	if err != nil || id <= 0 {
		return "", "", 0, false
	}
	return parts[0], parts[1], id, true
}

// GetAutoModelCoolingChannelIDs 返回该 (分组, 模型) 当前仍在冷却的渠道集合。
// 返回值是独立快照，可由调用方修改；不包含旧渠道级冷却或已过期记录。
// 选择器必须严格排除此集合；即使全部渠道冷却，也不能兜底重新尝试。
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

// excludeCoolingCandidates 严格丢掉所有渠道都在冷却中的候选。
// 全部候选都在冷却时返回空；冷却到期前不允许兜底重试。
func excludeCoolingCandidates(candidates []string, allCooling map[string]bool) []string {
	kept := make([]string, 0, len(candidates))
	for _, name := range candidates {
		if allCooling[name] {
			continue
		}
		kept = append(kept, name)
	}
	return kept
}

// excludeCoolingAutoModelCandidates 只保留至少有一条未冷却启用路由的模型。
// 单候选同样检查；全部冷却或路由快照不可用时不恢复任何候选。
func excludeCoolingAutoModelCandidates(group string, candidates []string) []string {
	if len(candidates) == 0 {
		return candidates
	}
	targets, err := model.GetAutoModelRoutingTargets(group)
	if err != nil {
		common.SysError("failed to load group model channels for auto cooldown: " + err.Error())
		return nil
	}
	snapshot := autoModelCooldownSnapshot()
	now := time.Now()
	unavailable := make(map[string]bool, len(candidates))
	dropped := make([]string, 0)
	for _, name := range candidates {
		ids := make([]int, 0, len(targets[name]))
		for _, target := range targets[name] {
			ids = append(ids, target.ChannelID)
		}
		if len(ids) == 0 || autoModelCooldownActiveInSnapshot(group, name, ids, snapshot, now) {
			unavailable[name] = true
			dropped = append(dropped, name)
		}
	}
	if len(dropped) == 0 {
		return candidates
	}
	common.SysLog(fmt.Sprintf("auto model: 冷却中或无可用路由，跳过候选 %s", strings.Join(dropped, ", ")))
	return excludeCoolingCandidates(candidates, unavailable)
}
