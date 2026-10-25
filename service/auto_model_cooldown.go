package service

import (
	"context"
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
	"gorm.io/gorm"
)

// auto 候选冷却：把"这个 (分组, 模型, 渠道) 刚才请求失败了"记住一段明确的时间，
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
//   - 任何成功 → 清除该组合的冷却（别的模型偶然成功不代表这个组合已经好了）。
//   - 失败 → 记该组合；耗时超过 autoModelSlowLatencyMs 时原因里带上耗时。
//   - 耗时未知也按真实成败处理，不猜测延迟。
//
// 慢成功不再进冷却：它只是一份"这条路能成，但首 token 较慢"的延迟证据，
// 规划器会把慢路由排进更靠后的延迟池（后备），而不是把它整体停用。过去把
// 慢成功按失败处理，会把当时唯一能用的上游停掉，反而只能去抽挂死的路由。

const (
	// autoModelSlowLatencyMs 超过它就算"慢"。流式请求量到的是首 token 时间;
	// 非流式量不到首 token,用总耗时兜底(会包含生成长文的时间)。
	// 现在它只用于失败原因标注与"客户端取消前上游长时间零响应"的判据，
	// 不再触发慢成功冷却（见上）。
	autoModelSlowLatencyMs int64 = 20000
	// autoModelCooldownBase 是抖动阶梯的起步时长"默认值"。实际运行值取
	// operation_setting.AutoModelCooldownBaseMinutes()(面板可改,分钟),这里
	// 只是无配置时的兜底;测试也直接引用本常量锁定默认阶梯。
	autoModelCooldownBase = 15 * time.Minute
	// autoModelCooldownMax 是封顶的兜底常量。实际运行值取
	// operation_setting.AutoModelCooldownMax()(面板可改),这里只是没配置时的下限。
	autoModelCooldownMax = 6 * time.Hour
	// autoModelCooldownSecondTier 是阶梯第二档(首档翻倍)的默认值。评分衰减窗口
	// 必须严格大于它,否则第 2 次失败解冻时扣分正好归零(见测试与配置校验)。
	autoModelCooldownSecondTier = 2 * autoModelCooldownBase
)

// AutoModelCooldownBaseForValidation 暴露抖动冷却的首档时长(面板可配),供配置校验使用。
//
// 为什么需要它:评分衰减窗口必须长于首档冷却,否则第 1 次失败解冻时扣分几乎
// 已归零;而窗口等于第 2 档(30 分钟)时,第 2 次失败解冻的瞬间分数正好回到
// 中立,冷却彻底白做。校验要用同一个来源,不能在校验处再写一个 15 分钟常量,
// 否则以后调冷却阶梯时会悄悄失配。
func AutoModelCooldownBaseForValidation() time.Duration {
	return time.Duration(operation_setting.AutoModelCooldownBaseMinutes()) * time.Minute
}

// AutoModelCooldownSecondTierForValidation 暴露抖动冷却的第二档时长(首档翻倍)。
//
// 为什么单看首档不够:衰减窗口若正好等于第二档(30 分钟),第 2 次失败解冻的
// 那一刻分数精确回到中立的 0.5——惩罚"刚好"作废。所以真正的不变式是
// "窗口 > 第二档",而不是"窗口 > 首档"。
func AutoModelCooldownSecondTierForValidation() time.Duration {
	return 2 * time.Duration(operation_setting.AutoModelCooldownBaseMinutes()) * time.Minute
}

// autoModelCooldownState 是一条冷却记录的内存态。
// 只有抖动(普通)冷却一种:确定性失败直接摘除组合；摘除写入失败时用普通冷却隔离。
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

// Cooldown state is process-local. Writes are persisted before the next
// candidate is planned so this instance reacts immediately, but other instances
// are not notified and keep their in-memory snapshot until they reload/restart.
// No cross-instance push or shared cache is assumed.
var (
	autoModelCooldowns        = types.NewRWMap[string, autoModelCooldownState]()
	autoModelCooldownLoadOnce sync.Once
	// autoModelCooldownRecoveries 保存每个键最近一次“恢复正常/管理员清除”的
	// 观测时间。冷却行删除后，这个时间屏障仍要保留，否则一个在恢复前观测、
	// 经过长后备请求才发布的旧失败会重新冷却已经恢复的路线。
	// 由 autoModelCooldownMutex 保护。
	autoModelCooldownRecoveries = make(map[string]time.Time)
	// 同一把锁覆盖内存读改写及数据库操作，避免并发升级丢失、写入/清除乱序，
	// 以及重新载入旧快照覆盖刚收到的请求结果。锁顺序始终是本锁 → RWMap。
	autoModelCooldownMutex sync.Mutex
)

