package service

import (
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// AutoModelScoreInfo 是“auto 路由状态”页面的一条只读明细。
// 粒度与证据一致 —— (分组, 模型, 渠道)。未观测的当前候选也会出现在列表里，
// 但不会伪造分数或延迟。
type AutoModelScoreInfo struct {
	Group            string  `json:"group"`
	Model            string  `json:"model"`
	ChannelID        int     `json:"channel_id"`
	Reliability      float64 `json:"reliability"`
	Observations     float64 `json:"observations"`
	ReliabilityKnown bool    `json:"reliability_known"`
	// Latency samples are the effective (decayed) counts the planner actually
	// evaluates, not the raw counters stored in memory.
	StreamLatencyMS           float64 `json:"stream_latency_ms"`
	StreamLatencySamples      float64 `json:"stream_latency_samples"`
	StreamLatencyKnown        bool    `json:"stream_latency_known"`
	StreamLatencyFresh        bool    `json:"stream_latency_fresh"`
	StreamLatencyUpdatedAt    int64   `json:"stream_latency_updated_at"`
	NonStreamLatencyMS        float64 `json:"non_stream_latency_ms"`
	NonStreamLatencySamples   float64 `json:"non_stream_latency_samples"`
	NonStreamLatencyKnown     bool    `json:"non_stream_latency_known"`
	NonStreamLatencyFresh     bool    `json:"non_stream_latency_fresh"`
	NonStreamLatencyUpdatedAt int64   `json:"non_stream_latency_updated_at"`
	LastObservedAt            int64   `json:"last_observed_at"`
	Cooling                   bool    `json:"cooling"`
	CooldownLevel             int     `json:"cooldown_level"`
	PendingRecovery           bool    `json:"pending_recovery"`
	ChannelPriority           *int64  `json:"channel_priority"`
	Category                  string  `json:"category"`
	Reason                    string  `json:"reason"`
}

// listAutoModelRouteStatus 是页面与测试共用的只读快照。targets 包含未观测
// 候选；health 只提供证据；cooldowns 只提供时间状态。
func listAutoModelRouteStatus(now time.Time, stream bool) ([]AutoModelScoreInfo, error) {
	targets, err := model.GetAllAutoModelRoutingTargets()
	if err != nil {
		return nil, err
	}
	health := autoModelHealth.ReadAll()
	cooldowns := autoModelCooldownSnapshot()
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	modelWeights := operation_setting.GetAutoModelWeights()
	candidatesConfig := operation_setting.GetAutoModelCandidates()
	mode := autoModelRequestNonStream
	if stream {
		mode = autoModelRequestStream
	}
	result := make([]AutoModelScoreInfo, 0)
	for group, models := range targets {
		// Preview classification and displayed rows must read the exact same
		// targets, health, cooldowns, time and configuration snapshot.
		previewSnapshot := autoModelRouteSnapshot{
			Targets: models, ModelHealth: health, Cooldowns: cooldowns,
			ModelWeights: modelWeights, Candidates: candidatesConfig,
			Mode: mode, Now: now, Settings: settings,
		}
		preview, _ := buildAutoModelGroupPlan(group, nil, previewSnapshot, false, rand.Float64)
		previewCategories := preview.Meta.Categories
		previewReasons := preview.Meta.Reasons
		allowed, configured := candidatesConfig[group]
		allowedSet := make(map[string]bool, len(allowed))
		for _, name := range allowed {
			allowedSet[strings.TrimSpace(name)] = true
		}
		for name, routes := range models {
			if configured && (len(allowed) == 0 || !allowedSet[strings.TrimSpace(name)]) {
				continue
			}
			if !positiveAutoModelWeight(autoModelRouteWeight(group, name, modelWeights)) ||
				!helper.HasModelBillingConfig(name) ||
				!autoModelSupportsChat(model.GetModelSupportEndpointTypes(name)) {
				continue
			}
			for _, target := range routes {
				key := autoModelHealthKey(group, name, target.ChannelID)
				outcome := health[key]
				streamView := autoModelRouteEvidenceView(outcome, cooldowns[key], autoModelRequestStream, now, settings)
				nonStreamView := autoModelRouteEvidenceView(outcome, cooldowns[key], autoModelRequestNonStream, now, settings)
				view := nonStreamView
				if stream {
					view = streamView
				}
				priority := target.Priority
				lastObservedAt := int64(0)
				if !view.LastObservedAt.IsZero() {
					lastObservedAt = view.LastObservedAt.Unix()
				}
				info := AutoModelScoreInfo{
					Group: group, Model: name, ChannelID: target.ChannelID,
					Reliability: view.Reliability, Observations: view.Observations,
					ReliabilityKnown: view.ReliabilityKnown,
					StreamLatencyMS:  streamView.LatencyMS, StreamLatencySamples: streamView.LatencySamples,
					StreamLatencyKnown:     streamView.LatencyKnown,
					StreamLatencyFresh:     autoModelEvidenceFreshFor(streamView.LatencyUpdatedAt, now, settings.ScoreDecayMinutes),
					StreamLatencyUpdatedAt: unixSecondsOrZero(streamView.LatencyUpdatedAt),
					NonStreamLatencyMS:     nonStreamView.LatencyMS, NonStreamLatencySamples: nonStreamView.LatencySamples,
					NonStreamLatencyKnown:     nonStreamView.LatencyKnown,
					NonStreamLatencyFresh:     autoModelEvidenceFreshFor(nonStreamView.LatencyUpdatedAt, now, settings.ScoreDecayMinutes),
					NonStreamLatencyUpdatedAt: unixSecondsOrZero(nonStreamView.LatencyUpdatedAt),
					LastObservedAt:            lastObservedAt,
					PendingRecovery:           view.PendingRecovery,
					ChannelPriority:           &priority,
				}
				if state, ok := cooldowns[key]; ok && state.Until.After(now) {
					info.Cooling = true
					info.CooldownLevel = state.Level
				}
				route := AutoModelRoute{Group: group, ModelName: name, ChannelID: target.ChannelID}
				if category, ok := previewCategories[route]; ok {
					info.Category, info.Reason = category, previewReasons[route]
				} else {
					info.Category, info.Reason = autoModelRouteStatusCategory(view, info.Cooling, settings)
				}
				result = append(result, info)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Group != result[j].Group {
			return result[i].Group < result[j].Group
		}
		if result[i].Model != result[j].Model {
			return result[i].Model < result[j].Model
		}
		return result[i].ChannelID < result[j].ChannelID
	})
	return result, nil
}

func unixSecondsOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.Unix()
}

func autoModelRouteStatusCategory(view autoModelEvidenceView, cooling bool, settings operation_setting.AutoModelRoutingSettings) (string, string) {
	if cooling {
		return "cooling", "冷却中"
	}
	if view.PendingRecovery {
		return "pending_recovery", "待恢复：已有失败，仅作最后后备，不参与探索"
	}
	if view.Observations == 0 {
		return "unobserved", "未观察"
	}
	if !view.ReliabilityKnown {
		if view.Observations < float64(settings.MinObservations) {
			return "insufficient", "业务样本不足"
		}
		return "stale", "证据陈旧"
	}
	if view.Reliability < settings.ReliabilityThreshold {
		return "unreliable", "可靠性不足"
	}
	if !view.LatencyKnown {
		if view.LatencySamples < autoModelMinLatencySamples {
			return "latency_insufficient", "成功延迟样本不足"
		}
		return "latency_stale", "成功延迟证据陈旧"
	}
	return "reliable", "可靠候选"
}

// ListAutoModelScores 返回当前所有候选及其证据，供“auto 路由状态”页面展示。
// 读取失败必须显式返回给调用方，不能伪装成空列表。
func ListAutoModelScores(now time.Time, stream bool) ([]AutoModelScoreInfo, error) {
	return listAutoModelRouteStatus(now, stream)
}

// ClearAutoModelNegativeEffects 清空某个 (分组, 模型, 渠道) 的业务证据并解除冷却。
// 不生成成功样本，不把清除时间伪装成业务观测时间；下次真实请求从待观察重新学习。
// 返回值第二个参数表示数据库持久化是否失败: 内存清除仍然生效，并由后台重试墓碑写入。
func ClearAutoModelNegativeEffects(group, name string, channelID int) (bool, error) {
	return clearAutoModelNegativeEffectsWithDB(model.DB, group, name, channelID)
}
