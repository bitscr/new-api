package service

import (
	"errors"
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
// channel. All remaining models on one channel stay together in the plan.
type AutoModelRoute struct {
	Group     string
	ModelName string
	ChannelID int
}

const autoModelRoutePlanContextKey = "auto_model_route_plan"

// This tolerance absorbs floating-point noise only. Genuinely different scores
// (for example 0.60 and 0.59) never share a weighted-random selection band.
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
	ChannelHealth    map[string]autoModelOutcome
	Cooldowns        map[string]autoModelCooldownState
	ModelWeights     map[string]map[string]float64
	ExcludedChannels map[int]bool
	// ExcludedRoutes drops concrete (分组, 模型, 渠道) 三元组,粒度比
	// ExcludedChannels 细:渠道级排除是"这条渠道整个不能用了",这里排除的是
	// "本次请求已经打过的那一条路"。重试时用它,让新计划天然从"还没试过的最优路"开头。
	// nil 表示不额外排除任何路由。
	ExcludedRoutes map[AutoModelRoute]bool
	Now            time.Time
}

func newAutoModelRouteSnapshot(excluded map[int]bool) autoModelRouteSnapshot {
	return autoModelRouteSnapshot{
		ModelHealth:      autoModelHealth.ReadAll(),
		ChannelHealth:    autoModelChannelHealth.ReadAll(),
		Cooldowns:        autoModelCooldownSnapshot(),
		ModelWeights:     operation_setting.GetAutoModelWeights(),
		ExcludedChannels: excluded,
		Now:              time.Now(),
	}
}

// BuildAutoModelRoutesForRequest takes fresh snapshots for every request. The
// order is group order, then channel health band, channel priority, channel score
// band and channel weight, then (inside that channel) model score band and model
// weight. Channels whose health differs by less than
// AutoModelPriorityOverrideMargin share a health band, so priority decides among
// them; a wider gap makes health decide on its own. Cooldown and hard exclusions
// remove routes before any tier is selected; they never fail open.
//
// For an auto token group, existing group authorization determines the ordered
// concrete groups. With cross-group retry disabled only the first nonempty
// group's plan is retained. A concrete group has already been authorized by
// middleware, including a permitted playground group override.
func BuildAutoModelRoutesForRequest(c *gin.Context, group string) ([]AutoModelRoute, error) {
	return buildAutoModelRoutesForRequest(c, group, nil)
}

// RebuildAutoModelRoutePlanForRetry re-derives the ordered plan for the same
// request group from a fresh health snapshot, dropping every route in attempted.
//
// Why a retry must re-plan rather than advance the plan it was given: the plan is
// ordered from the health visible when middleware ran. Any feedback produced by
// other requests since then is invisible to that frozen list, so index+1 can walk
// straight into a route that is currently the worst one available while the best
// one sits further down — and with AutoModelMaxAttempts at 4, "further down" is
// often unreachable.
//
// This is a pure re-ordering, never a loss: the plan is a permutation of the whole
// candidate route set (every eligible 模型×渠道 pair appears exactly once), so the
// rebuilt list contains every route the old one had, minus what is now attempted,
// cooling or hard-excluded. Those are exactly the routes a retry must not reuse.
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

type autoModelRouteChannel struct {
	ID       int
	Priority int64
	// Band 是健康分分档,0 最好。档宽 = AutoModelPriorityOverrideMargin:
	// 健康分差距小于档宽的渠道视为"一样健康",这时才由 Priority 说话;
	// 差距超过档宽,健康分自己分出档来。排序与分层都先读 Band 再读 Priority。
	Band int
	// Observed 表示这个渠道有近期证据(内存或库里的健康摘要)。没有证据的渠道不参与
	// 分档,见 assignAutoModelHealthBands:它既不是健康也不是不健康,而是"不知道"。
	Observed bool
	Score    float64
	Weight   float64
	Models   []autoModelRouteModel
}

type autoModelRouteModel struct {
	Name   string
	Score  float64
	Weight float64
}

