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

// AutoModelRoute pins both halves of an auto dispatch. The route key is used by
// several request-local exclusion sets, so its fields must remain exactly these
// three values.
type AutoModelRoute struct {
	Group     string
	ModelName string
	ChannelID int
}

// AutoModelRoutePlanMeta carries request-scoped routing reasons without changing
// AutoModelRoute's identity. Categories are used by logs and the status page;
// Trial marks routes that need the process-local trial admission slot.
type AutoModelRoutePlanMeta struct {
	PlannedAt          time.Time
	Settings           operation_setting.AutoModelRoutingSettings
	DiagnosticConfig   AutoModelDiagnosticConfig
	PlanningError      string
	Parameters         map[AutoModelRoute]AutoModelRouteParameters
	Cooldowns          map[string]autoModelCooldownState
	Mode               autoModelRequestMode
	ExplorationDecided bool
	Explored           bool
	ExplorationRatio   float64
	ExplorationRoute   AutoModelRoute
	ColdStart          bool
	Categories         map[AutoModelRoute]string
	Reasons            map[AutoModelRoute]string
	Evidence           map[AutoModelRoute]autoModelEvidenceView
	Trial              map[AutoModelRoute]bool
	Diagnostics        AutoModelRouteDiagnostics
}

// AutoModelRouteParameters records the exact target weights and priority used
// by planning. It is diagnostic metadata, never part of route identity.
type AutoModelRouteParameters struct {
	Priority      int64
	ChannelWeight float64
	ModelWeight   float64
}

// AutoModelRouteDiagnostics counts why candidates were filtered and how the
// final plan was split. It is request-scoped diagnostics, not route identity.
type AutoModelRouteDiagnostics struct {
	WhitelistExcluded int
	NoTargets         int
	ZeroWeight        int
	BillingOrEndpoint int
	Unauthorized      int
	ChannelExcluded   int
	RouteExcluded     int
	ModelExcluded     int
	Cooling           int
	Main              int
	Backup            int
	Trial             int
}

type AutoModelRoutePlan struct {
	Routes []AutoModelRoute
	Meta   AutoModelRoutePlanMeta
}

const autoModelRoutePlanContextKey = "auto_model_route_plan"
const autoModelRoutePlanMetaContextKey = "auto_model_route_plan_meta"
const autoModelFailedModelsContextKey = "auto_model_failed_models"

func autoModelFailedModelKey(group, modelName string) string {
	return strings.TrimSpace(group) + "\x00" + strings.TrimSpace(modelName)
}

// MarkAutoModelModelFailed records a model-scoped retry failure for this request.
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

var ErrAutoModelNoAuthorizedCandidates = errors.New("no authorized auto model candidates")

// SetAutoModelRoutePlan owns a copy of the request's plan and activates its first
// route. Metadata is written separately so plan replacement can never silently
// change route identity.
func SetAutoModelRoutePlan(c *gin.Context, routes []AutoModelRoute) {
	if c == nil {
		return
	}
	plan := append([]AutoModelRoute(nil), routes...)
	c.Set(autoModelRoutePlanContextKey, plan)
	c.Set(autoModelRoutePlanMetaContextKey, AutoModelRoutePlanMeta{})
	names := make([]string, len(plan))
	for i, route := range plan {
		names[i] = route.ModelName
	}
	common.SetContextKey(c, constant.ContextKeyAutoModelCandidates, names)
	common.SetContextKey(c, constant.ContextKeyAutoModelIndex, -1)
	ActivateAutoModelRoute(c, 0)
}

func SetAutoModelRoutePlanMeta(c *gin.Context, meta AutoModelRoutePlanMeta) {
	if c == nil {
		return
	}
	c.Set(autoModelRoutePlanMetaContextKey, meta)
}

func GetAutoModelRoutePlanMeta(c *gin.Context) AutoModelRoutePlanMeta {
	if c == nil {
		return AutoModelRoutePlanMeta{}
	}
	value, _ := c.Get(autoModelRoutePlanMetaContextKey)
	meta, _ := value.(AutoModelRoutePlanMeta)
	return meta
}

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
	Candidates       map[string][]string
	ExcludedChannels map[int]bool
	ExcludedRoutes   map[AutoModelRoute]bool
	ExcludedModels   map[string]bool
	Mode             autoModelRequestMode
	Now              time.Time
	Settings         operation_setting.AutoModelRoutingSettings
	Diagnostics      *AutoModelRouteDiagnostics
}

// settingsSnapshot treats an unset Settings field as "use the process
// snapshot". Hand-built test snapshots predate the field; production snapshot
// constructors always set it explicitly.
func (snapshot autoModelRouteSnapshot) settingsSnapshot() operation_setting.AutoModelRoutingSettings {
	if snapshot.Settings == (operation_setting.AutoModelRoutingSettings{}) {
		return operation_setting.AutoModelRoutingSettingsSnapshot()
	}
	return snapshot.Settings
}

