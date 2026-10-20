package service

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
)

// AutoModelRoute pins both halves of an auto dispatch. A retry advances through
// concrete routes, rather than choosing a model and independently redrawing its
// channel. Equal-priority routes interleave across channels by model score:
// a channel's remaining models never form an impenetrable block.
type AutoModelRoute struct {
	Group     string
	ModelName string
	ChannelID int
}

const autoModelRoutePlanContextKey = "auto_model_route_plan"
const autoModelFailedModelsContextKey = "auto_model_failed_models"

func autoModelFailedModelKey(group, modelName string) string {
	return strings.TrimSpace(group) + "\x00" + strings.TrimSpace(modelName)
}

// MarkAutoModelModelFailed records a model-scoped retry failure for this request.
// The exclusion is intentionally request-local and group-scoped: persistent
// cooldowns remain per (group, model, channel), while a model-scoped upstream
// error must not send this request to the same model on another channel.
func MarkAutoModelModelFailed(c *gin.Context, group, modelName string) {
	group = strings.TrimSpace(group)
	modelName = strings.TrimSpace(modelName)
	if c == nil || group == "" || modelName == "" {
		return
	}
	excluded := getAutoModelFailedModels(c)
	excluded[autoModelFailedModelKey(group, modelName)] = true
	c.Set(autoModelFailedModelsContextKey, excluded)
}

// IsAutoModelModelFailed reports whether this request has already seen a
// model-scoped failure for the concrete group/model pair.
func IsAutoModelModelFailed(c *gin.Context, group, modelName string) bool {
	group = strings.TrimSpace(group)
	modelName = strings.TrimSpace(modelName)
	if c == nil || group == "" || modelName == "" {
		return false
	}
	return getAutoModelFailedModels(c)[autoModelFailedModelKey(group, modelName)]
}

func getAutoModelFailedModels(c *gin.Context) map[string]bool {
	result := make(map[string]bool)
	if c == nil {
		return result
	}
	value, _ := c.Get(autoModelFailedModelsContextKey)
	source, _ := value.(map[string]bool)
	for key, failed := range source {
		if failed {
			result[key] = true
		}
	}
	return result
}

// This tolerance absorbs floating-point noise only. It applies at the band
// boundary (so the documented 0.900/0.890 example stays one band despite
// 0.900-0.890 being 0.0100...9 in float64); whether genuinely different scores
// share a band is decided by AutoModelScoreBandWidth, not by this constant.
const autoModelRouteScoreTieEpsilon = 1e-9

// ErrAutoModelNoAuthorizedCandidates distinguishes a token whitelist rejection
// from exhausted routes. Authorized but cooling candidates never produce it.
var ErrAutoModelNoAuthorizedCandidates = errors.New("no authorized auto model candidates")

// SetAutoModelRoutePlan owns a copy of the request's plan and activates its first
// route. The legacy model-name slice deliberately has one entry per route, so
// names may repeat on different channels. An empty plan invalidates the current
// route without erasing the original group used in error reporting.
func SetAutoModelRoutePlan(c *gin.Context, routes []AutoModelRoute) {
	if c == nil {
		return
	}
	plan := append([]AutoModelRoute(nil), routes...)
	c.Set(autoModelRoutePlanContextKey, plan)
	names := make([]string, len(plan))
	for i, route := range plan {
		names[i] = route.ModelName
	}
	common.SetContextKey(c, constant.ContextKeyAutoModelCandidates, names)
	common.SetContextKey(c, constant.ContextKeyAutoModelIndex, -1)
	ActivateAutoModelRoute(c, 0)
}

// GetAutoModelRoutePlan returns a copy; callers cannot reorder a live plan by
// mutating the returned slice.
func GetAutoModelRoutePlan(c *gin.Context) []AutoModelRoute {
	return append([]AutoModelRoute(nil), autoModelRoutePlan(c)...)
}

