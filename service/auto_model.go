package service

import (
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
)

// autoModelOutcome 是一个 (分组, 模型) 的被动健康摘要。
// 数据仅来自真实请求的成功/失败反馈，绝不发送主动测活请求。
type autoModelOutcome struct {
	Score        float64
	LatencyMS    float64
	Observations float64
	UpdatedAt    time.Time
}

var autoModelHealth = types.NewRWMap[string, autoModelOutcome]()

// autoModelHalfLifeMs 用于读取时的评分时间衰减（线性衰减至 10 分钟）。
const autoModelHalfLifeMs = 600000

func autoModelHealthKey(group, name string) string {
	return group + "\x00" + name
}

// RecordAutoModelOutcome 记录一次已观察到的请求结果（被动反馈）。
// latencyMs <= 0 表示未采集到延迟。
func RecordAutoModelOutcome(group, name string, success bool, latencyMs int64) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || operation_setting.IsAutoModelName(name) {
		return
	}
	key := autoModelHealthKey(group, name)
	current, ok := autoModelHealth.Get(key)
	if !ok {
		current = autoModelOutcome{Score: 0.5, Observations: 0, LatencyMS: 0, UpdatedAt: time.Now()}
	}
	value := 0.0
	if success {
		value = 1
	}
	current.Score = (current.Score*current.Observations + value) / (current.Observations + 1)
	current.Observations = current.Observations + 1
	if latencyMs > 0 {
		if current.LatencyMS == 0 {
			current.LatencyMS = float64(latencyMs)
		} else {
			current.LatencyMS = current.LatencyMS*0.8 + float64(latencyMs)*0.2
		}
	}
	current.UpdatedAt = time.Now()
	autoModelHealth.Set(key, current)
}

// GetAutoModelCandidates 返回某分组可参与 auto 路由的候选模型，按健康度降序。
// 候选 = 分组白名单 ∩ 分组启用模型 ∩ 已配置计费 ∩ chat 端点（未知端点不拦截）。
// 冷启动（无观测）时按白名单顺序和模型可见顺序排序。
func GetAutoModelCandidates(group string) []string {
	available := model.GetGroupEnabledModels(group)
	availableSet := make(map[string]bool, len(available))
	for _, name := range available {
		availableSet[name] = true
	}
	configured := operation_setting.GetAutoModelCandidates()
	allowed, hasConfigured := configured[group]
	if !hasConfigured {
		allowed = available
	}
	result := make([]string, 0, len(allowed))
	seen := make(map[string]bool)
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name == "" || operation_setting.IsAutoModelName(name) || seen[name] {
			continue
		}
		seen[name] = true
		if !availableSet[name] {
			continue
		}
		if !helper.HasModelBillingConfig(name) {
			continue
		}
		endpoints := model.GetModelSupportEndpointTypes(name)
		if len(endpoints) > 0 {
			chatSupported := false
			for _, endpoint := range endpoints {
				if endpoint == constant.EndpointTypeOpenAI || endpoint == constant.EndpointTypeOpenAIResponse {
					chatSupported = true
					break
				}
			}
			if !chatSupported {
				continue
			}
		}
		result = append(result, name)
	}
	return rankAutoModelCandidates(group, result)
}

type candidateMeta struct {
	Group        string
	Name         string
	Score        float64
	LatencyMS    float64
	Observations float64
	Order        int
}

func rankAutoModelCandidates(group string, candidates []string) []string {
	health := autoModelHealth.ReadAll()
	now := time.Now()
	items := make([]candidateMeta, 0, len(candidates))
	for i, name := range candidates {
		meta := candidateMeta{Group: group, Name: name, Score: 0.5, Order: i}
		outcome, ok := health[autoModelHealthKey(group, name)]
		if ok {
			ageMs := now.Sub(outcome.UpdatedAt).Milliseconds()
			ageFactor := 1.0
			if ageMs > 0 {
				ageFactor = 1.0 - float64(ageMs)/float64(autoModelHalfLifeMs)
				if ageFactor < 0 {
					ageFactor = 0
				}
			}
			effectiveObs := outcome.Observations * ageFactor
			confidence := effectiveObs / (effectiveObs + 3.0)
			meta.Score = 0.5*(1-confidence) + outcome.Score*confidence
			meta.LatencyMS = outcome.LatencyMS
			if meta.LatencyMS > 0 {
				meta.Score = meta.Score / (1.0 + meta.LatencyMS/15000.0)
			}
		}
		items = append(items, meta)
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Score > items[j].Score+0.00001 {
			return true
		}
		if items[i].Score < items[j].Score-0.00001 {
			return false
		}
		if items[i].LatencyMS > 0 && items[j].LatencyMS > 0 && items[i].LatencyMS != items[j].LatencyMS {
			return items[i].LatencyMS < items[j].LatencyMS
		}
		return items[i].Order < items[j].Order
	})
	result := make([]string, len(items))
	for i, meta := range items {
		result[i] = meta.Name
	}
	return result
}