func newAutoModelRouteSnapshot(excluded map[int]bool, mode autoModelRequestMode) autoModelRouteSnapshot {
	return autoModelRouteSnapshot{
		ModelHealth:      autoModelHealth.ReadAll(),
		Cooldowns:        autoModelCooldownSnapshot(),
		ModelWeights:     operation_setting.GetAutoModelWeights(),
		Candidates:       operation_setting.GetAutoModelCandidates(),
		ExcludedChannels: excluded,
		Mode:             mode,
		Now:              time.Now(),
		Settings:         operation_setting.AutoModelRoutingSettingsSnapshot(),
		Diagnostics:      &AutoModelRouteDiagnostics{},
	}
}

// BuildAutoModelRoutePlanForRequest builds the initial plan once. Exploration is
// decided at most once, in the first nonempty group/priority layer.
func BuildAutoModelRoutePlanForRequest(c *gin.Context, group string, stream bool) (AutoModelRoutePlan, error) {
	mode := autoModelRequestNonStream
	if stream {
		mode = autoModelRequestStream
	}
	return buildAutoModelRoutePlanForRequest(c, group, mode, nil, true, rand.Float64)
}

// BuildAutoModelRoutesForRequest is the model-list compatibility wrapper.
func BuildAutoModelRoutesForRequest(c *gin.Context, group string) ([]AutoModelRoute, error) {
	stream := false
	if c != nil {
		stream = common.GetContextKeyBool(c, constant.ContextKeyIsStream)
	}
	plan, err := BuildAutoModelRoutePlanForRequest(c, group, stream)
	return plan.Routes, err
}

func buildAutoModelRoutePlanForRequest(c *gin.Context, group string, mode autoModelRequestMode, exclude map[AutoModelRoute]bool, allowExploration bool, random func() float64) (AutoModelRoutePlan, error) {
	group = strings.TrimSpace(group)
	plan := AutoModelRoutePlan{Meta: AutoModelRoutePlanMeta{
		Mode:       mode,
		Categories: make(map[AutoModelRoute]string),
		Reasons:    make(map[AutoModelRoute]string),
		Evidence:   make(map[AutoModelRoute]autoModelEvidenceView),
		Trial:      make(map[AutoModelRoute]bool),
	}}
	if c == nil || !operation_setting.AutoModelEnabled || group == "" {
		return plan, nil
	}
	if random == nil {
		random = rand.Float64
	}
	groups := []string{group}
	crossGroupRetry := false
	if group == "auto" {
		userGroup := common.GetContextKeyString(c, constant.ContextKeyUserGroup)
		groups = GetRequestGroupCandidates(c, userGroup, group)
		crossGroupRetry = common.GetContextKeyBool(c, constant.ContextKeyTokenCrossGroupRetry)
	}
	snapshot := newAutoModelRouteSnapshot(GetAutoModelHardExcludedChannelIDs(c), mode)
	plan.Meta.PlannedAt, plan.Meta.Settings = snapshot.Now, snapshot.settingsSnapshot()
	plan.Meta.DiagnosticConfig = autoModelDiagnosticConfigSnapshot()
	plan.Meta.Cooldowns = snapshot.Cooldowns
	plan.Meta.Parameters = make(map[AutoModelRoute]AutoModelRouteParameters)
	if snapshot.Diagnostics == nil {
		snapshot.Diagnostics = &AutoModelRouteDiagnostics{}
	}
	snapshot.ExcludedRoutes = exclude
	snapshot.ExcludedModels = getAutoModelFailedModels(c)
	hasCandidates, hasAuthorizedCandidates := false, false
	seenGroups := make(map[string]bool, len(groups))
	for _, candidateGroup := range groups {
		candidateGroup = strings.TrimSpace(candidateGroup)
		if candidateGroup == "" || candidateGroup == "auto" || seenGroups[candidateGroup] {
			continue
		}
		seenGroups[candidateGroup] = true
		result, err := buildAutoModelGroupPlan(candidateGroup, func(name string) bool {
			return IsAutoModelCandidateAuthorized(c, name)
		}, snapshot, allowExploration && !plan.Meta.ExplorationDecided, random)
		if err != nil {
			plan.Meta.Diagnostics = *snapshot.Diagnostics
			plan.Meta.PlanningError = err.Error()
			return plan, err
		}
		hasCandidates = hasCandidates || result.HasCandidates
		hasAuthorizedCandidates = hasAuthorizedCandidates || result.HasAuthorizedCandidates
		plan.Routes = append(plan.Routes, result.Routes...)
		for route, category := range result.Meta.Categories {
			plan.Meta.Categories[route] = category
		}
		for route, reason := range result.Meta.Reasons {
			plan.Meta.Reasons[route] = reason
		}
		for route, view := range result.Meta.Evidence {
			plan.Meta.Evidence[route] = view
		}
		for route, parameters := range result.Meta.Parameters {
			plan.Meta.Parameters[route] = parameters
		}
		for route, trial := range result.Meta.Trial {
			plan.Meta.Trial[route] = trial
		}
		if result.Meta.ExplorationDecided && !plan.Meta.ExplorationDecided {
			plan.Meta.ExplorationDecided = true
			plan.Meta.Explored = result.Meta.Explored
			plan.Meta.ExplorationRatio = result.Meta.ExplorationRatio
			plan.Meta.ExplorationRoute = result.Meta.ExplorationRoute
			plan.Meta.ColdStart = result.Meta.ColdStart
		}
		if len(plan.Routes) > 0 && !crossGroupRetry {
			break
		}
	}
	if !hasAuthorizedCandidates && hasCandidates {
		plan.Meta.Diagnostics = *snapshot.Diagnostics
		return plan, ErrAutoModelNoAuthorizedCandidates
	}
	plan.Meta.Diagnostics = *snapshot.Diagnostics
	return plan, nil
}