func autoModelRoutePlan(c *gin.Context) []AutoModelRoute {
	if c == nil {
		return nil
	}
	value, _ := c.Get(autoModelRoutePlanContextKey)
	plan, _ := value.([]AutoModelRoute)
	return plan
}

// GetAutoModelRouteIndex returns -1 when no route is active. The legacy index is
// the single source of truth, avoiding separate route/model indices drifting.
func GetAutoModelRouteIndex(c *gin.Context) int {
	plan := autoModelRoutePlan(c)
	if len(plan) == 0 {
		return -1
	}
	value, _ := common.GetContextKey(c, constant.ContextKeyAutoModelIndex)
	index, ok := value.(int)
	if !ok || index < 0 || index >= len(plan) {
		return -1
	}
	return index
}

func GetCurrentAutoModelRoute(c *gin.Context) (AutoModelRoute, bool) {
	index := GetAutoModelRouteIndex(c)
	if index < 0 {
		return AutoModelRoute{}, false
	}
	return autoModelRoutePlan(c)[index], true
}

// ActivateAutoModelRoute updates only route/group bookkeeping. Channel setup
// owns original_model and all channel credentials; planning never fabricates a
// selected channel or rewrites the token's original group.
func ActivateAutoModelRoute(c *gin.Context, index int) bool {
	plan := autoModelRoutePlan(c)
	if index < 0 || index >= len(plan) {
		return false
	}
	route := plan[index]
	common.SetContextKey(c, constant.ContextKeyAutoModelIndex, index)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, route.Group)
	if common.GetContextKeyString(c, constant.ContextKeyTokenGroup) == "auto" {
		common.SetContextKey(c, constant.ContextKeyAutoGroup, route.Group)
	}
	return true
}

type autoModelRouteSnapshot struct {
	Targets          map[string][]model.AutoModelRoutingTarget
	ModelHealth      map[string]autoModelOutcome
	Cooldowns        map[string]autoModelCooldownState
	ModelWeights     map[string]map[string]float64
	ExcludedChannels map[int]bool
	// ExcludedRoutes drops concrete (分组, 模型, 渠道) 三元组,粒度比
	// ExcludedChannels 细:渠道级排除是"这条渠道整个不能用了",这里排除的是
	// "本次请求已经打过的那一条路"。重试时用它,让新计划天然从"还没试过的最优路"开头。
	// nil 表示不额外排除任何路由。
	ExcludedRoutes map[AutoModelRoute]bool
	// ExcludedModels drops all routes for a request-scoped model failure in
	// the concrete group, without changing persistent cooldown granularity.
	ExcludedModels map[string]bool
	Now            time.Time
}

func newAutoModelRouteSnapshot(excluded map[int]bool) autoModelRouteSnapshot {
	return autoModelRouteSnapshot{
		ModelHealth:      autoModelHealth.ReadAll(),
		Cooldowns:        autoModelCooldownSnapshot(),
		ModelWeights:     operation_setting.GetAutoModelWeights(),
		ExcludedChannels: excluded,
		Now:              time.Now(),
	}
}

// BuildAutoModelRoutesForRequest takes fresh snapshots for every request. The
// order is group order, then eligibility filters, then channel priority in
// descending order as a hard boundary, then model effective-score bands
// (AutoModelScoreBandWidth) inside each priority tier, then within-band weighted
// sampling that preserves each channel's share. Channel health takes no part in
// ordering, banding or cold-start handling — that chain has been removed
// entirely. Cooldown and
// hard exclusions remove routes before any tier is selected; they never fail open.
//
// For an auto token group, existing group authorization determines the ordered
// concrete groups. With cross-group retry disabled only the first nonempty
// group's plan is retained. A concrete group has already been authorized by
// middleware, including a permitted playground group override.
func BuildAutoModelRoutesForRequest(c *gin.Context, group string) ([]AutoModelRoute, error) {
	return buildAutoModelRoutesForRequest(c, group, nil)
}