// LoadAutoModelCooldowns 从数据库重新载入冷却记录，包括已到期的失败记忆。
// 可重复调用（测试也用它）；生产路径只缓存成功的首次载入。
func LoadAutoModelCooldowns() {
	db := model.DB
	ctx, cancel := context.WithTimeout(context.Background(), autoModelScoreIOTimeout)
	defer cancel()
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	if loadAutoModelCooldownsLocked(ctx, db) {
		autoModelCooldownLoadOnce.Do(func() {})
	}
	flushPendingAutoModelCooldownsLocked(ctx, db)
}

// loadAutoModelCooldownsLocked 的调用方必须持有 autoModelCooldownMutex。
// 失败或超时保留已有内存状态，且不把这次尝试当成成功初始化。
func loadAutoModelCooldownsLocked(ctx context.Context, db *gorm.DB) bool {
	if db == nil {
		return false
	}
	rows, err := model.GetAutoModelCooldownsContext(ctx, db)
	if err != nil {
		common.SysError("failed to load auto model cooldowns: " + err.Error())
		return false
	}
	loaded := make(map[string]autoModelCooldownState, len(rows))
	health := autoModelHealth.ReadAll()
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			common.SysError("failed to finish auto model cooldown load: " + err.Error())
			return false
		}
		purge := func(description string, row model.AutoModelCooldown) {
			if err := db.WithContext(ctx).Delete(&row).Error; err != nil {
				common.SysError("failed to purge " + description + " auto model cooldown: " + err.Error())
			}
		}
		if row.Model == "" {
			// 渠道级冷却已废弃（见 applyAutoModelCooldown）：顺手清掉历史残留，
			// 否则旧记录还会继续让 auto 整条渠道避让，把该渠道里健康的模型一起错过。
			// 按主键删除，避免 struct Where 忽略空 Model 而误删同渠道其它模型。
			purge("legacy channel-level", row)
			continue
		}
		if row.Permanent {
			// "永久级冷却"已废弃：确定性失败不再产生冷却（命中关键词的组合会被
			// 直接摘除）。旧版本留下的 Permanent=true 记录在这里一并清掉——新代码
			// 没有任何路径会去清除它们，不清就永远挡路。
			purge("legacy permanent", row)
			continue
		}
		until := time.Unix(row.Until, 0)
		// Expiry only permits another recovery attempt. Preserve the failure
		// reason and escalation level until a real success or administrator
		// clear, including when score evidence has expired or the process restarts.
		key := autoModelHealthKey(row.Group, row.Model, row.ChannelId)
		state := autoModelCooldownState{
			Until:     until,
			Level:     row.Level,
			Reason:    row.Reason,
			UpdatedAt: time.Unix(row.UpdatedAt, 0),
		}
		// A restored business success may have been saved before a failed
		// cooldown deletion. Cooldown timestamps have only second precision:
		// require success in a later second to avoid reversing same-second events.
		if outcome := health[key]; outcome.SuccessCount > 0 && !outcome.PendingRecovery &&
			outcome.LastObservedAt.Unix() > row.UpdatedAt {
			state.Until = time.Time{}
			state.pending = &autoModelCooldownPendingWrite{group: row.Group, name: row.Model, channelID: row.ChannelId, delete: true}
			if recoveredAt := autoModelCooldownRecoveries[key]; outcome.LastObservedAt.After(recoveredAt) {
				autoModelCooldownRecoveries[key] = outcome.LastObservedAt
			}
		}
		loaded[key] = state
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
	db := model.DB
	ctx, cancel := context.WithTimeout(context.Background(), autoModelScoreIOTimeout)
	defer cancel()
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked(ctx, db)
}