type autoModelGroupRouteResult struct {
	Routes                  []AutoModelRoute
	Meta                    AutoModelRoutePlanMeta
	HasCandidates           bool
	HasAuthorizedCandidates bool
}

func buildAutoModelGroupPlan(group string, authorized func(string) bool, snapshot autoModelRouteSnapshot, allowExploration bool, random func() float64) (autoModelGroupRouteResult, error) {
	result := autoModelGroupRouteResult{Meta: AutoModelRoutePlanMeta{
		Mode:       snapshot.Mode,
		Categories: make(map[AutoModelRoute]string),
		Reasons:    make(map[AutoModelRoute]string),
		Evidence:   make(map[AutoModelRoute]autoModelEvidenceView),
		Trial:      make(map[AutoModelRoute]bool),
	}}
	// A caller that already snapshotted this group's targets (the status page)
	// must get the same targets for classification and display.
	targets := snapshot.Targets
	if targets == nil {
		var err error
		targets, err = model.GetAutoModelRoutingTargets(group)
		if err != nil {
			return result, err
		}
	}
	snapshot.Targets = targets
	candidates := eligibleAutoModelCandidates(group, snapshot)
	result.HasCandidates = len(candidates) > 0
	if authorized != nil {
		allowed := make([]string, 0, len(candidates))
		for _, name := range candidates {
			if authorized(name) {
				allowed = append(allowed, name)
			} else if snapshot.Diagnostics != nil {
				snapshot.Diagnostics.Unauthorized++
			}
		}
		candidates = allowed
	}
	result.HasAuthorizedCandidates = len(candidates) > 0
	result.Routes, result.Meta = buildAutoModelRoutesFromSnapshot(group, candidates, snapshot, allowExploration, random)
	result.Meta.Mode = snapshot.Mode
	return result, nil
}

// Eligibility comes from the same enabled channel/ability snapshot used by the
// planner. Empty allowlists and explicit zero weights intentionally disable all
// affected routes.
func eligibleAutoModelCandidates(group string, snapshot autoModelRouteSnapshot) []string {
	candidatesConfig := snapshot.Candidates
	if candidatesConfig == nil {
		candidatesConfig = operation_setting.GetAutoModelCandidates()
	}
	allowed, configured := candidatesConfig[group]
	if !configured {
		allowed = make([]string, 0, len(snapshot.Targets))
		for name := range snapshot.Targets {
			allowed = append(allowed, name)
		}
		sort.Strings(allowed)
	} else if snapshot.Diagnostics != nil {
		allowedSet := make(map[string]bool, len(allowed))
		for _, name := range allowed {
			allowedSet[strings.TrimSpace(name)] = true
		}
		for name := range snapshot.Targets {
			if !allowedSet[strings.TrimSpace(name)] {
				snapshot.Diagnostics.WhitelistExcluded++
			}
		}
	}
	var candidates []string
	seen := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name == "" || operation_setting.IsAutoModelName(name) || seen[name] {
			continue
		}
		seen[name] = true
		if len(snapshot.Targets[name]) == 0 {
			if snapshot.Diagnostics != nil {
				snapshot.Diagnostics.NoTargets++
			}
			continue
		}
		if !positiveAutoModelWeight(autoModelRouteWeight(group, name, snapshot.ModelWeights)) {
			if snapshot.Diagnostics != nil {
				snapshot.Diagnostics.ZeroWeight++
			}
			continue
		}
		if !helper.HasModelBillingConfig(name) || !autoModelSupportsChat(model.GetModelSupportEndpointTypes(name)) {
			if snapshot.Diagnostics != nil {
				snapshot.Diagnostics.BillingOrEndpoint++
			}
			continue
		}
		candidates = append(candidates, name)
	}
	return candidates
}

