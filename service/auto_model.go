package service

import (
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
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
// elapsedMs 是本次尝试的冷却信号：成功流式请求用首包延迟，非流式或失败用本次尝试耗时。
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
	autoModelHealth.Update(key, func(current autoModelOutcome, exists bool) autoModelOutcome {
		// Read the clock under the map lock, so a delayed writer cannot roll the
		// timestamp back or overwrite another request's observation.
		return updateAutoModelOutcome(current, exists, success, latencyMs, time.Now())
	})
	if modelOnlyCooldown {
		reason := strings.TrimSpace(cooldownReason)
		if reason == "" {
			reason = "响应无有效内容"
		}
		tripAutoModelCooldown(group, name, channelID, reason, time.Now())
		return
	}
	// 慢或者失败进入冷却（落库，跨重启生效），正常速度的成功清除冷却。
	applyAutoModelCooldown(group, name, channelID, success, elapsedMs)
}

const autoModelMaxObservations = 20.0

// decayAutoModelOutcome folds all stale evidence toward a neutral cold start.
// Applying this before both reads and writes prevents fresh feedback from
// reviving an expired latency or an unbounded historical confidence count.
func decayAutoModelOutcome(outcome autoModelOutcome, now time.Time) autoModelOutcome {
	ageFactor := 1.0 - math.Max(0, float64(now.Sub(outcome.UpdatedAt).Milliseconds()))/float64(autoModelHalfLifeMs)
	ageFactor = math.Max(0, ageFactor)
	outcome.Observations = math.Min(autoModelMaxObservations, math.Max(0, outcome.Observations)) * ageFactor
	outcome.Score = 0.5 + (outcome.Score-0.5)*ageFactor
	outcome.LatencyMS *= ageFactor
	if outcome.Observations == 0 {
		outcome.Score = 0.5
		outcome.LatencyMS = 0
	}
	outcome.UpdatedAt = now
	return outcome
}

func updateAutoModelOutcome(current autoModelOutcome, exists, success bool, latencyMs int64, now time.Time) autoModelOutcome {
	if !exists {
		current = autoModelOutcome{Score: 0.5, UpdatedAt: now}
	}
	current = decayAutoModelOutcome(current, now)
	value := 0.0
	if success {
		value = 1
	}
	current.Score += autoModelScoreAlpha * (value - current.Score)
	current.Observations = math.Min(autoModelMaxObservations, current.Observations+1)
	if latencyMs > 0 {
		if current.LatencyMS == 0 {
			current.LatencyMS = float64(latencyMs)
		} else {
			current.LatencyMS = current.LatencyMS*(1-autoModelLatencyAlpha) + float64(latencyMs)*autoModelLatencyAlpha
		}
	}
	return current
}

// effectiveScore applies synchronous evidence decay, confidence and latency.
// Fully expired or unobserved outcomes are exactly neutral, including latency.
func effectiveScore(outcome autoModelOutcome, now time.Time) float64 {
	outcome = decayAutoModelOutcome(outcome, now)
	confidence := outcome.Observations / (outcome.Observations + 3.0)
	score := 0.5*(1-confidence) + outcome.Score*confidence
	return score / (1.0 + outcome.LatencyMS/15000.0)
}

// GetAutoModelCandidates 返回某分组可参与 auto 路由的候选模型，按健康度降序。
// 候选 = 分组白名单 ∩ 分组启用模型 ∩ 已配置计费 ∩ chat 端点（未知端点不拦截）。
// 分数一致（含全部未观测的冷启动）的候选在同一档内随机排列，不再按列表顺序——
// 列表顺序与质量无关，谁排前面纯看数据库返回顺序。
func GetAutoModelCandidates(group string) []string {
	return getAutoModelCandidates(group, nil)
}

// GetAutoModelCandidatesForRequest applies authorization before cooldown filtering,
// so an unauthorized healthy model cannot suppress an authorized cooling model's
// existing all-cooling fallback.
func GetAutoModelCandidatesForRequest(c *gin.Context, group string) []string {
	return getAutoModelCandidates(group, func(name string) bool {
		return IsAutoModelCandidateAuthorized(c, name)
	})
}