// RebuildAutoModelRoutePlanForRetry re-derives the ordered plan for the same
// request group from a fresh health snapshot, dropping every route in attempted
// and every model marked failed by a model-scoped retry decision.
//
// Why a retry must re-plan rather than advance the plan it was given: the plan is
// ordered from the health visible when middleware ran. Any feedback produced by
// other requests since then is invisible to that frozen list, so index+1 can walk
// straight into a route that is currently the worst one available while the best
// one sits further down — and with AutoModelMaxAttempts at 4, "further down" is
// often unreachable.
//
// This is a re-ordering of the still-eligible route set: every eligible 模型×渠道
// pair appears at most once, while attempted routes, request-failed models, cooling
// routes and hard-excluded channels are removed. A model-scoped failure therefore
// cannot return through another channel during the same request.
//
// It returns nil when the plan cannot be rebuilt — auto disabled, no request group
// recorded, nothing attempted yet, or nothing left to try — and callers must then
// keep the existing plan. ErrAutoModelNoAuthorizedCandidates is reported the same
// way: an empty rebuild is a signal to fall back, not a failure to surface to the
// client.
//
// 空 attempted 直接返回 nil,不做"纯重排"。两个理由:
//  1. 没有已试路由时,冻结计划是几毫秒前刚按同一份健康快照排出来的,重排只会得到
//     同一批候选的另一次随机抽取,没有信息增量。
//  2. 更要紧的是终止性。重排后调用方要从队首(index 0)重新走,而调用方在
//     "渠道在派发前就不可用"这条路径上不会标记 attempted;若空集也重排,那个循环
//     就失去了"索引单调递增"这个原有的上界保证。空集返回 nil 让调用方退回
//     index+1,把这条路径的终止性原样保留。
func RebuildAutoModelRoutePlanForRetry(c *gin.Context, group string, attempted map[AutoModelRoute]bool) []AutoModelRoute {
	if c == nil || len(attempted) == 0 {
		return nil
	}
	routes, err := buildAutoModelRoutesForRequest(c, group, attempted)
	if err != nil && !errors.Is(err, ErrAutoModelNoAuthorizedCandidates) {
		common.SysError("failed to rebuild auto model routes for retry: " + err.Error())
		return nil
	}
	return routes
}

// ReplaceAutoModelRoutePlan swaps the plan in place without activating a route.
//
// The active index is invalidated so no caller can report the replaced plan's head
// as "current" before a bind actually committed. Group bookkeeping is deliberately
// left alone: only a successful bind may move the request to another group, and a
// rebuild that turns out unusable must not have moved it already.
func ReplaceAutoModelRoutePlan(c *gin.Context, routes []AutoModelRoute) {
	if c == nil {
		return
	}
	plan := append([]AutoModelRoute(nil), routes...)
	c.Set(autoModelRoutePlanContextKey, plan)
	names := make([]string, len(plan))
	for i, route := range plan {
		names[i] = route.ModelName
	}
	common.SetContextKey(c, constant.ContextKeyAutoModelCandidates, names)
	common.SetContextKey(c, constant.ContextKeyAutoModelIndex, -1)
}