func autoModelSupportsChat(endpoints []constant.EndpointType) bool {
	if len(endpoints) == 0 {
		return true
	}
	for _, endpoint := range endpoints {
		if endpoint == constant.EndpointTypeOpenAI || endpoint == constant.EndpointTypeOpenAIResponse {
			return true
		}
	}
	return false
}

// autoModelRouteCandidate is one concrete (group, model, channel) route with its
// independent reliability and mode-specific success latency evidence.
type autoModelRouteCandidate struct {
	ModelName     string
	ChannelID     int
	Priority      int64
	ChannelWeight float64
	ModelWeight   float64
	View          autoModelEvidenceView
}

func collectAutoModelRouteCandidates(group string, candidates []string, snapshot autoModelRouteSnapshot) []autoModelRouteCandidate {
	settings := snapshot.settingsSnapshot()
	var routes []autoModelRouteCandidate
	seenModels := make(map[string]bool, len(candidates))
	for _, name := range candidates {
		if name == "" || operation_setting.IsAutoModelName(name) || seenModels[name] {
			continue
		}
		seenModels[name] = true
		if snapshot.ExcludedModels[autoModelFailedModelKey(group, name)] {
			if snapshot.Diagnostics != nil {
				snapshot.Diagnostics.ModelExcluded++
			}
			continue
		}
		modelWeight := autoModelRouteWeight(group, name, snapshot.ModelWeights)
		if !positiveAutoModelWeight(modelWeight) {
			if snapshot.Diagnostics != nil {
				snapshot.Diagnostics.ZeroWeight++
			}
			continue
		}
		seenTargets := make(map[int]bool, len(snapshot.Targets[name]))
		for _, target := range snapshot.Targets[name] {
			if target.ChannelID <= 0 || seenTargets[target.ChannelID] {
				continue
			}
			seenTargets[target.ChannelID] = true
			if snapshot.ExcludedChannels[target.ChannelID] {
				if snapshot.Diagnostics != nil {
					snapshot.Diagnostics.ChannelExcluded++
				}
				continue
			}
			if snapshot.ExcludedRoutes[AutoModelRoute{Group: group, ModelName: name, ChannelID: target.ChannelID}] {
				if snapshot.Diagnostics != nil {
					snapshot.Diagnostics.RouteExcluded++
				}
				continue
			}
			key := autoModelHealthKey(group, name, target.ChannelID)
			if snapshot.Cooldowns[key].Until.After(snapshot.Now) {
				if snapshot.Diagnostics != nil {
					snapshot.Diagnostics.Cooling++
				}
				continue
			}
			routes = append(routes, autoModelRouteCandidate{
				ModelName: name, ChannelID: target.ChannelID,
				Priority: target.Priority, ChannelWeight: target.Weight, ModelWeight: modelWeight,
				View: autoModelRouteEvidenceView(snapshot.ModelHealth[key], snapshot.Cooldowns[key], snapshot.Mode, snapshot.Now, settings),
			})
		}
	}
	return routes
}

// A cooldown row carries failure memory even after expiry or evidence pruning.
// It never grants confidence or invents observations/latency samples.
func autoModelRouteEvidenceView(outcome autoModelOutcome, cooldown autoModelCooldownState, mode autoModelRequestMode, now time.Time, settings operation_setting.AutoModelRoutingSettings) autoModelEvidenceView {
	view := autoModelEvidenceViewForSettings(outcome, mode, now, settings)
	if !cooldown.Until.IsZero() && cooldown.Level > 0 &&
		(outcome.PendingRecovery || outcome.SuccessCount <= 0 || outcome.LastObservedAt.Unix() <= cooldown.UpdatedAt.Unix()) {
		view.PendingRecovery = true
	}
	return view
}