func getAutoModelCandidates(group string, authorized func(string) bool) []string {
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
		if !availableSet[name] || (authorized != nil && !authorized(name)) {
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
	Priority     int64 // highest channel priority this model can use (for tiering, not global first-key)
	LatestChannelID int64 // channel used last time for this model; keeps preference if same channel and score band unchanged
	LatencyMS    float64
	Observations float64
}

// autoScoreTieEpsilon 是最高分档容差：档内按权重随机，档外严格按分数优先。
const autoScoreTieEpsilon = 0.02

func rankAutoModelCandidates(group string, candidates []string) []string {
	if len(candidates) == 0 {
		return candidates
	}
	targets, err := model.GetAutoModelRoutingTargets(group)
	if err != nil {
		// Keep availability fail-open, but never promote unavailable historical
		// channels when the current routing snapshot cannot be obtained.
		common.SysError("failed to load auto model routing targets: " + err.Error())
		return rankAutoModelCandidatesFromSnapshot(group, candidates, nil, nil, nil, time.Now())
	}
	return rankAutoModelCandidatesFromSnapshot(group, candidates, targets, autoModelHealth.ReadAll(), autoModelCooldownSnapshot(), time.Now())
}

func rankAutoModelCandidatesFromSnapshot(group string, candidates []string, targets map[string][]model.AutoModelRoutingTarget, health map[string]autoModelOutcome, cooldowns map[string]autoModelCooldownState, now time.Time) []string {
	allCooling := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		// Only check cooling on currently eligible targets (non-cooled channels).
		// A model is fully cooled only if all its non-cooled channels are unavailable.
		ids := make([]int, 0, len(targets[name]))
		for _, target := range targets[name] {
			key := autoModelHealthKey(group, name, target.ChannelID)
			if state, ok := cooldowns[key]; !ok || !state.Until.After(now) {
				ids = append(ids, target.ChannelID)
			}
		}
		allCooling[name] = autoModelCooldownActiveInSnapshot(group, name, ids, cooldowns, now)
	}
	candidates = excludeCoolingCandidates(candidates, allCooling)
	items := make([]candidateMeta, 0, len(candidates))
	for _, name := range candidates {
		eligible := make([]model.AutoModelRoutingTarget, 0, len(targets[name]))
		for _, target := range targets[name] {
			state := cooldowns[autoModelHealthKey(group, name, target.ChannelID)]
			if allCooling[name] || !state.Until.After(now) {
				eligible = append(eligible, target)
			}
		}
			items = append(items, candidateMeta{
				Group:           group,
				Name:            name,
				Score:           autoModelRoutingExpectedScore(group, name, eligible, health, now),
				Priority:        maxTargetPriority(eligible),
				LatestChannelID: lastUsedChannelID(group, name, eligible, cooldowns, now),
			})
	}
	items = orderByScoreTieBands(items)
	result := make([]string, len(items))
	for i, meta := range items {
		result[i] = meta.Name
	}
	return result
}

// autoModelRoutingExpectedScore mirrors initial channel selection: highest
// available priority (across all known channels for this model, not just non-cooled),
// then the highest score band among the non-cooled ones, then its weighted expectation.
func autoModelRoutingExpectedScore(group, name string, targets []model.AutoModelRoutingTarget, health map[string]autoModelOutcome, now time.Time) float64 {
	if len(targets) == 0 {
		return 0.5
	}
	// Highest priority across ALL targets for this model, including cooled channels.
	// This is what your rule requires: "next time comes in as same channel → compare
	// model scores inside this channel again"—the priority stays with the model, not
	// the channel snapshot.
	priority := targets[0].Priority
	for _, target := range targets {
		if target.Priority > priority {
			priority = target.Priority
		}
	}
	best := math.Inf(-1)
	scores := make(map[int]float64, len(targets))
	for _, target := range targets {
		if target.Priority != priority {
			continue
		}
		score := effectiveScore(health[autoModelHealthKey(group, name, target.ChannelID)], now)
		scores[target.ChannelID] = score
		best = math.Max(best, score)
	}
	weighted, totalWeight, unweighted, count := 0.0, 0.0, 0.0, 0.0
	for _, target := range targets {
		score := scores[target.ChannelID]
		if target.Priority != priority || score < best-autoScoreTieEpsilon {
			continue
		}
		weight := math.Max(0, target.Weight)
		weighted += score * weight
		totalWeight += weight
		unweighted += score
		count++
	}
	if totalWeight == 0 {
		return unweighted / count // memory selector's all-zero band smoothing
	}
	return weighted / totalWeight
}