func ensureAutoModelCooldownsLoadedLocked(ctx context.Context, db *gorm.DB) bool {
	loaded := true
	autoModelCooldownLoadOnce.Do(func() {
		loaded = loadAutoModelCooldownsLocked(ctx, db)
	})
	if !loaded {
		// sync.Once 仅表示成功载入；数据库未就绪或查询失败时允许下一次重试。
		// Do 和重置都在事务锁内，不会与其它调用并发访问 Once。
		autoModelCooldownLoadOnce = sync.Once{}
	}
	flushPendingAutoModelCooldownsLocked(ctx, db)
	return loaded
}

// autoModelCooldownSnapshot 返回同一时刻的独立快照，调用方不持有事务锁。
// The first call may load or retry pending SQL; it uses one bounded budget so a
// blocked database cannot hold the cooldown lock indefinitely.
func autoModelCooldownSnapshot() map[string]autoModelCooldownState {
	db := model.DB
	ctx, cancel := context.WithTimeout(context.Background(), autoModelScoreIOTimeout)
	defer cancel()
	return autoModelCooldownSnapshotContext(ctx, db)
}

func autoModelCooldownSnapshotContext(ctx context.Context, db *gorm.DB) map[string]autoModelCooldownState {
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked(ctx, db)
	return autoModelCooldowns.ReadAll()
}

// nextAutoModelCooldown 计算下一次冷却:每犯一次错,窗口翻一倍,封顶取面板配置。
func nextAutoModelCooldown(prev autoModelCooldownState, reason string, now time.Time) autoModelCooldownState {
	// 起步值与封顶都可由面板改(operation_setting)。任一取不到或不合法时退回
	// 常量兜底,且封顶必须 >= 首档,否则阶梯会一开始就"封"到比首档还短。
	base := time.Duration(operation_setting.AutoModelCooldownBaseMinutes()) * time.Minute
	maximum := operation_setting.AutoModelCooldownMax()
	if maximum < base {
		maximum = autoModelCooldownMax
	}
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
	applyAutoModelCooldownWithReason(group, name, channelID, success, elapsedMs, "")
}

// applyAutoModelCooldownWithReason 与 applyAutoModelCooldown 相同，但失败时可以
// 带上游错误摘要。摘要由 service.AutoModelFailureReason 生成，只包含错误码与
// HTTP 状态；这里再做一次长度限制，避免面板与数据库出现超长原因。
func applyAutoModelCooldownWithReason(group, name string, channelID int, success bool, elapsedMs int64, failureReason string) {
	applyAutoModelCooldownWithReasonAt(group, name, channelID, success, elapsedMs, failureReason, time.Now())
}

// applyAutoModelCooldownWithReasonAt 使用入队时固定的观测时间。延迟发布的旧反馈
// 不能清除或加重一条由更新观测建立的冷却。
func applyAutoModelCooldownWithReasonAt(group, name string, channelID int, success bool, elapsedMs int64, failureReason string, observedAt time.Time) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	now := observedAt
	if success {
		// 成功就是恢复证据，不因首 token 慢而降级。慢只会让它在延迟池里
		// 排到更快且同样可靠的路由后面。
		clearAutoModelCooldown(group, name, channelID, now)
		return
	}

	reason := strings.TrimSpace(failureReason)
	if elapsedMs > autoModelSlowLatencyMs {
		if reason != "" {
			reason = fmt.Sprintf("超时/失败（%dms）: %s", elapsedMs, reason)
		} else {
			reason = fmt.Sprintf("超时/失败（%dms）", elapsedMs)
		}
	} else if reason == "" {
		reason = "请求失败"
	}
	reason = truncateAutoModelCooldownReason(reason)
	tripAutoModelCooldown(group, name, channelID, reason, now)

	// 只冷却"这个渠道上的这个模型"。
	// 早期版本在这里额外冷却了整条渠道（同上游的其它模型一并避让），但他的判断更准确：
	// 一个渠道下 5 个模型，试挂了 2 个不代表剩下 3 个也不行——它们可能又快又稳，
	// 整条避让就把它们错过了。所以渠道级冷却已移除（历史上的 model='' 记录在载入时清理）。
}

func truncateAutoModelCooldownReason(reason string) string {
	const maxRunes = 200
	runes := []rune(strings.TrimSpace(reason))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "…"
}