// buildAutoModelRoutesFromSnapshot keeps group/priority as hard boundaries. In
// each priority tier it classifies main candidates, orders latency pools, then
// appends trial routes. Exploration may prepend one route from outside the main
// pool, but never from another group or priority tier.
func buildAutoModelRoutesFromSnapshot(group string, candidates []string, snapshot autoModelRouteSnapshot, allowExploration bool, random func() float64) ([]AutoModelRoute, AutoModelRoutePlanMeta) {
	meta := AutoModelRoutePlanMeta{
		PlannedAt:        snapshot.Now,
		Settings:         snapshot.settingsSnapshot(),
		DiagnosticConfig: autoModelDiagnosticConfigSnapshot(),
		Parameters:       make(map[AutoModelRoute]AutoModelRouteParameters),
		Cooldowns:        snapshot.Cooldowns,
		Mode:             snapshot.Mode,
		Categories:       make(map[AutoModelRoute]string),
		Reasons:          make(map[AutoModelRoute]string),
		Evidence:         make(map[AutoModelRoute]autoModelEvidenceView),
		Trial:            make(map[AutoModelRoute]bool),
	}
	routes := collectAutoModelRouteCandidates(group, candidates, snapshot)
	byPriority := make(map[int64][]autoModelRouteCandidate)
	var priorities []int64
	for _, route := range routes {
		if _, ok := byPriority[route.Priority]; !ok {
			priorities = append(priorities, route.Priority)
		}
		byPriority[route.Priority] = append(byPriority[route.Priority], route)
	}
	sort.Slice(priorities, func(i, j int) bool { return priorities[i] > priorities[j] })
	var plan []AutoModelRoute
	decided := false
	for _, priority := range priorities {
		tierRoutes, tierMeta, tierDecided := buildAutoModelPriorityTier(group, byPriority[priority], snapshot, allowExploration && !decided, random)
		plan = append(plan, tierRoutes...)
		for route, category := range tierMeta.Categories {
			meta.Categories[route] = category
		}
		for route, reason := range tierMeta.Reasons {
			meta.Reasons[route] = reason
		}
		for route, view := range tierMeta.Evidence {
			meta.Evidence[route] = view
		}
		for route, parameters := range tierMeta.Parameters {
			meta.Parameters[route] = parameters
		}
		for route, trial := range tierMeta.Trial {
			meta.Trial[route] = trial
		}
		if tierDecided && !decided {
			decided = true
			meta.ExplorationDecided = tierMeta.ExplorationDecided
			meta.Explored = tierMeta.Explored
			meta.ExplorationRatio = tierMeta.ExplorationRatio
			meta.ExplorationRoute = tierMeta.ExplorationRoute
			meta.ColdStart = tierMeta.ColdStart
		}
	}
	if snapshot.Diagnostics != nil {
		meta.Diagnostics = *snapshot.Diagnostics
	}
	return plan, meta
}

