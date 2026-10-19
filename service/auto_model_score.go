package service

import (
	"sort"
	"time"
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
	Permanent     bool    `json:"permanent"`
}

// ListAutoModelScores 返回所有已观察到反馈的 (分组, 模型, 渠道) 得分明细,
// 按当前有效得分从高到低排序;附带冷却状态便于排查。
func ListAutoModelScores(now time.Time) []AutoModelScoreInfo {
	health := autoModelHealth.ReadAll()
	cooldowns := autoModelCooldownSnapshot()
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
			info.Permanent = state.Permanent
		}
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Score > result[j].Score
	})
	return result
}