func buildAutoModelRoutesForRequest(c *gin.Context, group string, exclude map[AutoModelRoute]bool) ([]AutoModelRoute, error) {
	group = strings.TrimSpace(group)
	if c == nil || !operation_setting.AutoModelEnabled || group == "" {
		return nil, nil
	}
	groups := []string{group}
	crossGroupRetry := false
	if group == "auto" {
		userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
		groups = GetRequestGroupCandidates(c, userGroup, group)
		crossGroupRetry = common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry)
	}
	snapshot := newAutoModelRouteSnapshot(GetAutoModelHardExcludedChannelIDs(c))
	snapshot.ExcludedRoutes = exclude
	snapshot.ExcludedModels = getAutoModelFailedModels(c)
	var routes []AutoModelRoute
	hasCandidates, hasAuthorizedCandidates := false, false
	seenGroups := make(map[string]bool, len(groups))
	for _, candidateGroup := range groups {
		candidateGroup = strings.TrimSpace(candidateGroup)
		if candidateGroup == "" || candidateGroup == "auto" || seenGroups[candidateGroup] {
			continue
		}
		seenGroups[candidateGroup] = true
		result, err := buildAutoModelRoutesForGroup(candidateGroup, func(name string) bool {
			return IsAutoModelCandidateAuthorized(c, name)
		}, snapshot)
		if err != nil {
			return nil, err
		}
		hasCandidates = hasCandidates || result.HasCandidates
		hasAuthorizedCandidates = hasAuthorizedCandidates || result.HasAuthorizedCandidates
		routes = append(routes, result.Routes...)
		if len(routes) > 0 && !crossGroupRetry {
			break
		}
	}
	if !hasAuthorizedCandidates && hasCandidates {
		return nil, ErrAutoModelNoAuthorizedCandidates
	}
	return routes, nil
}

type autoModelGroupRouteResult struct {
	Routes                  []AutoModelRoute
	HasCandidates           bool
	HasAuthorizedCandidates bool
}

func buildAutoModelRoutesForGroup(group string, authorized func(string) bool, snapshot autoModelRouteSnapshot) (autoModelGroupRouteResult, error) {
	result := autoModelGroupRouteResult{}
	targets, err := model.GetAutoModelRoutingTargets(group)
	if err != nil {
		return result, err
	}
	snapshot.Targets = targets
	candidates := eligibleAutoModelCandidates(group, snapshot)
	result.HasCandidates = len(candidates) > 0
	if authorized != nil {
		allowed := make([]string, 0, len(candidates))
		for _, name := range candidates {
			if authorized(name) {
				allowed = append(allowed, name)
			}
		}
		candidates = allowed
	}
	result.HasAuthorizedCandidates = len(candidates) > 0
	result.Routes = buildAutoModelRoutesFromSnapshot(group, candidates, snapshot, rand.Float64)
	return result, nil
}

// Eligibility comes from the same enabled channel/ability snapshot used by the
// planner, not a second model query or historical evidence. Empty allowlists and
// explicit zero weights intentionally disable every affected route.
func eligibleAutoModelCandidates(group string, snapshot autoModelRouteSnapshot) []string {
	allowed, configured := operation_setting.GetAutoModelCandidates()[group]
	if !configured {
		allowed = make([]string, 0, len(snapshot.Targets))
		for name := range snapshot.Targets {
			allowed = append(allowed, name)
		}
		sort.Strings(allowed)
	}
	var candidates []string
	seen := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name == "" || operation_setting.IsAutoModelName(name) || seen[name] {
			continue
		}
		seen[name] = true
		if len(snapshot.Targets[name]) == 0 || !positiveAutoModelWeight(autoModelRouteWeight(group, name, snapshot.ModelWeights)) || !helper.HasModelBillingConfig(name) {
			continue
		}
		if !autoModelSupportsChat(model.GetModelSupportEndpointTypes(name)) {
			continue
		}
		candidates = append(candidates, name)
	}
	return candidates
}

func autoModelSupportsChat(endpoints []constant.EndpointType) bool {
	if len(endpoints) == 0 {
		return true // preserve compatibility with models lacking endpoint metadata
	}
	for _, endpoint := range endpoints {
		if endpoint == constant.EndpointTypeOpenAI || endpoint == constant.EndpointTypeOpenAIResponse {
			return true
		}
	}
	return false
}

// autoModelRouteCandidate 是参与排序的一条具体路线 (分组, 渠道, 模型)。
// 比较单元是路线而不是渠道:同名模型在不同渠道的得分独立,同优先级的模型分
// 可以跨渠道交错比较。
type autoModelRouteCandidate struct {
	ModelName     string
	ChannelID     int
	Priority      int64
	ChannelWeight float64
	ModelWeight   float64
	Score         float64 // effectiveScore:成功、延迟、观测置信度与时间衰减的有效分
}

