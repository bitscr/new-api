package service

import (
	"errors"
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
	// autoModelCooldownMax 是封顶的兜底常量。实际运行值取
	// operation_setting.AutoModelCooldownMax()(面板可改),这里只是没配置时的下限。
	autoModelCooldownMax = 6 * time.Hour
	// autoModelCooldownSecondTier 是阶梯第二档(首档翻倍)。评分衰减窗口必须
	// 严格大于它,否则第 2 次失败解冻时扣分正好归零(见测试与配置校验)。
	autoModelCooldownSecondTier = 2 * autoModelCooldownBase
)

// AutoModelCooldownBaseForValidation 暴露抖动冷却的首档时长,供配置校验使用。
//
// 为什么需要它:评分衰减窗口必须长于首档冷却,否则第 1 次失败解冻时扣分几乎
// 已归零;而窗口等于第 2 档(30 分钟)时,第 2 次失败解冻的瞬间分数正好回到
// 中立,冷却彻底白做。校验要用同一个来源,不能在校验处再写一个 15 分钟常量,
// 否则以后调冷却阶梯时会悄悄失配。
func AutoModelCooldownBaseForValidation() time.Duration {
	return autoModelCooldownBase
}

// AutoModelCooldownSecondTierForValidation 暴露抖动冷却的第二档时长(首档翻倍)。
//
// 为什么单看首档不够:衰减窗口若正好等于第二档(30 分钟),第 2 次失败解冻的
// 那一刻分数精确回到中立的 0.5——惩罚"刚好"作废。所以真正的不变式是
// "窗口 > 第二档",而不是"窗口 > 首档"。
func AutoModelCooldownSecondTierForValidation() time.Duration {
	return autoModelCooldownSecondTier
}

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

// nextAutoModelCooldown 计算下一次冷却:每犯一次错,窗口翻一倍,封顶取面板配置。
func nextAutoModelCooldown(prev autoModelCooldownState, reason string, now time.Time) autoModelCooldownState {
	// 封顶可由面板改(operation_setting)。取不到或不合法时退回常量兜底,
	// 且必须 >= 首档,否则阶梯会一开始就"封"到比首档还短。
	maximum := operation_setting.AutoModelCooldownMax()
	if maximum < autoModelCooldownBase {
		maximum = autoModelCooldownMax
	}
	return nextAutoModelCooldownIn(prev, reason, now, autoModelCooldownBase, maximum)
}

// nextAutoModelPermanentCooldown 是确定性失败的阶梯,只有两档:
//   - 首犯:冷却 autoModelPermanentCooldownBase()(默认 24 小时);
//   - 再犯:不再延长冷却,而是直接把该 (分组, 模型, 渠道) 的模型注册删掉
//     (见 RetireAutoModelCombination)。
//
// 为什么不再翻倍到 30 天:确定性失败(模型不存在/无权)重试永远不会好。
// 挂一个"30 天后还会回来试"的窗口,等于给自己埋一个到期就复发的问题;
// 而两档处理能让它在第二次就彻底退出候选池,不用等。
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
//
// 两档处理:
//   - 首犯(prev.Level == 0):冷却 autoModelPermanentCooldownBase()(默认 24 小时);
//   - 再犯(prev.Level >= 1):不再延长冷却,直接摘除该 (分组, 模型, 渠道)
//     组合,让它永久退出候选池。
//
// 第二次之后没必要再挂冷却:一个明确"模型在这条渠道上不存在/无权"的组合,
// 24 小时后再试一次就已经足够宽容了,继续失败只能说明它就是不该被选。
func RecordAutoModelPermanentFailure(group, name string, channelID int, reason string) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	reason = strings.TrimSpace(reason)
	if group == "" || name == "" || channelID <= 0 {
		return
	}

	// 先看这个组合是否已经吃过一次确定性失败。要在加锁前读,避免摘除流程
	// 里去拿 model.DB 时与冷却锁互相等待。
	if autoModelPermanentStrikeCount(group, name, channelID) >= 1 {
		if err := RetireAutoModelCombination(group, name, channelID, reason); err != nil {
			common.SysError(fmt.Sprintf("auto model: retire %s(渠道 %d) failed: %s", name, channelID, err.Error()))
			// 摘除失败就退回冷却,至少别让它在候选池里裸奔。
			tripAutoModelCooldownWith(group, name, channelID, reason, time.Now(), true)
		}
		return
	}

	tripAutoModelCooldownWith(group, name, channelID, reason, time.Now(), true)
}

