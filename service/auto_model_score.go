package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// AutoModelScoreInfo 是"模型得分排序"页面的一条只读明细。
// 粒度与健康体系一致 —— (分组, 模型, 渠道),同一个模型名在不同
// 分组/渠道下各有独立得分。
type AutoModelScoreInfo struct {
	Group         string  `json:"group"`
	Model         string  `json:"model"`
	ChannelID     int     `json:"channel_id"`
	Score         float64 `json:"score"`
	LatencyMS     float64 `json:"latency_ms"`
	Observations  float64 `json:"observations"`
	UpdatedAt     int64   `json:"updated_at"`
	Cooling       bool    `json:"cooling"`
	CooldownLevel int     `json:"cooldown_level"`
	// ChannelPriority 是渠道的 priority(auto 排序的第一硬边界,降序分层)。
	// 取自路由目标快照;渠道或能力已被删除时为 null,前端显示 '-'。
	ChannelPriority *int64 `json:"channel_priority"`
}

// ListAutoModelScores 返回所有已观察到反馈的 (分组, 模型, 渠道) 得分明细,
// 按当前有效得分从高到低排序;附带冷却状态便于排查。
func ListAutoModelScores(now time.Time) []AutoModelScoreInfo {
	health := autoModelHealth.ReadAll()
	cooldowns := autoModelCooldownSnapshot()
	// 渠道 priority 从路由目标快照取(命中 2 秒缓存);渠道/能力已删除的行拿不到,
	// 保持 null。
	channelPriorities := map[string]map[int]int64{}
	for key := range health {
		if group, _, _, ok := parseAutoModelHealthKey(key); ok {
			if _, loaded := channelPriorities[group]; loaded {
				continue
			}
			byChannel := map[int]int64{}
			if targets, err := model.GetAutoModelRoutingTargets(group); err == nil {
				for _, list := range targets {
					for _, target := range list {
						byChannel[target.ChannelID] = target.Priority
					}
				}
			}
			channelPriorities[group] = byChannel
		}
	}
	result := make([]AutoModelScoreInfo, 0, len(health))
	for key, outcome := range health {
		group, name, channelID, ok := parseAutoModelHealthKey(key)
		if !ok {
			continue
		}
		info := AutoModelScoreInfo{
			Group:        group,
			Model:        name,
			ChannelID:    channelID,
			Score:        effectiveScore(outcome, now),
			LatencyMS:    outcome.LatencyMS,
			Observations: outcome.Observations,
			UpdatedAt:    outcome.UpdatedAt.Unix(),
		}
		if state, ok := cooldowns[key]; ok && state.Until.After(now) {
			info.Cooling = true
			info.CooldownLevel = state.Level
		}
		if byChannel, ok := channelPriorities[group]; ok {
			if priority, ok := byChannel[channelID]; ok {
				info.ChannelPriority = &priority
			}
		}
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Score > result[j].Score
	})
	return result
}

// ClearAutoModelNegativeEffects 一键清除某个 (分组, 模型, 渠道) 组合的负面效果:
// 得分恢复中立 0.5,延迟惩罚清零,冷却解除。观测次数保留——它只表达置信度,
// 得分回中后不再携带任何偏差。组合没有观测数据时本就是中立,返回 false 表示无事可做。
//
// 粒度与健康体系一致:证据本来就按 (分组, 模型, 渠道) 记录,
// 清除范围与这一行完全重合,不会影响同渠道其它模型的证据。
//
// 持久化无需特殊处理:快照按固定周期把重置后的值 upsert 覆盖旧行,重启后
// restore 读到的就是中性值(restore 只合并内存缺失的键,不覆盖在线值)。
func ClearAutoModelNegativeEffects(group, name string, channelID int) bool {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return false
	}
	key := autoModelHealthKey(group, name, channelID)
	now := time.Now()
	cleared := false
	// Get + Set 而不是 Update:RWMap.Update 对不存在的键也会写入返回值,
	// 那会在没有观测数据时凭空造出一条零分记录。
	autoModelScoreMutex.Lock()
	if current, exists := autoModelHealth.Get(key); exists {
		autoModelHealth.Set(key, autoModelOutcome{Score: 0.5, Observations: current.Observations, UpdatedAt: now})
		cleared = true
	}
	autoModelScoreMutex.Unlock()
	if cleared {
		clearAutoModelCooldown(group, name, channelID, now)
		common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)的负面效果已清除,得分恢复中立 0.5", name, channelID))
	}
	return cleared
}