func buildAutoModelRoutesFromSnapshot(group string, candidates []string, snapshot autoModelRouteSnapshot, random func() float64) []AutoModelRoute {
	if random == nil {
		random = rand.Float64
	}
	channelsByID := make(map[int]*autoModelRouteChannel)
	seenModels := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		if name == "" || operation_setting.IsAutoModelName(name) || seenModels[name] {
			continue
		}
		seenModels[name] = true
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
			channel := channelsByID[target.ChannelID]
			if channel == nil {
				// The target snapshot supplies actual channel metadata, identical
				// across its models. Never derive this priority/score from a model.
				channelHealth, observed := snapshot.ChannelHealth[autoModelChannelHealthKey(group, target.ChannelID)]
				channel = &autoModelRouteChannel{
					ID:       target.ChannelID,
					Priority: target.Priority,
					Observed: observed,
					Score:    effectiveScore(channelHealth, snapshot.Now),
					Weight:   target.Weight,
				}
				channelsByID[target.ChannelID] = channel
			}
			channel.Models = append(channel.Models, autoModelRouteModel{
				Name:   name,
				Score:  effectiveScore(snapshot.ModelHealth[key], snapshot.Now),
				Weight: modelWeight,
			})
		}
	}
	channels := make([]*autoModelRouteChannel, 0, len(channelsByID))
	for _, channel := range channelsByID {
		channels = append(channels, channel)
	}
	assignAutoModelHealthBands(channels, operation_setting.AutoModelPriorityOverrideMargin())
	sort.Slice(channels, func(i, j int) bool {
		// 先比健康分档,再比 priority。档内 priority 说了算,跨档健康分说了算。
		if channels[i].Band != channels[j].Band {
			return channels[i].Band < channels[j].Band
		}
		if channels[i].Priority != channels[j].Priority {
			return channels[i].Priority > channels[j].Priority
		}
		// 同档同 priority 内先按 ID,给档内挑选一个稳定起点:
		// orderAutoModelScoreBands 是稳定排序,同分渠道会保持这里的先后。
		return channels[i].ID < channels[j].ID
	})
	var routes []AutoModelRoute
	for start := 0; start < len(channels); {
		end := start + 1
		for end < len(channels) &&
			channels[end].Band == channels[start].Band &&
			channels[end].Priority == channels[start].Priority {
			end++
		}
		items := make([]autoModelScoreItem, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, autoModelScoreItem{Index: i, Score: channels[i].Score, Weight: channels[i].Weight})
		}
		for _, item := range orderAutoModelScoreBands(items, random) {
			channel := channels[item.Index]
			modelItems := make([]autoModelScoreItem, len(channel.Models))
			for i, candidate := range channel.Models {
				modelItems[i] = autoModelScoreItem{Index: i, Score: candidate.Score, Weight: candidate.Weight}
			}
			for _, modelItem := range orderAutoModelScoreBands(modelItems, random) {
				routes = append(routes, AutoModelRoute{Group: group, ModelName: channel.Models[modelItem.Index].Name, ChannelID: channel.ID})
			}
		}
		start = end
	}
	return routes
}