// tripAutoModelCooldown 让一个 (分组, 模型, 渠道) 组合进入(或加重)抖动冷却。
func tripAutoModelCooldown(group, name string, channelID int, reason string, now time.Time) {
	if group == "" || name == "" || channelID <= 0 {
		return
	}
	if reason == "" {
		reason = "请求失败"
	}
	db := model.DB
	ctx, cancel := context.WithTimeout(context.Background(), autoModelScoreIOTimeout)
	defer cancel()
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked(ctx, db)

	key := autoModelHealthKey(group, name, channelID)
	// The recovery timestamp is the ordering barrier and must be checked BEFORE
	// discarding a pending delete: a success that cleared the cooldown is newer
	// evidence even when its database deletion has not been confirmed yet.
	if recoveredAt, recovered := autoModelCooldownRecoveries[key]; recovered {
		if recoveredAt.After(now) {
			return
		}
		delete(autoModelCooldownRecoveries, key)
	}
	prev, _ := autoModelCooldowns.Get(key)
	if !prev.UpdatedAt.IsZero() && prev.UpdatedAt.After(now) {
		return // A stale, delayed failure must not override a newer observation.
	}
	if (prev.pending != nil && prev.pending.delete) || prev.Until.IsZero() {
		// A recovered combination starts a fresh ladder; keep the ordering check
		// above intact by clearing the old level only after it.
		prev = autoModelCooldownState{}
	}
	state := nextAutoModelCooldown(prev, reason, now)
	state.pending = &autoModelCooldownPendingWrite{group: group, name: name, channelID: channelID}
	autoModelCooldowns.Set(key, state)
	if err := persistAutoModelCooldownLocked(ctx, db, key, state); err != nil {
		return
	}
	common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)进入冷却,%s 后再试,等级 %d,原因:%s",
		name, channelID, state.Until.Sub(now).Round(time.Second), state.Level, reason))
}

// RecordAutoModelPermanentFailure 处理一次确定性失败(命中管理员配置的
// 确定性失败关键词,见 AutoModelPermanentFailure)。
//
// 没有冷却档:确定性失败重试也不会好,给它挂一个"过几小时再撞一次"的窗口
// 只是推迟必然的失败,还会让废组合在窗口到期后再次被选中、再次浪费一次尝试。
// 命中即把该 (分组, 模型, 渠道) 组合直接摘除(见 RetireAutoModelCombination):
// 模型从渠道声明里删除、abilities 同步失效;渠道被摘空时自动禁用。
//
// 摘除失败(如数据库暂不可用)时先隔离该组合，防止新的请求持续撞上它。
// 冷却到期后只作为待恢复后备；再次失败仍会尝试摘除。
func RecordAutoModelPermanentFailure(group, name string, channelID int, reason string) {
	RecordAutoModelPermanentFailureAt(group, name, channelID, reason, time.Now())
}

// RecordAutoModelPermanentFailureAt applies the same clear-generation guard as
// evidence publication: a deterministic failure observed before an administrator
// clear must not retire the route after that clear.
func RecordAutoModelPermanentFailureAt(group, name string, channelID int, reason string, observedAt time.Time) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	reason = strings.TrimSpace(reason)
	if group == "" || name == "" || channelID <= 0 {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	key := autoModelHealthKey(group, name, channelID)
	unlock := lockAutoModelRoute(key)
	defer unlock()
	if autoModelObservationCleared(key, observedAt) {
		return
	}
	// Keep the route publication lock until the retirement transaction and the
	// cooldown cleanup have committed. An administrator clear arriving after
	// this point installs its tombstone/barrier after the retirement.
	if err := retireAutoModelCombinationLocked(group, name, channelID, reason); err != nil {
		common.SysError(fmt.Sprintf("auto model: retire %s(渠道 %d) failed: %s", name, channelID, err.Error()))
		autoModelScoreMutex.Lock()
		autoModelHealth.Update(key, func(current autoModelOutcome, exists bool) autoModelOutcome {
			return updateAutoModelOutcome(current, exists, false, false, false, 0, observedAt)
		})
		autoModelScoreMutex.Unlock()
		tripAutoModelCooldown(group, name, channelID, "摘除待重试: "+reason, observedAt)
	}
}