func buildAutoModelPriorityTier(group string, tier []autoModelRouteCandidate, snapshot autoModelRouteSnapshot, allowExploration bool, random func() float64) ([]AutoModelRoute, AutoModelRoutePlanMeta, bool) {
	meta := AutoModelRoutePlanMeta{
		Parameters: make(map[AutoModelRoute]AutoModelRouteParameters),
		Mode:       snapshot.Mode,
		Categories: make(map[AutoModelRoute]string),
		Reasons:    make(map[AutoModelRoute]string),
		Evidence:   make(map[AutoModelRoute]autoModelEvidenceView),
		Trial:      make(map[AutoModelRoute]bool),
	}
	settings := snapshot.settingsSnapshot()
	mainCandidates := make([]autoModelRouteCandidate, 0, len(tier))
	var successfulTrialCandidates []autoModelRouteCandidate
	var trialCandidates []autoModelRouteCandidate
	var recoveryCandidates []autoModelRouteCandidate
	for _, candidate := range tier {
		route := AutoModelRoute{Group: group, ModelName: candidate.ModelName, ChannelID: candidate.ChannelID}
		meta.Evidence[route] = candidate.View
		meta.Parameters[route] = AutoModelRouteParameters{
			Priority: candidate.Priority, ChannelWeight: candidate.ChannelWeight, ModelWeight: candidate.ModelWeight,
		}
		if autoModelCandidateNeedsRecovery(candidate, settings) {
			recoveryCandidates = append(recoveryCandidates, candidate)
		} else if autoModelIsMainCandidate(candidate.View, settings) {
			mainCandidates = append(mainCandidates, candidate)
		} else if autoModelIsSuccessfulTrialCandidate(candidate.View, snapshot.Now, settings) {
			successfulTrialCandidates = append(successfulTrialCandidates, candidate)
		} else {
			trialCandidates = append(trialCandidates, candidate)
		}
	}

	pools := autoModelOrderLatencyPools(mainCandidates, settings)
	successfulTrialPools := autoModelOrderLatencyPools(successfulTrialCandidates, settings)
	if snapshot.Diagnostics != nil {
		snapshot.Diagnostics.Trial += len(successfulTrialCandidates) + len(trialCandidates) + len(recoveryCandidates)
		if len(pools) > 0 {
			snapshot.Diagnostics.Main += len(pools[0])
			for _, pool := range pools[1:] {
				snapshot.Diagnostics.Backup += len(pool)
			}
		}
	}
	mainRoutes := make([]AutoModelRoute, 0, len(tier))
	for poolIndex, pool := range pools {
		category := "main"
		reason := fmt.Sprintf("主流量候选：成功延迟 %.0fms", pool[0].View.LatencyMS)
		if poolIndex > 0 {
			category = "backup"
			reason = fmt.Sprintf("可靠慢后备：成功延迟 %.0fms", pool[0].View.LatencyMS)
		}
		for _, route := range sampleAutoModelCandidateRoutes(group, pool, random, func(candidate autoModelRouteCandidate) float64 {
			return candidate.ModelWeight
		}) {
			mainRoutes = append(mainRoutes, route)
			meta.Categories[route] = category
			meta.Reasons[route] = reason
		}
	}

	// A successful business request is enough to prefer this trial over an
	// unverified route. Keep the main-pool gates and single-flight admission:
	// subsequent real requests collect the remaining evidence instead of
	// spreading every cold-start request over hundreds of unknown routes.
	var successfulTrialRoutes []AutoModelRoute
	for _, pool := range successfulTrialPools {
		for _, route := range sampleAutoModelCandidateRoutes(group, pool, random, func(candidate autoModelRouteCandidate) float64 {
			return candidate.ModelWeight
		}) {
			successfulTrialRoutes = append(successfulTrialRoutes, route)
			meta.Categories[route] = "successful_trial"
			meta.Reasons[route] = "优先试用：已有成功，继续积累业务样本"
			meta.Trial[route] = true
		}
	}

	trialByRoute := make(map[AutoModelRoute]autoModelRouteCandidate, len(trialCandidates)+len(recoveryCandidates))
	for _, candidate := range append(append([]autoModelRouteCandidate(nil), trialCandidates...), recoveryCandidates...) {
		trialByRoute[AutoModelRoute{Group: group, ModelName: candidate.ModelName, ChannelID: candidate.ChannelID}] = candidate
	}
	trialRoutes := sampleAutoModelCandidateRoutes(group, trialCandidates, random, func(candidate autoModelRouteCandidate) float64 {
		return candidate.ModelWeight * autoModelExplorationTendency(candidate, snapshot.Now, settings)
	})
	// Expired cooldowns are permission to recover, not proof of health. Keep
	// these routes after untouched trials, even once their counts have decayed.
	trialRoutes = append(trialRoutes, sampleAutoModelCandidateRoutes(group, recoveryCandidates, random, func(candidate autoModelRouteCandidate) float64 {
		return candidate.ModelWeight
	})...)
	for _, route := range trialRoutes {
		category, reason := autoModelTrialCategory(trialByRoute[route], settings)
		meta.Categories[route] = category
		meta.Reasons[route] = reason
		meta.Trial[route] = true
	}

	layer := append(append([]AutoModelRoute(nil), mainRoutes...), successfulTrialRoutes...)
	layer = append(layer, trialRoutes...)
	if allowExploration && len(layer) > 0 {
		meta.ExplorationDecided = true
		if len(pools) == 0 && len(successfulTrialPools) == 0 {
			meta.ColdStart = true
			return layer, meta, true
		}
		exploreCandidates := append([]autoModelRouteCandidate(nil), trialCandidates...)
		// No admitted pool exists while a first successful trial warms up.
		if len(pools) > 1 {
			for _, pool := range pools[1:] {
				exploreCandidates = append(exploreCandidates, pool...)
			}
		}
		// When no fully admitted route exists, the fastest successful trial
		// pool receives normal traffic. Only routes outside that pool compete
		// for the same bounded exploration draw used after warm-up.
		trialPoolStart := 0
		if len(pools) == 0 {
			trialPoolStart = 1
		}
		for _, pool := range successfulTrialPools[trialPoolStart:] {
			exploreCandidates = append(exploreCandidates, pool...)
		}
		if len(exploreCandidates) > 0 {
			trialRatio := float64(len(successfulTrialCandidates)+len(trialCandidates)+len(recoveryCandidates)) / float64(len(tier))
			base := settings.ExploreRatio
			maximum := settings.ExploreMaxRatio
			meta.ExplorationRatio = base + (maximum-base)*trialRatio
			if meta.ExplorationRatio > 0 && random() < meta.ExplorationRatio {
				chosen, ok := chooseAutoModelExplorationCandidate(exploreCandidates, random, snapshot.Now, settings)
				if ok {
					chosenRoute := AutoModelRoute{Group: group, ModelName: chosen.ModelName, ChannelID: chosen.ChannelID}
					layer = prependUniqueAutoModelRoute(layer, chosenRoute)
					meta.Explored = true
					meta.ExplorationRoute = chosenRoute
				}
			}
		}
	}
	return layer, meta, meta.ExplorationDecided
}

func autoModelCandidateNeedsRecovery(candidate autoModelRouteCandidate, settings operation_setting.AutoModelRoutingSettings) bool {
	view := candidate.View
	return view.PendingRecovery || (view.Observations > 0 && view.Reliability < settings.ReliabilityThreshold)
}

func autoModelIsMainCandidate(view autoModelEvidenceView, settings operation_setting.AutoModelRoutingSettings) bool {
	return view.ReliabilityKnown &&
		view.Reliability >= settings.ReliabilityThreshold &&
		view.LatencyKnown && !view.PendingRecovery
}

func autoModelIsSuccessfulTrialCandidate(view autoModelEvidenceView, now time.Time, settings operation_setting.AutoModelRoutingSettings) bool {
	return view.Observations > 0 && view.Reliability >= settings.ReliabilityThreshold &&
		!view.PendingRecovery && autoModelEvidenceFreshFor(view.LastObservedAt, now, settings.ScoreDecayMinutes) &&
		view.LatencySamples > 0 && view.LatencyMS > 0 &&
		autoModelEvidenceFreshFor(view.LatencyUpdatedAt, now, settings.ScoreDecayMinutes)
}