// assignAutoModelHealthBands 把渠道按健康分分档,0 是最好的档。
//
// 档宽就是 AutoModelPriorityOverrideMargin,分档规则与 orderAutoModelScoreBands
// 同源:每一档锚定在"还没分档的渠道里的最高分",而不是上一条渠道的分数。锚在相邻项上
// 会让一串接近但不等的分数被串成一档,把真正不同的健康度混在一起——那正是既有
// 渠道分档刻意避开的坑。
//
// 为什么用"档"而不是"逐条越级判定":健康分是连续量,真正可调的只有一个阈值。
// 分档让"健康分差距小于阈值时由 priority 说话,超过阈值时健康分自己说话"这句话
// 对任意多层同时成立。逐条比较则必须回答"越过谁",而多层场景下这个问题没有好
// 答案:比相邻的会漏掉更上面的渠道,比最高的又会把已经被越过的渠道算进参照物,
// 结果是"越级了但没排上去",规则看着生效、实际白做。
//
// 两个端点都自然落位,不需要额外的开关:
//   - margin <= 0:每个不同的分数各占一档,档序 = 分数序,priority 只用于同分,
//     即"健康分完全接管顺序";
//   - margin >= 1(健康分上限是 1.0):所有渠道同档,priority 完全决定顺序,
//     即"关闭分档、回到旧行为"。
//
// 两个端点都有一处共同的例外:从未观测过的渠道始终留在 0 档,不参与分档
// (理由见循环里的注释)。所以更准确的说法是"健康分在有证据的渠道之间决定顺序"。
//
// 输入顺序无所谓:函数自己按分数降序复制一份来分档,不改动入参顺序。
func assignAutoModelHealthBands(channels []*autoModelRouteChannel, margin float64) {
	if len(channels) == 0 || math.IsNaN(margin) {
		// NaN 档宽下所有比较都为假,留在 0 档等价于"关闭分档",是安全的一侧。
		return
	}
	byScore := append([]*autoModelRouteChannel(nil), channels...)
	sort.SliceStable(byScore, func(i, j int) bool {
		if byScore[i].Score != byScore[j].Score {
			return byScore[i].Score > byScore[j].Score
		}
		return byScore[i].ID < byScore[j].ID
	})
	// anchor 只取"观测到的最高分":未观测渠道的 0.5 是占位值而不是测量值,
	// 拿它当档锚会把真正的测量值按一个虚构的参照物分档。列表已按分数降序,
	// 所以遇到的第一个已观测渠道就是观测值里的最高分。
	band, started, anchor := 0, false, 0.0
	for _, channel := range byScore {
		// 从未观测过的渠道不参与分档,留在最好的档里由 priority 决定先后。
		//
		// 它既不是"健康"也不是"不健康",而是"不知道":按中性 0.5 分档会让一条刚
		// 加进来、管理员特意给了高 priority 的渠道,永远排在已证明健康的渠道后面。
		// 而本系统不做任何主动探测,证据只能来自真实流量——排不上队就永远拿不到证据,
		// 也就永远证明不了自己。让它按 priority 参与竞争,正是引入分档规则之前的行为。
		if !channel.Observed {
			channel.Band = 0
			continue
		}
		if !started {
			started, anchor = true, channel.Score
		} else if channel.Score < anchor-margin {
			band++
			anchor = channel.Score
		}
		channel.Band = band
	}
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

type autoModelScoreItem struct {
	Index  int
	Score  float64
	Weight float64
}

// Each band is anchored to its highest score, not to the previous item (which
// would let a chain of near-ties mix genuinely different scores). Sampling is
// without replacement so each concrete route occurs at most once per plan.
func orderAutoModelScoreBands(items []autoModelScoreItem, random func() float64) []autoModelScoreItem {
	ordered := append([]autoModelScoreItem(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Score > ordered[j].Score })
	result := make([]autoModelScoreItem, 0, len(ordered))
	for start := 0; start < len(ordered); {
		end := start + 1
		for end < len(ordered) && ordered[end].Score >= ordered[start].Score-autoModelRouteScoreTieEpsilon {
			end++
		}
		band := append([]autoModelScoreItem(nil), ordered[start:end]...)
		for len(band) > 0 {
			index := 0
			if len(band) > 1 {
				index = chooseAutoModelWeightedItem(band, random)
			}
			result = append(result, band[index])
			band = append(band[:index], band[index+1:]...)
		}
		start = end
	}
	return result
}

func chooseAutoModelWeightedItem(items []autoModelScoreItem, random func() float64) int {
	maxWeight := 0.0
	for _, item := range items {
		if positiveAutoModelWeight(item.Weight) && item.Weight > maxWeight {
			maxWeight = item.Weight
		}
	}
	if maxWeight == 0 {
		// Preserve channel-weight semantics: an all-zero channel band is
		// uniform. Zero-weight models were removed before channel construction.
		return int(random() * float64(len(items)))
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