// RecordAutoModelAbandonedAttempt 记录一次"客户端取消前,上游长时间零响应"的挂起。
//
// 客户端取消本身不是上游的证据(它可能只是等不及一个慢而正常的流式响应),所以
// 取消路径默认不记任何东西。但有一个例外必须处理:本次尝试确实派发了,却整整
// 20 秒(autoModelSlowLatencyMs)以上一个字节都没写出来——挂死的上游会一直占住
// 计划头部,每个请求都死在它身上,健康的候选永远轮不到;而取消路径不留任何证据,
// 下一批请求会原样再撞一遍。这类长挂起记录失败证据并按抖动冷却处理，让
// 计划头尽快让位;短取消(<20s)仍然不记,保护"用户主动停止"的正常场景。
func RecordAutoModelAbandonedAttempt(group, name string, channelID int, elapsedMs int64) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return
	}
	if elapsedMs < autoModelSlowLatencyMs {
		return
	}
	reason := fmt.Sprintf("客户端取消(上游 %dms 零响应)", elapsedMs)
	// A 20s+ zero-byte upstream attempt is real failure evidence: it must also
	// mark the pair pending recovery so a cooldown expiry cannot silently make it
	// a main-traffic candidate again.
	recordAutoModelOutcome(group, name, channelID, false, false, false, 0, elapsedMs, false, reason, time.Now())
}

// RetireAutoModelCombination 把一个命中确定性关键词的 (分组, 模型, 渠道) 组合从
// 候选池里彻底摘掉:从渠道声明的 models 里移除该模型,并同步重建 abilities。
//
// 不删除整条渠道(渠道上还有别的模型,凭据与配置都是复用的),只摘掉这个不可用的
// 模型。读改写、abilities 重建与摘空后的渠道状态变更都在 model 层的原子入口里
// 作为一个数据库事务完成;这里只负责失败分类、调用和确认成功后的冷却清理与日志。
//
// 幂等:模型已经不在 models 里时,model 层会核对并修复遗留 ability,不再重复更新
// models;并发摘除由 CAS + 有限重试保证互不覆盖,最终不会复活任何一个被摘模型。
func RetireAutoModelCombination(group, name string, channelID int, reason string) error {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	reason = strings.TrimSpace(reason)
	if group == "" || name == "" || channelID <= 0 {
		return nil
	}
	unlock := lockAutoModelRoute(autoModelHealthKey(group, name, channelID))
	defer unlock()
	return retireAutoModelCombinationLocked(group, name, channelID, reason)
}

// retireAutoModelCombinationLocked must be called with the route publication
// lock held. It deliberately keeps the score/cooldown global locks out of the
// retirement SQL transaction.
func retireAutoModelCombinationLocked(group, name string, channelID int, reason string) error {
	db := model.DB
	ctx, cancel := context.WithTimeout(context.Background(), autoModelScoreIOTimeout)
	defer cancel()
	// model.RetireChannelModel 自带 DB 未初始化检查,这里不复述。事务开始、查询、
	// 更新、abilities 重建和 CAS 重试退避都受同一个截止时间约束；超时后返回，
	// 调用方释放路线发布锁，反馈和试用名额才能继续推进。
	result, err := model.RetireChannelModel(ctx, channelID, name, reason)
	if err != nil {
		return fmt.Errorf("retire %s on channel %d failed: %w", name, channelID, err)
	}
	// 冷却清理与成功日志放在数据库确认之后:摘除失败必须返回明确错误,不假报成功。
	// 清理继续使用刚才的预算；若已耗尽，pending 会留给后台重试。
	if cooldownErr := clearAutoModelCooldownWithResultContext(ctx, db, group, name, channelID, time.Now()); cooldownErr != nil {
		common.SysLog(fmt.Sprintf("auto model: cooldown cleanup after retiring %s(渠道 %d) is pending: %s", name, channelID, cooldownErr.Error()))
	}
	if result.Removed {
		common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)命中确定性失败判据,已从渠道模型列表删除(原因:%s)",
			name, channelID, reason))
	}
	if result.Emptied {
		common.SysLog(fmt.Sprintf("auto model: 渠道 %d 已无任何模型,已自动禁用。原因:%s", channelID, reason))
	}
	return nil
}

func clearAutoModelCooldown(group, name string, channelID int, now time.Time) {
	_ = clearAutoModelCooldownWithResult(group, name, channelID, now)
}

// clearAutoModelCooldownWithResult returns the database result of the deletion.
// The in-memory recovery still takes effect when SQL fails; the caller may then
// report the persistence as pending and leave the retry marker in the cache.
func clearAutoModelCooldownWithResult(group, name string, channelID int, now time.Time) error {
	db := model.DB
	ctx, cancel := context.WithTimeout(context.Background(), autoModelScoreIOTimeout)
	defer cancel()
	return clearAutoModelCooldownWithResultContext(ctx, db, group, name, channelID, now)
}

