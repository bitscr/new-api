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
	Now              time.Time
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
// order is group order, channel priority, channel score band, channel weight,
// then (inside that channel) model score band and model weight. Cooldown and hard
// exclusions remove routes before any tier is selected; they never fail open.
//
// For an auto token group, existing group authorization determines the ordered
// concrete groups. With cross-group retry disabled only the first nonempty
// group's plan is retained. A concrete group has already been authorized by
// middleware, including a permitted playground group override.
func BuildAutoModelRoutesForRequest(c *gin.Context, group string) ([]AutoModelRoute, error) {
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
			key := autoModelHealthKey(group, name, target.ChannelID)
			if snapshot.Cooldowns[key].Until.After(snapshot.Now) {
				continue
			}
			channel := channelsByID[target.ChannelID]
			if channel == nil {
				// The target snapshot supplies actual channel metadata, identical
				// across its models. Never derive this priority/score from a model.
				channel = &autoModelRouteChannel{
					ID:       target.ChannelID,
					Priority: target.Priority,
					Score:    effectiveScore(snapshot.ChannelHealth[autoModelChannelHealthKey(group, target.ChannelID)], snapshot.Now),
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
	sort.Slice(channels, func(i, j int) bool {
		if channels[i].Priority != channels[j].Priority {
			return channels[i].Priority > channels[j].Priority
		}
		return channels[i].ID < channels[j].ID
	})
	var routes []AutoModelRoute
	for start := 0; start < len(channels); {
		end := start + 1
		for end < len(channels) && channels[end].Priority == channels[start].Priority {
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
