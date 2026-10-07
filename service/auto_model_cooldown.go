package service

import (
	"fmt"
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
// 分两层：
//   - 模型级：键是 (分组, 模型, 渠道)，只挡这一个组合。
//   - 渠道级：模型名为空的那条记录 (分组, "", 渠道)，挡该渠道上的所有模型。
//     上游是"排队"型时，同一渠道上的其它模型也会同样卡住，所以一次慢事件
//     要连带整个渠道一起避让，否则 auto 会顺着模型列表一个个踩过去。
//
// 为什么需要它：内存里的健康分只做短期反应（约 10 分钟线性衰减归零），重启或者
// 空闲一会儿就全没了，于是 auto 又会去挑那个排在最前面、但上游正在排队几十秒的
// 候选，客户端等不到响应就报错。冷却窗口不衰减，只有到期才会被重新尝试；
// 重复犯错时窗口逐级翻倍（15m → 30m → 1h → 2h → 4h → 6h 封顶）。
//
// 判定规则（elapsedMs 是本次请求的总耗时）：
//   - 正常速度的成功 → 清除该组合的模型级冷却；渠道级只按自己的到期时间失效，
//     别的模型偶然成功不代表这个渠道已经好了。
//   - 慢（超过 autoModelSlowLatencyMs）的成功或失败 → 模型级 + 渠道级一起进冷却。
//   - 快速失败（例如 401、余额不足）→ 只记模型级，不连累整个渠道。
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
}

var (
	autoModelCooldowns        = types.NewRWMap[string, autoModelCooldownState]()
	autoModelCooldownLoadOnce sync.Once
)

// LoadAutoModelCooldowns 从数据库重新载入冷却记录，过期的顺手清库。
// 可重复调用（测试也用它）；生产路径通过 ensureAutoModelCooldownsLoaded 只跑一次。
func LoadAutoModelCooldowns() {
	if model.DB == nil {
		return
	}
	rows, err := model.GetAutoModelCooldowns()
	if err != nil {
		common.SysError("failed to load auto model cooldowns: " + err.Error())
		return
	}
	now := time.Now()
	loaded := make(map[string]autoModelCooldownState, len(rows))
	for _, row := range rows {
		until := time.Unix(row.Until, 0)
		if !until.After(now) {
			if err := model.DeleteAutoModelCooldown(row.Group, row.Model, row.ChannelId); err != nil {
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
	autoModelCooldowns.Clear()
	autoModelCooldowns.AddAll(loaded)
}

func ensureAutoModelCooldownsLoaded() {
	autoModelCooldownLoadOnce.Do(LoadAutoModelCooldowns)
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

// applyAutoModelCooldown 依据一次 auto 请求的结果维护冷却（模型级 + 渠道级）。
func applyAutoModelCooldown(group, name string, channelID int, success bool, elapsedMs int64) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return
	}
	if success && elapsedMs <= 0 {
		return // 成功但没有耗时数据：不动冷却
	}
	ensureAutoModelCooldownsLoaded()

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

	// 慢事件会连累整个渠道：同一上游上的其它模型大概率也在排队。
	// 快速失败（鉴权、余额之类）只算这个模型自己的问题。
	if success || elapsedMs > autoModelSlowLatencyMs {
		tripAutoModelCooldown(group, "", channelID, reason, now)
	}
}

// tripAutoModelCooldown 让一个 (分组, 模型, 渠道) 组合进入（或加重）冷却。
// name 为空表示渠道级冷却。
func tripAutoModelCooldown(group, name string, channelID int, reason string, now time.Time) {
	key := autoModelHealthKey(group, name, channelID)
	prev, _ := autoModelCooldowns.Get(key)
	state := nextAutoModelCooldown(prev, reason, now)
	autoModelCooldowns.Set(key, state)

	scope := name
	if scope == "" {
		scope = "该渠道上的所有模型"
	}
	if model.DB == nil {
		common.SysLog(fmt.Sprintf("auto model: %s（渠道 %d）进入冷却（未落库：没有数据库连接），等级 %d", scope, channelID, state.Level))
		return
	}
	row := &model.AutoModelCooldown{
		Group:     group,
		Model:     name,
		ChannelId: channelID,
		Until:     state.Until.Unix(),
		Level:     state.Level,
		Reason:    reason,
		UpdatedAt: state.UpdatedAt.Unix(),
	}
	if err := model.UpsertAutoModelCooldown(row); err != nil {
		common.SysError("failed to save auto model cooldown: " + err.Error())
		return
	}
	common.SysLog(fmt.Sprintf("auto model: %s（渠道 %d）进入冷却，%s 后再试，等级 %d，原因：%s",
		scope, channelID, state.Until.Sub(now).Round(time.Second), state.Level, reason))
}

// clearAutoModelCooldown 正常速度的成功：清掉这个组合的模型级冷却。
func clearAutoModelCooldown(group, name string, channelID int, now time.Time) {
	key := autoModelHealthKey(group, name, channelID)
	if _, ok := autoModelCooldowns.Get(key); !ok {
		return
	}
	deleteAutoModelCooldownCache(key)
	if model.DB != nil {
		if err := model.DeleteAutoModelCooldown(group, name, channelID); err != nil {
			common.SysError("failed to clear auto model cooldown: " + err.Error())
			return
		}
	}
	common.SysLog(fmt.Sprintf("auto model: %s（渠道 %d）恢复正常，清除冷却", name, channelID))
}

// deleteAutoModelCooldownCache 从内存表里移除一条（RWMap 没有 Delete，整体重建）。
func deleteAutoModelCooldownCache(key string) {
	all := autoModelCooldowns.ReadAll()
	if _, ok := all[key]; !ok {
		return
	}
	delete(all, key)
	autoModelCooldowns.Clear()
	autoModelCooldowns.AddAll(all)
}

// autoModelChannelCooling 报告某个渠道是否处于渠道级冷却中（name 传空）。
func autoModelChannelCooling(group string, channelID int, now time.Time) bool {
	state, ok := autoModelCooldowns.Get(autoModelHealthKey(group, "", channelID))
	return ok && state.Until.After(now)
}

// autoModelCooldownActive 报告该模型的渠道是否全部不可用：
// 只要还有一个渠道既没有模型级冷却、也没有渠道级冷却，就算还能试。
func autoModelCooldownActive(group, name string, channelIDs []int) bool {
	if len(channelIDs) == 0 {
		return false
	}
	ensureAutoModelCooldownsLoaded()
	now := time.Now()
	for _, id := range channelIDs {
		if autoModelChannelCooling(group, id, now) {
			continue
		}
		state, ok := autoModelCooldowns.Get(autoModelHealthKey(group, name, id))
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
	allCooling := make(map[string]bool, len(candidates))
	dropped := make([]string, 0)
	for _, name := range candidates {
		ids := channelIDs[name]
		if len(ids) == 0 {
			continue
		}
		if autoModelCooldownActive(group, name, ids) {
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