// clearAutoModelCooldownWithResultContext lets the administrator clear share one
// deadline (and DB handle) with the score tombstone write.
func clearAutoModelCooldownWithResultContext(ctx context.Context, db *gorm.DB, group, name string, channelID int, now time.Time) error {
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	loaded := ensureAutoModelCooldownsLoadedLocked(ctx, db)

	key := autoModelHealthKey(group, name, channelID)
	previous, hasPrevious := autoModelCooldowns.Get(key)
	barrier := autoModelCooldownRecoveries[key]
	if hasPrevious && previous.UpdatedAt.After(barrier) {
		barrier = previous.UpdatedAt
	}
	if !barrier.IsZero() && now.Before(barrier) {
		// This success is strictly older than a recovery or observation already
		// accepted. Preserve the newer state and never move the barrier backwards.
		// Equal timestamps keep the established test/deterministic tie semantics.
		if barrier.After(autoModelCooldownRecoveries[key]) {
			autoModelCooldownRecoveries[key] = barrier
		}
		return nil
	}
	if !hasPrevious {
		if loaded {
			// No stored cooldown to delete, but this success is still an ordering
			// barrier: a failure observed before it must not cool the route later.
			autoModelCooldownRecoveries[key] = now
			return nil
		}
	} else if previous.pending != nil && previous.pending.delete {
		// Keep the pending deletion but advance its ordering barrier; an older
		// success must not replace a newer pending recovery.
		autoModelCooldownRecoveries[key] = now
		previous.UpdatedAt = now
		autoModelCooldowns.Set(key, previous)
		return persistAutoModelCooldownLocked(ctx, db, key, previous)
	}
	// Publish recovery immediately, but retain the deletion intent until the
	// database confirms it. If initial loading failed, even an absent cache key
	// may have an old stored cooldown that this successful request must clear.
	// The recovery barrier is independent of the cooldown row lifecycle: deleting
	// the row must not delete the evidence that this observation is newer.
	autoModelCooldownRecoveries[key] = now
	state := autoModelCooldownState{
		UpdatedAt: now,
		pending:   &autoModelCooldownPendingWrite{group: group, name: name, channelID: channelID, delete: true},
	}
	autoModelCooldowns.Set(key, state)
	if err := persistAutoModelCooldownLocked(ctx, db, key, state); err != nil {
		return err
	}
	common.SysLog(fmt.Sprintf("auto model: %s（渠道 %d）恢复正常，清除冷却", name, channelID))
	return nil
}

// persistAutoModelCooldownLocked commits one pending intent. Failures leave it
// in the same map for later synchronous retries; callers hold the transaction lock.
func persistAutoModelCooldownLocked(ctx context.Context, db *gorm.DB, key string, state autoModelCooldownState) error {
	pending := state.pending
	if pending == nil {
		return nil
	}
	if db == nil {
		return errors.New("auto model cooldown database is not initialized")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if pending.delete {
		if err := model.DeleteAutoModelCooldownContext(ctx, db, pending.group, pending.name, pending.channelID); err != nil {
			common.SysError("failed to clear auto model cooldown: " + err.Error())
			return err
		}
		// The recovery barrier lives in autoModelCooldownRecoveries; the cooldown
		// cache key itself can be removed once the row is gone.
		autoModelCooldowns.Delete(key)
		return nil
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
	if err := model.UpsertAutoModelCooldownContext(ctx, db, row); err != nil {
		common.SysError("failed to save auto model cooldown: " + err.Error())
		return err
	}
	state.pending = nil
	autoModelCooldowns.Set(key, state)
	return nil
}

// flushPendingAutoModelCooldownsLocked retries recovery from a synchronous read
// path. It must use the caller's already-bounded context so loading and pending
// writes share one deadline instead of renewing it for every stage.
func flushPendingAutoModelCooldownsLocked(ctx context.Context, db *gorm.DB) {
	_ = retryPendingAutoModelCooldownWritesLocked(ctx, db)
}

func retryPendingAutoModelCooldownWritesLocked(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return nil
	}
	var joined error
	for key, state := range autoModelCooldowns.ReadAll() {
		if state.pending == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(joined, err)
		}
		if err := persistAutoModelCooldownLocked(ctx, db, key, state); err != nil {
			joined = errors.Join(joined, fmt.Errorf("cooldown %s: %w", key, err))
			if ctx.Err() != nil {
				return joined
			}
		}
	}
	return joined
}