func autoModelOrderLatencyPools(candidates []autoModelRouteCandidate, settings operation_setting.AutoModelRoutingSettings) [][]autoModelRouteCandidate {
	ordered := append([]autoModelRouteCandidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].View.LatencyMS != ordered[j].View.LatencyMS {
			return ordered[i].View.LatencyMS < ordered[j].View.LatencyMS
		}
		if ordered[i].ChannelID != ordered[j].ChannelID {
			return ordered[i].ChannelID < ordered[j].ChannelID
		}
		return ordered[i].ModelName < ordered[j].ModelName
	})
	var pools [][]autoModelRouteCandidate
	for start := 0; start < len(ordered); {
		anchor := ordered[start].View.LatencyMS
		tolerance := math.Max(float64(settings.LatencyToleranceMs),
			settings.LatencyToleranceRatio*anchor)
		end := start + 1
		for end < len(ordered) && ordered[end].View.LatencyMS <= anchor+tolerance+1e-9 {
			end++
		}
		pools = append(pools, append([]autoModelRouteCandidate(nil), ordered[start:end]...))
		start = end
	}
	return pools
}

func autoModelTrialCategory(route autoModelRouteCandidate, settings operation_setting.AutoModelRoutingSettings) (string, string) {
	view := route.View
	if view.PendingRecovery {
		return "pending_recovery", "待恢复：已有失败，仅作最后后备，不参与探索"
	}
	if view.Observations > 0 && view.Reliability < settings.ReliabilityThreshold {
		return "unreliable", "可靠性不足：仅作最后后备，不参与探索"
	}
	if view.Observations == 0 {
		return "unobserved", "待观察：未观察"
	}
	if !view.ReliabilityKnown {
		if view.Observations < float64(settings.MinObservations) {
			return "insufficient", "试用：业务样本不足"
		}
		return "stale", "试用：可靠性证据陈旧"
	}
	if view.Reliability < settings.ReliabilityThreshold {
		return "unreliable", "试用：可靠性不足"
	}
	if !view.LatencyKnown {
		if view.LatencySamples < autoModelMinLatencySamples {
			return "latency_insufficient", "试用：成功延迟样本不足"
		}
		return "latency_stale", "试用：成功延迟证据陈旧"
	}
	return "trial", "试用"
}

func autoModelExplorationTendency(candidate autoModelRouteCandidate, now time.Time, settings operation_setting.AutoModelRoutingSettings) float64 {
	tendency := 1.0
	minObservations := float64(settings.MinObservations)
	if minObservations > 0 && candidate.View.Observations < minObservations {
		tendency += 1 - candidate.View.Observations/minObservations
	}
	if candidate.View.LastObservedAt.IsZero() {
		tendency += 1
	} else {
		decay := time.Duration(settings.ScoreDecayMinutes) * time.Minute
		if decay > 0 {
			tendency += math.Min(1, math.Max(0, float64(now.Sub(candidate.View.LastObservedAt))/float64(decay)))
		}
	}
	return tendency
}

func chooseAutoModelExplorationCandidate(candidates []autoModelRouteCandidate, random func() float64, now time.Time, settings operation_setting.AutoModelRoutingSettings) (autoModelRouteCandidate, bool) {
	if len(candidates) == 0 {
		return autoModelRouteCandidate{}, false
	}
	byChannel := make(map[int][]autoModelRouteCandidate)
	channelWeights := make(map[int]float64)
	for _, candidate := range candidates {
		byChannel[candidate.ChannelID] = append(byChannel[candidate.ChannelID], candidate)
		channelWeights[candidate.ChannelID] = candidate.ChannelWeight
	}
	channelIDs := make([]int, 0, len(byChannel))
	active := make([]int, 0, len(byChannel))
	positive := make([]int, 0, len(byChannel))
	for id := range byChannel {
		channelIDs = append(channelIDs, id)
	}
	sort.Ints(channelIDs)
	for _, id := range channelIDs {
		active = append(active, id)
		if positiveAutoModelWeight(channelWeights[id]) {
			positive = append(positive, id)
		}
	}
	var channelID int
	if len(positive) > 0 {
		items := make([]autoModelWeightedItem, 0, len(positive))
		for _, id := range positive {
			items = append(items, autoModelWeightedItem{Index: id, Weight: channelWeights[id]})
		}
		channelID = positive[chooseAutoModelWeightedItem(items, random)]
	} else {
		channelID = active[int(random()*float64(len(active)))%len(active)]
	}
	models := byChannel[channelID]
	items := make([]autoModelWeightedItem, 0, len(models))
	for i, candidate := range models {
		items = append(items, autoModelWeightedItem{Index: i, Weight: candidate.ModelWeight * autoModelExplorationTendency(candidate, now, settings)})
	}
	return models[chooseAutoModelWeightedItem(items, random)], true
}