// collectAutoModelRouteCandidates 收集参与竞争的路线。候选配置、模型权重、
// 计费资格与启用状态已在 eligibleAutoModelCandidates 过滤;这里再执行请求级
// 排除(已试路线、模型级失败)与严格冷却,被拒绝的路线不会进入任何档。
func collectAutoModelRouteCandidates(group string, candidates []string, snapshot autoModelRouteSnapshot) []autoModelRouteCandidate {
	var routes []autoModelRouteCandidate
	seenModels := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		if name == "" || operation_setting.IsAutoModelName(name) || seenModels[name] {
			continue
		}
		seenModels[name] = true
		if snapshot.ExcludedModels[autoModelFailedModelKey(group, name)] {
			continue
		}
		modelWeight := autoModelRouteWeight(group, name, snapshot.ModelWeights)
		if !positiveAutoModelWeight(modelWeight) {
			continue
		}
		seenTargets := make(map[int]bool, len(snapshot.Targets[name]))
		for _, target := range snapshot.Targets[name] {
			if target.ChannelID <= 0 || seenTargets[target.ChannelID] || snapshot.ExcludedChannels[target.ChannelID] {
				continue
			}
			seenTargets[target.ChannelID] = true
			// 本次请求已经打过的那条路不再进计划。放在去重之后:重复的 ability 行
			// 仍按一次计,不会被这里改变语义。
			if snapshot.ExcludedRoutes[AutoModelRoute{Group: group, ModelName: name, ChannelID: target.ChannelID}] {
				continue
			}
			key := autoModelHealthKey(group, name, target.ChannelID)
			if snapshot.Cooldowns[key].Until.After(snapshot.Now) {
				continue
			}
			routes = append(routes, autoModelRouteCandidate{
				ModelName:     name,
				ChannelID:     target.ChannelID,
				Priority:      target.Priority,
				ChannelWeight: target.Weight,
				ModelWeight:   modelWeight,
				Score:         effectiveScore(snapshot.ModelHealth[key], snapshot.Now),
			})
		}
	}
	return routes
}

// buildAutoModelRoutesFromSnapshot 把全部可用路线展平后按新规则排序:
//
//	渠道 priority 降序(硬边界) → 每个优先级内按模型有效得分分档 → 档内保留渠道份额的两级加权随机
//
// 渠道聚合健康分的整条链路(状态、更新、持久化、快照、展示)已随本方案整体
// 移除。没有模型观测的路线按中立有效分 0.5 参与分档,不再有"未观测渠道强制进
// 最好档"的例外。
// 冷却与请求级排除在收集阶段完成;它们不会因为高优先级或候选全部不可用而放行。
func buildAutoModelRoutesFromSnapshot(group string, candidates []string, snapshot autoModelRouteSnapshot, random func() float64) []AutoModelRoute {
	if random == nil {
		random = rand.Float64
	}
	routes := collectAutoModelRouteCandidates(group, candidates, snapshot)
	// priority 是硬边界:降序排列后逐层处理。低优先级候选即使得分更高,也只能
	// 排在仍有候选的高优先级层之后。
	sort.SliceStable(routes, func(i, j int) bool { return routes[i].Priority > routes[j].Priority })
	width := operation_setting.AutoModelScoreBandWidth()
	var plan []AutoModelRoute
	var debugAnchor float64
	for start := 0; start < len(routes); {
		end := start + 1
		for end < len(routes) && routes[end].Priority == routes[start].Priority {
			end++
		}
		tier := routes[start:end]
		plan = append(plan, orderAutoModelScoreBandRoutes(group, tier, width, random)...)
		if start == 0 {
			// 队首所在档的锚点 = 首层最高分(band 0 锚定在层内最高分)。
			debugAnchor = tier[0].Score
			for _, route := range tier {
				if route.Score > debugAnchor {
					debugAnchor = route.Score
				}
			}
		}
		start = end
	}
	if common.DebugEnabled && len(plan) > 0 {
		head := plan[0]
		for _, route := range routes {
			if route.ModelName == head.ModelName && route.ChannelID == head.ChannelID {
				common.SysLog(fmt.Sprintf(
					"auto route: group %s plan %d routes, band width %.4f, head %s@%d priority %d score %.4f anchor %.4f channel weight %.1f model weight %.1f",
					group, len(plan), width, route.ModelName, route.ChannelID, route.Priority,
					route.Score, debugAnchor, route.ChannelWeight, route.ModelWeight))
				break
			}
		}
	}
	return plan
}