// RetryPendingAutoModelCooldownWrites is called by the score persistence
// worker so a failed cooldown write is retried even when no request touches the
// affected route. It shares the worker's single deadline and stable DB handle.
// A busy cooldown lock is skipped for this round instead of blocking the worker;
// the pending state remains for the next tick.
func RetryPendingAutoModelCooldownWrites(ctx context.Context, db *gorm.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !autoModelCooldownMutex.TryLock() {
		return nil
	}
	defer autoModelCooldownMutex.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return retryPendingAutoModelCooldownWritesLocked(ctx, db)
}

// AutoModelCooldownInfo 是给管理员看的一条当前生效的冷却。
// 这是只读视图:不含请求内容、凭据或任何上游信息。
type AutoModelCooldownInfo struct {
	Group            string `json:"group"`
	Model            string `json:"model"`
	ChannelID        int    `json:"channel_id"`
	Level            int    `json:"level"`
	Reason           string `json:"reason"`
	Until            int64  `json:"until"`
	RemainingSeconds int64  `json:"remaining_seconds"`
	UpdatedAt        int64  `json:"updated_at"`
}

// ListAutoModelCooldowns 返回当前仍在冷却的组合,按到期时间从近到远排序。
//
// 为什么要它:抖动冷却阶梯是自我强化的(起步时长起步、翻倍、封顶可配),而清除
// 条件是一次真实成功（耗时不限）——冷却中的组合根本不会被路由,所以它只能等
// 窗口自然到期。没有这个入口,管理员在故障时看不到 auto 挡住了谁、为什么、还要
// 多久,只能手改数据库。共享同一把读锁,不改动任何状态。
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

// GetAutoModelCooldown 返回指定 (分组, 模型, 渠道) 当前仍在冷却中的状态,
// 供排障日志等只读场景使用。第二个返回值表示是否存在生效中的冷却。
func GetAutoModelCooldown(group, name string, channelID int, now time.Time) (AutoModelCooldownInfo, bool) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return AutoModelCooldownInfo{}, false
	}
	snapshot := autoModelCooldownSnapshot()
	state, ok := snapshot[autoModelHealthKey(group, name, channelID)]
	if !ok || !state.Until.After(now) {
		return AutoModelCooldownInfo{}, false
	}
	remaining := int64(state.Until.Sub(now).Seconds())
	if remaining < 0 {
		remaining = 0
	}
	return AutoModelCooldownInfo{
		Group:            group,
		Model:            name,
		ChannelID:        channelID,
		Level:            state.Level,
		Reason:           state.Reason,
		Until:            state.Until.Unix(),
		RemainingSeconds: remaining,
		UpdatedAt:        state.UpdatedAt.Unix(),
	}, true
}

// DeleteAutoModelCooldown 手工解除一条冷却(管理员操作)。
// 只删指定组合;参数不全时拒绝,避免误删整片冷却。
func DeleteAutoModelCooldown(group, name string, channelID int) error {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return fmt.Errorf("无效的冷却标识:分组/模型/渠道都必须给出")
	}
	key := autoModelHealthKey(group, name, channelID)
	unlock := lockAutoModelRoute(key)
	defer unlock()
	db := model.DB
	ctx, cancel := context.WithTimeout(context.Background(), autoModelScoreIOTimeout)
	defer cancel()
	autoModelCooldownMutex.Lock()
	defer autoModelCooldownMutex.Unlock()
	ensureAutoModelCooldownsLoadedLocked(ctx, db)

	// 先落库,成功后再撤内存:反过来的话,库写失败会让内存以为已恢复。
	if db != nil {
		if err := model.DeleteAutoModelCooldownContext(ctx, db, group, name, channelID); err != nil {
			return err
		}
	}
	// Keep a recovery barrier for the same reason as a real success: a delayed
	// failure observed before this manual clear must not cool the route again.
	autoModelCooldowns.Delete(key)
	if now := time.Now(); now.After(autoModelCooldownRecoveries[key]) {
		autoModelCooldownRecoveries[key] = now
	}
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
