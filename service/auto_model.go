package service

import (
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
)

// autoModelOutcome 是一个 (分组，模型，渠道) 的被动健康摘要。
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

// autoModelAlphaEWMA 是延迟 EWMA 平滑系数，沿用现有的 0.2 / 0.8。
const autoModelLatencyAlpha = 0.2

// autoModelScoreAlpha 是成功率的指数加权移动平均系数：新样本权重 30%，
// 让一条渠道变坏时分数快速下跌，而不是被历史大量成功拖住。
const autoModelScoreAlpha = 0.30

func autoModelHealthKey(group, name string, channelID int) string {
	return group + "\x00" + name + "\x00" + strconv.Itoa(channelID)
}

func autoModelHealthPrefix(group, name string) string {
	return group + "\x00" + name + "\x00"
}

// RecordAutoModelOutcome 记录一次已观察到的请求结果（被动反馈）。
// 记分粒度为 分组 + 模型 + 渠道，latencyMs <= 0 表示未采集到延迟。
// elapsedMs 是本次请求的总耗时，用于判断这次是否算"慢"（失败请求的 latencyMs 为 0，但耗时可能很长）。
func RecordAutoModelOutcome(group, name string, channelID int, success bool, latencyMs int64, elapsedMs int64) {
	recordAutoModelOutcome(group, name, channelID, success, latencyMs, elapsedMs, false, "")
}

// RecordAutoModelUnusableAnswer 记录一次"HTTP 200 但没有有效回答"：
// 正文为空，或正文只是上游网关塞进来的告警横幅（实测样本见 relay/common/answer_observation.go）。
//
// 与普通失败的区别在冷却范围：只让"该渠道下的该模型"进冷却，不连累该上游的其它模型。
// 理由是证据只覆盖这一个组合——上游对这个模型给了横幅，并不能说明它别的模型也不行。
// 冷却整体上就是 (模型, 渠道) 粒度的，不存在"整条渠道避让"这一层。
func RecordAutoModelUnusableAnswer(group, name string, channelID int, reason string) {
	recordAutoModelOutcome(group, name, channelID, false, 0, 0, true, reason)
}

func recordAutoModelOutcome(group, name string, channelID int, success bool, latencyMs int64, elapsedMs int64, modelOnlyCooldown bool, cooldownReason string) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 || operation_setting.IsAutoModelName(name) {
		return
	}
	key := autoModelHealthKey(group, name, channelID)
	current, ok := autoModelHealth.Get(key)
	if !ok {
		current = autoModelOutcome{Score: 0.5, Observations: 0, LatencyMS: 0, UpdatedAt: time.Now()}
	}
	value := 0.0
	if success {
		value = 1
	}
	// EWMA：成功率向本次结果移动固定比例，近期表现占主导。
	current.Score += autoModelScoreAlpha * (value - current.Score)
	current.Observations = current.Observations + 1
	if latencyMs > 0 {
		if current.LatencyMS == 0 {
			current.LatencyMS = float64(latencyMs)
		} else {
			current.LatencyMS = current.LatencyMS*(1-autoModelLatencyAlpha) + float64(latencyMs)*autoModelLatencyAlpha
		}
	}
	current.UpdatedAt = time.Now()
	autoModelHealth.Set(key, current)
	if modelOnlyCooldown {
		reason := strings.TrimSpace(cooldownReason)
		if reason == "" {
			reason = "响应无有效内容"
		}
		ensureAutoModelCooldownsLoaded()
		tripAutoModelCooldown(group, name, channelID, reason, time.Now())
		return
	}
	// 慢或者失败进入冷却（落库，跨重启生效），正常速度的成功清除冷却。
	applyAutoModelCooldown(group, name, channelID, success, elapsedMs)
}

// effectiveScore 将观测折算为可用分数：
// 时间衰减 -> 置信度压缩 -> 延迟惩罚。无观测时返回中性 0.5。
func effectiveScore(outcome autoModelOutcome, now time.Time) float64 {
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
	score := 0.5*(1-confidence) + outcome.Score*confidence
	if outcome.LatencyMS > 0 {
		score = score / (1.0 + outcome.LatencyMS/15000.0)
	}
	return score
}

// GetAutoModelCandidates 返回某分组可参与 auto 路由的候选模型，按健康度降序。
// 候选 = 分组白名单 ∩ 分组启用模型 ∩ 已配置计费 ∩ chat 端点（未知端点不拦截）。
// 分数一致（含全部未观测的冷启动）的候选在同一档内随机排列，不再按列表顺序——
// 列表顺序与质量无关，谁排前面纯看数据库返回顺序。
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
	// 所有渠道都在冷却中的模型先剔除（见 auto_model_cooldown.go）。
	result = excludeCoolingAutoModelCandidates(group, result)
	return rankAutoModelCandidates(group, result)
}

type candidateMeta struct {
	Group        string
	Name         string
	Score        float64
	LatencyMS    float64
	Observations float64
}

// autoScoreTieEpsilon 视为"分数并列"的容差。EMA 分数一次观测至少移动 0.1
// （alpha 0.2，冷启动 0.5），所以 0.02 只吃掉"真实质量相当"的抖动，
// 不会把明显更好的候选拉平。他的口径：分数一致就加权随机，不一致才按分数优先。
const autoScoreTieEpsilon = 0.02

func rankAutoModelCandidates(group string, candidates []string) []string {
	health := autoModelHealth.ReadAll()
	now := time.Now()
	items := make([]candidateMeta, 0, len(candidates))
	for _, name := range candidates {
		meta := candidateMeta{Group: group, Name: name, Score: 0.5}
		prefix := autoModelHealthPrefix(group, name)
		bestObs := autoModelOutcome{}
		found := false
		for key, outcome := range health {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			if !found || outcome.Observations > bestObs.Observations {
				bestObs = outcome
				found = true
			}
		}
		if found {
			meta.Score = effectiveScore(bestObs, now)
			meta.LatencyMS = bestObs.LatencyMS
			meta.Observations = bestObs.Observations
		}
		items = append(items, meta)
	}
	items = orderByScoreTieBands(items)
	result := make([]string, len(items))
	for i, meta := range items {
		result[i] = meta.Name
	}
	return result
}

// orderByScoreTieBands 把候选排成"分数档从高到低、档内随机"的顺序。
//
// 档内随机取代了原来的列表顺序兜底：那份顺序来自数据库/白名单，与实际质量无关，
// 冷启动（所有候选都还是中性 0.5）时等于每次挑数据库里排第一的那个——实测就是
// 排队几十秒的渠道 1。模型本身没有权重字段，所以档内等权随机；渠道权重在挑渠道时生效。
func orderByScoreTieBands(items []candidateMeta) []candidateMeta {
	if len(items) <= 1 {
		return items
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Score > items[j].Score })
	bands := make([][]candidateMeta, 0, len(items))
	for _, item := range items {
		if n := len(bands); n > 0 && item.Score >= bands[n-1][0].Score-autoScoreTieEpsilon {
			bands[n-1] = append(bands[n-1], item)
			continue
		}
		bands = append(bands, []candidateMeta{item})
	}
	result := make([]candidateMeta, 0, len(items))
	for _, band := range bands {
		if len(band) > 1 {
			rand.Shuffle(len(band), func(i, j int) { band[i], band[j] = band[j], band[i] })
		}
		result = append(result, band...)
	}
	return result
}