// orderAutoModelScoreBandRoutes 把同一渠道优先级内的全部路线按模型有效得分分档,
// 档内做保留渠道份额的无放回抽样,输出这一层的完整路线顺序。
//
// 分档锚定在"尚未分档路线里的最高分",不是相邻项:相邻锚定会把
// 0.900/0.894/0.888 这样的一串接近分数串成一档。边界包含恰好等于档宽的情况;
// 浮点比较带 1e-9 容差,否则 0.900-0.890=0.0100…9 的浮点误差会把方案里
// "0.900/0.895/0.890 同档"的边界例子拆开。档宽 >= 1 时同优先级内所有正常
// 得分同档;档宽 = 0 时只有同分(含浮点容差)才同档。
//
// 同档以后精确分数不再决定先后,也不乘进随机权重:否则流量仍会向微弱领先者
// 集中,这正是得分分档要解决的问题。
func orderAutoModelScoreBandRoutes(group string, tier []autoModelRouteCandidate, width float64, random func() float64) []AutoModelRoute {
	ordered := append([]autoModelRouteCandidate(nil), tier...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Score != ordered[j].Score {
			return ordered[i].Score > ordered[j].Score
		}
		if ordered[i].ChannelID != ordered[j].ChannelID {
			return ordered[i].ChannelID < ordered[j].ChannelID
		}
		return ordered[i].ModelName < ordered[j].ModelName
	})
	var bands [][]autoModelRouteCandidate
	anchor := 0.0
	for _, route := range ordered {
		if len(bands) == 0 || route.Score < anchor-width-autoModelRouteScoreTieEpsilon {
			bands = append(bands, []autoModelRouteCandidate{route})
			anchor = route.Score
			continue
		}
		bands[len(bands)-1] = append(bands[len(bands)-1], route)
	}
	var plan []AutoModelRoute
	for _, band := range bands {
		plan = append(plan, sampleAutoModelBandRoutes(group, band, random)...)
	}
	return plan
}

