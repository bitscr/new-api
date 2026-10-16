package model

import (
	"errors"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm/clause"
)

// AutoModelRoutingTarget is a caller-owned scalar snapshot of an enabled ability
// on an existing, enabled channel, never a mutable cached Channel reference.
// Priority is Channel.Priority (nil means zero), not Ability.Priority.
// Weight is a CHANNEL selection weight, never a per-model selection weight:
// it is Channel.Weight (nil means zero) in memory-cache mode and Channel.Weight
// + 10 in DB mode to preserve the existing DB sampling smoothing. Ability.Weight
// is deliberately ignored. An all-zero memory weight band is sampled uniformly;
// model-selection weights are configured independently of this snapshot.
type AutoModelRoutingTarget struct {
	ChannelID int
	Priority  int64
	Weight    float64
}

// auto 每次请求会对同一分组做 2~8 次 GetAutoModelRoutingTargets(规划、重试重排、
// 逐次尝试的授权复验、冷却排除),全部落在同一条 abilities⋈channels 批量查询上。
// 这是 auto 热路径上唯一一处每请求重复的 DB 查询,这里用短 TTL 缓存把它收拢成
// 每 TTL 至多一次:
//
//   - TTL(autoModelRoutingTargetsCacheTTL,2 秒)是兜底:任何未被失效钩子覆盖的
//     写入,最多延迟一个 TTL 生效。它严格短于官方内存渠道缓存(SyncChannelCache
//     周期同步)的陈旧度,不会比既有路径更不新鲜。
//   - 失效钩子(InvalidateAutoModelRoutingTargetsCache)挂在 abilities 层的全部
//     写入口(AddAbilities/UpdateAbilities/DeleteAbilities/UpdateAbilityStatus*/
//     UpdateAbilityByTag/FixAbility):管理员增删改渠道、改分组/模型/优先级/权重、
//     渠道被禁用(含 auto 自动禁用)都经这些函数,缓存立刻失效。
//   - 读返回深拷贝:既有契约是"调用方可自由修改返回值"(见
//     TestAutoModelRoutingTargetsSnapshot 的 mutation 断言),共享缓存后由
//     拷贝来继续满足它。每次拷贝是 O(目标数) 的切片复制,远小于一次 DB 查询。
//
// 新鲜度边界:授权复验(getFixedAutoModelChannel)读到的能力快照最多滞后 TTL。
// 它不是授权边界——分组与令牌的授权在分发层判定;这里只是"该渠道还挂着这个
// 模型吗"的快速否决。最坏情况是一个刚被禁用的组合在 ≤TTL 内多打一次上游,
// 结果会被记录并进入冷却,不产生正确性问题。

// 生产默认 2 秒;测试用例可以改短以验证过期行为(改完必须还原)。
var autoModelRoutingTargetsCacheTTL = 2 * time.Second

type autoModelRoutingTargetsEntry struct {
	targets   map[string][]AutoModelRoutingTarget
	fetchedAt time.Time
}

var (
	autoModelRoutingTargetsMu sync.Mutex
	autoModelRoutingTargets   = make(map[string]autoModelRoutingTargetsEntry)
)

// InvalidateAutoModelRoutingTargetsCache 丢弃全部分组的路由目标缓存。
// 必须在任何会改变 abilities⋈channels 快照的写路径成功执行后调用。
func InvalidateAutoModelRoutingTargetsCache() {
	autoModelRoutingTargetsMu.Lock()
	autoModelRoutingTargets = make(map[string]autoModelRoutingTargetsEntry)
	autoModelRoutingTargetsMu.Unlock()
}

// cloneAutoModelRoutingTargets 深拷贝快照,调用方对返回值的任何修改都不会
// 影响缓存本身。
func cloneAutoModelRoutingTargets(src map[string][]AutoModelRoutingTarget) map[string][]AutoModelRoutingTarget {
	dst := make(map[string][]AutoModelRoutingTarget, len(src))
	for model, targets := range src {
		dst[model] = append([]AutoModelRoutingTarget(nil), targets...)
	}
	return dst
}