func prependUniqueAutoModelRoute(routes []AutoModelRoute, chosen AutoModelRoute) []AutoModelRoute {
	result := make([]AutoModelRoute, 0, len(routes)+1)
	result = append(result, chosen)
	for _, route := range routes {
		if route == chosen {
			continue
		}
		result = append(result, route)
	}
	return result
}

// sampleAutoModelCandidateRoutes preserves the existing two-level channel share:
// choose a channel by channel weight, then a model by its candidate weight.
func sampleAutoModelCandidateRoutes(group string, candidates []autoModelRouteCandidate, random func() float64, weight func(autoModelRouteCandidate) float64) []AutoModelRoute {
	if len(candidates) == 0 {
		return nil
	}
	if random == nil {
		random = rand.Float64
	}
	byChannel := make(map[int][]autoModelRouteCandidate, len(candidates))
	for _, route := range candidates {
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
			items = append(items, autoModelWeightedItem{Index: i, Weight: weight(route)})
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

type AutoModelRebuildStatus int

const (
	AutoModelRebuildSkipped AutoModelRebuildStatus = iota
	AutoModelRebuildReplaced
	AutoModelRebuildEmpty
	AutoModelRebuildFailed
)

func RebuildAutoModelRoutePlanForRetry(c *gin.Context, group string, attempted map[AutoModelRoute]bool) (AutoModelRoutePlan, AutoModelRebuildStatus) {
	if c == nil || len(attempted) == 0 {
		return AutoModelRoutePlan{}, AutoModelRebuildSkipped
	}
	mode := GetAutoModelRoutePlanMeta(c).Mode
	plan, err := buildAutoModelRoutePlanForRequest(c, group, mode, attempted, false, rand.Float64)
	if err != nil {
		if errors.Is(err, ErrAutoModelNoAuthorizedCandidates) {
			return plan, AutoModelRebuildEmpty
		}
		common.SysError("failed to rebuild auto model routes for retry: " + err.Error())
		return plan, AutoModelRebuildFailed
	}
	if len(plan.Routes) == 0 {
		// Preserve diagnostics even when the rebuild has no route left.
		return plan, AutoModelRebuildEmpty
	}
	plan.Meta.ExplorationDecided = true
	plan.Meta.Explored = false
	return plan, AutoModelRebuildReplaced
}

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

func ValidateAutoModelRouteCandidate(group, modelName string) bool {
	group = strings.TrimSpace(group)
	modelName = strings.TrimSpace(modelName)
	if group == "" || modelName == "" || operation_setting.IsAutoModelName(modelName) {
		return false
	}
	allowed, configured := operation_setting.GetAutoModelCandidates()[group]
	if configured {
		if len(allowed) == 0 {
			return false
		}
		found := false
		for _, candidate := range allowed {
			if strings.TrimSpace(candidate) == modelName {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	weights := operation_setting.GetAutoModelWeights()
	if !positiveAutoModelWeight(autoModelRouteWeight(group, modelName, weights)) {
		return false
	}
	if !autoModelSupportsChat(model.GetModelSupportEndpointTypes(modelName)) {
		return false
	}
	return true
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

type autoModelWeightedItem struct {
	Index  int
	Weight float64
}

func chooseAutoModelWeightedItem(items []autoModelWeightedItem, random func() float64) int {
	maxWeight := 0.0
	for _, item := range items {
		if positiveAutoModelWeight(item.Weight) && item.Weight > maxWeight {
			maxWeight = item.Weight
		}
	}
	if maxWeight == 0 {
		return int(random()*float64(len(items))) % len(items)
	}
	total := 0.0
	lastPositive := 0
	for i, item := range items {
		if positiveAutoModelWeight(item.Weight) {
			total += item.Weight / maxWeight
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
	return lastPositive
}

func GetAutoModelCandidates(group string) []string {
	if !operation_setting.AutoModelEnabled || strings.TrimSpace(group) == "" {
		return nil
	}
	result, err := buildAutoModelGroupPlan(strings.TrimSpace(group), nil, newAutoModelRouteSnapshot(nil, autoModelRequestNonStream), false, rand.Float64)
	if err != nil {
		common.SysError("failed to load auto model routing targets: " + err.Error())
		return nil
	}
	return autoModelRouteModelNames(result.Routes)
}

func GetAutoModelCandidatesForRequest(c *gin.Context, group string) []string {
	routes, err := BuildAutoModelRoutesForRequest(c, group)
	if err != nil && !errors.Is(err, ErrAutoModelNoAuthorizedCandidates) {
		common.SysError("failed to build auto model routes: " + err.Error())
	}
	return autoModelRouteModelNames(routes)
}