// sampleAutoModelBandRoutes 在一个得分档内做无放回的两级抽样:先按渠道权重在
// 剩余渠道里选渠道,再按模型权重在该渠道的剩余路线里选模型。
//
// 每抽走一条路线都更新剩余候选与归一化分母,渠道不会被一次抽中后连续输出其
// 全部模型。渠道权重相同时,10 个候选模型的渠道与 1 个候选模型的渠道总流量
// 约各占一半;模型权重为 0 的路线在收集阶段已被排除,渠道权重为 0 的渠道在
// 正权重渠道耗尽前不参与(保留为该档内的后备),剩余渠道全零权重时等概率。
// 渠道与模型都按稳定顺序枚举,同一随机源下结果可复现。
func sampleAutoModelBandRoutes(group string, band []autoModelRouteCandidate, random func() float64) []AutoModelRoute {
	byChannel := make(map[int][]autoModelRouteCandidate, len(band))
	for _, route := range band {
		byChannel[route.ChannelID] = append(byChannel[route.ChannelID], route)
	}
	channelIDs := make([]int, 0, len(byChannel))
	for id := range byChannel {
		channelIDs = append(channelIDs, id)
	}
	sort.Ints(channelIDs)
	remaining := make(map[int][]autoModelRouteCandidate, len(channelIDs))
	channelWeights := make(map[int]float64, len(channelIDs))
	for _, id := range channelIDs {
		models := byChannel[id]
		sort.Slice(models, func(i, j int) bool { return models[i].ModelName < models[j].ModelName })
		remaining[id] = models
		channelWeights[id] = models[0].ChannelWeight
	}
	var plan []AutoModelRoute
	for len(remaining) > 0 {
		active := make([]int, 0, len(remaining))
		positive := make([]int, 0, len(remaining))
		for _, id := range channelIDs {
			if len(remaining[id]) == 0 {
				continue
			}
			active = append(active, id)
			if positiveAutoModelWeight(channelWeights[id]) {
				positive = append(positive, id)
			}
		}
		if len(active) == 0 {
			break
		}
		var chosenChannel int
		if len(positive) > 0 {
			items := make([]autoModelWeightedItem, 0, len(positive))
			for _, id := range positive {
				items = append(items, autoModelWeightedItem{Index: id, Weight: channelWeights[id]})
			}
			chosenChannel = positive[chooseAutoModelWeightedItem(items, random)]
		} else {
			chosenChannel = active[int(random()*float64(len(active)))%len(active)]
		}
		models := remaining[chosenChannel]
		items := make([]autoModelWeightedItem, 0, len(models))
		for i, route := range models {
			items = append(items, autoModelWeightedItem{Index: i, Weight: route.ModelWeight})
		}
		idx := chooseAutoModelWeightedItem(items, random)
		chosen := models[idx]
		plan = append(plan, AutoModelRoute{Group: group, ModelName: chosen.ModelName, ChannelID: chosen.ChannelID})
		if rest := append(models[:idx], models[idx+1:]...); len(rest) > 0 {
			remaining[chosenChannel] = rest
		} else {
			delete(remaining, chosenChannel)
		}
	}
	return plan
}


// autoModelWeightedItem 是一次加权抽样的候选项:Index 是候选项在其切片中的位置,
// Weight 是渠道权重或模型权重。分数不参与档内抽样——它只决定分档归属。
type autoModelWeightedItem struct {
	Index  int
	Weight float64
}

// chooseAutoModelWeightedItem 在全部正权重候选项里按权重抽一个,返回下标。
// 先按最大权重归一再求和,极大有限权重不会溢出;调用方保证不含零/负权重
// (零权重渠道的后备与全零等概率语义在抽样调用处处理)。
func chooseAutoModelWeightedItem(items []autoModelWeightedItem, random func() float64) int {
	maxWeight := 0.0
	for _, item := range items {
		if positiveAutoModelWeight(item.Weight) && item.Weight > maxWeight {
			maxWeight = item.Weight
		}
	}
	if maxWeight == 0 {
		// 理论上不可达:渠道与模型两级调用都只传正权重。保留等概率兜底。
		return int(random()*float64(len(items))) % len(items)
	}
	total := 0.0
	lastPositive := 0
	for i, item := range items {
		if positiveAutoModelWeight(item.Weight) {
			total += item.Weight / maxWeight // finite even for very large weights
			lastPositive = i
		}
	}
	draw := random() * total
	for i, item := range items {
		if positiveAutoModelWeight(item.Weight) {
			draw -= item.Weight / maxWeight
			if draw < 0 {
				return i
			}
		}
	}
	return lastPositive // floating-point rounding at the upper edge
}

func autoModelRouteWeight(group, name string, weights map[string]map[string]float64) float64 {
	if weight, exists := weights[group][name]; exists {
		return weight
	}
	return operation_setting.DefaultAutoModelWeight
}

func positiveAutoModelWeight(weight float64) bool {
	return weight > 0 && !math.IsNaN(weight) && !math.IsInf(weight, 0)
}

func autoModelRouteModelNames(routes []AutoModelRoute) []string {
	var names []string
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		if !seen[route.ModelName] {
			seen[route.ModelName] = true
			names = append(names, route.ModelName)
		}
	}
	return names
}