// autoModelPermanentStrikeCount 返回该组合已经记录过几次确定性失败。
// 只读:不加写锁副作用,拿到的是一致快照。
func autoModelPermanentStrikeCount(group, name string, channelID int) int {
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked()

	state, ok := autoModelCooldowns.Get(autoModelHealthKey(group, name, channelID))
	if !ok {
		return 0
	}
	if !state.Permanent {
		return 0
	}
	// pending.delete 是"已被成功请求清掉"的墓碑,不算已经吃过失败。
	if state.pending != nil && state.pending.delete {
		return 0
	}
	return state.Level
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
// RetireAutoModelCombination 把一个"确定性失败且已经犯过一次"的 (分组, 模型, 渠道)
// 组合从候选池里彻底摘掉:从渠道声明的 models 里移除该模型,并同步重建 abilities。
//
// 这是"第二次直接删除"——不删除整条渠道(渠道上还有别的模型,凭据与配置都是复用的),
// 只摘掉这个不可用的模型。摘除后:
//   - 该模型不再出现在渠道的 models 里,auto 路由不会再挑中它;
//   - abilities 表同步失效,显式请求也会立刻得到"无可用渠道",不再打到上游;
//   - 冷却记录同时清除(它已不在候选池里,留着没有意义)。
//
// 幂等:模型已经不在 models 里时直接返回 nil,不重复写库。
func RetireAutoModelCombination(group, name string, channelID int, reason string) error {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return nil
	}
	if model.DB == nil {
		return errors.New("database not initialized")
	}

	channel, err := model.GetChannelById(channelID, false)
	if err != nil || channel == nil {
		return fmt.Errorf("load channel %d failed: %w", channelID, err)
	}

	remaining := make([]string, 0, len(channel.GetModels()))
	removed := false
	for _, m := range channel.GetModels() {
		if strings.TrimSpace(m) == name {
			removed = true
			continue
		}
		remaining = append(remaining, m)
	}
	if !removed {
		// 已经摘过了:不做任何写入,避免并发请求变成重复更新。
		common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)已不在渠道模型列表中,跳过摘除", name, channelID))
		return nil
	}
	channel.Models = strings.Join(remaining, ",")
	if err := channel.Update(); err != nil {
		return fmt.Errorf("update channel %d models failed: %w", channelID, err)
	}

	// 摘掉的是最后一个模型时,这条渠道已经没有可服务的模型了。留着它只会:
	// 每次选通道都白跑一趟(没有可用模型直接失败),面板上也看不出它的状态。
	// 所以自动禁用它,把状态显式标出来,让禁用原因可查。
	if len(remaining) == 0 {
		autoDisableModelEmptiedChannel(channel, name, reason)
	}

	// 摘除后冷却没有意义;清除以免残留记录干扰面板显示。
	clearAutoModelCooldown(group, name, channelID, time.Now())
	common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)确定性失败已达上限,已从渠道模型列表中删除(原因:%s)",
		name, channelID, reason))
	return nil
}

// autoDisableModelEmptiedChannel 在渠道被摘掉最后一个模型后,把它标记为自动禁用。
//
// 为什么不是删除:删除不可逆,而且这个渠道的凭据、代理、分组配置都还在,
// 上游恢复或补新模型后需要能一键启用。禁用保留全部配置、只是不再参与选路,
// 状态里带原因,面板上能直接看出"为什么它不干活了"。
func autoDisableModelEmptiedChannel(channel *model.Channel, removedModel, reason string) {
	if channel == nil || channel.Id <= 0 {
		return
	}
	disableReason := fmt.Sprintf("模型 %s 确定性失败已被移除,渠道已无可用模型", removedModel)
	if strings.TrimSpace(reason) != "" {
		disableReason = fmt.Sprintf("%s(最后一次失败:%s)", disableReason, strings.TrimSpace(reason))
	}
	if ok := model.UpdateChannelStatus(channel.Id, "", common.ChannelStatusAutoDisabled, disableReason); ok {
		common.SysLog(fmt.Sprintf("auto model: 渠道 %d(%s)已无任何模型,已自动禁用。原因:%s",
			channel.Id, channel.Name, disableReason))
		return
	}
	common.SysError(fmt.Sprintf("auto model: 渠道 %d 已无模型,但自动禁用失败", channel.Id))
}

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