// maxTargetPriority 取一组候选渠道里的最高优先级（与挑渠道时的定层口径一致）
// （priority 越大越优先）。
func maxTargetPriority(targets []model.AutoModelRoutingTarget) int64 {
	if len(targets) == 0 {
		return 0
	}
	priority := targets[0].Priority
	for _, target := range targets[1:] {
		if target.Priority > priority {
			priority = target.Priority
		}
	}
	return priority
}

// lastUsedChannelID 返回该模型上次在已选渠道中实际用的那个通道 ID。
// 如果所有候选都被冷却或不可用，返回 0（冷启动退化随机）。
func lastUsedChannelID(group, name string, eligible []model.AutoModelRoutingTarget, cooldowns map[string]autoModelCooldownState, now time.Time) int64 {
	if len(eligible) == 0 {
		return 0
	}
	// 简单近似：看本模型的冷却表里最早解除的那个通道，把它当作"最近可能还在用"的候选。
	// 更精确的做法需要读历史事件日志；这里只做一个保守信号：只要有一个通道没冷却且不是刚被禁用，就视为"继续优先"。
	for _, t := range eligible {
		key := autoModelHealthKey(group, name, t.ChannelID)
		state, ok := cooldowns[key]
		if !ok || !state.Until.After(now) {
			return int64(t.ChannelID)
		}
	}
	return 0
}

// orderByScoreTieBands 把候选排成"优先级从高到低 → 分数档从高到低 → 档内随机 → 冷启动保留输入顺序"的顺序。
//
// 严格对齐用户口述：
//   1) 先挑渠道：优先级高的直接用；优先级一致看渠道分，一致才加权随机挑渠道。
//   2) 再看这个渠道下的模型：模型得分一致就加权随机，不然就模型得分高的先用。
//   3) 关键：下次进来的时候还是这个渠道，然后判断该渠道下模型得分是一致还是不同；
//      不同则再次加权随机，如果有分数低的要排后面；冷却的直接跳过（调用方已过滤）。
//
// 这里的实现：用 candidateMeta.Priority（模型的最高优先级）作为第一排序关键字，
// 这样就能体现"渠道优先级是第一关键字"的规则，即使某个通道被冷却，只要模型历史上
// 有更高优先级的通道，它就会排在低优先级模型的上面。
func orderByScoreTieBands(items []candidateMeta) []candidateMeta {
	if len(items) <= 1 {
		return items
	}
	// Stable sort by priority (desc), then score (desc).
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Priority != items[j].Priority {
			return items[i].Priority > items[j].Priority
		}
		return items[i].Score > items[j].Score
	})
	bands := make([][]candidateMeta, 0, len(items))
	for _, item := range items {
		if n := len(bands); n > 0 && item.Priority == bands[n-1][0].Priority && item.Score >= bands[n-1][0].Score-autoScoreTieEpsilon {
			bands[n-1] = append(bands[n-1], item)
			continue
		}
		bands = append(bands, []candidateMeta{item})
	}
	result := make([]candidateMeta, 0, len(items))
	for _, band := range bands {
		if len(band) > 1 {
			// 同优先级 + 同分档内随机
			rand.Shuffle(len(band), func(i, j int) { band[i], band[j] = band[j], band[i] })
		}
		result = append(result, band...)
	}
	return result
}