// queryAutoModelRoutingTargets 执行那一条批量 join 查询并组装快照。
func queryAutoModelRoutingTargets(group string) (map[string][]AutoModelRoutingTarget, error) {
	var rows []struct {
		Model     string
		ChannelID int
		Priority  *int64
		Weight    *uint
	}
	if err := DB.Table("abilities").
		Select("abilities.model, channels.id AS channel_id, channels.priority, channels.weight").
		Joins("INNER JOIN channels ON channels.id = abilities.channel_id").
		Where(clause.Eq{Column: clause.Column{Table: "abilities", Name: "group"}, Value: group}).
		Where("abilities.enabled = ? AND channels.status = ?", true, common.ChannelStatusEnabled).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make(map[string][]AutoModelRoutingTarget)
	for _, row := range rows {
		target := AutoModelRoutingTarget{ChannelID: row.ChannelID}
		if row.Priority != nil {
			target.Priority = *row.Priority
		}
		if row.Weight != nil {
			target.Weight = float64(*row.Weight)
		}
		if !common.MemoryCacheEnabled {
			target.Weight += 10
		}
		result[row.Model] = append(result[row.Model], target)
	}
	return result, nil
}

// GetAutoModelRoutingTargetsFresh 是绕过缓存的强新鲜变体,专供逐次尝试的
// 授权复验(getFixedAutoModelChannel)使用:任何绕过失效钩子的写入(直接改库、
// 未来新增而忘记挂钩的路径)都会被它看到。查询成功后顺带刷新缓存,让规划路径
// 也共享这次的新鲜快照。
func GetAutoModelRoutingTargetsFresh(group string) (map[string][]AutoModelRoutingTarget, error) {
	if DB == nil {
		return nil, errors.New("routing database is not initialized")
	}
	autoModelRoutingTargetsMu.Lock()
	defer autoModelRoutingTargetsMu.Unlock()
	targets, err := queryAutoModelRoutingTargets(group)
	if err != nil {
		return nil, err
	}
	autoModelRoutingTargets[group] = autoModelRoutingTargetsEntry{targets: targets, fetchedAt: time.Now()}
	return cloneAutoModelRoutingTargets(targets), nil
}

// GetAutoModelRoutingTargets snapshots a group's enabled abilities joined to
// existing, enabled channels in one batch query, with no per-channel queries.
// Both cache modes query the database: the legacy channel cache is built from
// Channel.Models/Group and does not preserve Ability.Enabled. Only the channel
// weight smoothing differs between modes; eligibility and priorities do not.
// The result has no guaranteed order. The returned map is a private deep copy:
// callers may modify it freely without affecting the cache or other callers.
func GetAutoModelRoutingTargets(group string) (map[string][]AutoModelRoutingTarget, error) {
	if DB == nil {
		return nil, errors.New("routing database is not initialized")
	}
	// 全程持锁(含查询):双检会让 TTL 到期瞬间的并发未命中各自重复查询,
	// 而这条查询本身是索引上的批量 join,持锁串行的代价远小于惊群。
	autoModelRoutingTargetsMu.Lock()
	defer autoModelRoutingTargetsMu.Unlock()
	if entry, ok := autoModelRoutingTargets[group]; ok && time.Since(entry.fetchedAt) < autoModelRoutingTargetsCacheTTL {
		return cloneAutoModelRoutingTargets(entry.targets), nil
	}
	targets, err := queryAutoModelRoutingTargets(group)
	if err != nil {
		// 失败不缓存:失败路径(断连、表缺失)不允许落回旧快照,也不留下
		// 永久失败的记录;下一次调用会重新查询。
		return nil, err
	}
	autoModelRoutingTargets[group] = autoModelRoutingTargetsEntry{targets: targets, fetchedAt: time.Now()}
	return cloneAutoModelRoutingTargets(targets), nil
}
