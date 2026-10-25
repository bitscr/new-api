package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
)

// 这些函数只产生日志文本，不参与任何路由判定。目标是让一条日志足以回答：
// 本次用了哪些参数、计划顺序是什么、每条路由为什么在主池/后备/试用、
// 当前参数组合有没有明显不适合本部署的地方。

const autoModelDiagnosticRouteLimit = 10

// Runtime controls are captured alongside planning evidence for diagnostics;
// they do not change retry/cooldown behavior or introduce another config store.
type AutoModelDiagnosticConfig struct {
	CooldownBaseMinutes int
	CooldownMaxHours    int
	MaxAttempts         int
	RetryStatusCodes    string
}

func autoModelDiagnosticConfigSnapshot() AutoModelDiagnosticConfig {
	return AutoModelDiagnosticConfig{
		CooldownBaseMinutes: operation_setting.AutoModelCooldownBaseMinutes(),
		CooldownMaxHours:    operation_setting.AutoModelCooldownMaxHours(),
		MaxAttempts:         operation_setting.AutoModelMaxAttempts(),
		RetryStatusCodes:    operation_setting.AutomaticRetryStatusCodesToString(),
	}
}

// AutoModelRoutingConfigSummary 输出本次规划实际生效的参数快照。
// 配置在请求之间可能变化，所以每条计划日志都带一份，而不是只在启动时打印。
func AutoModelRoutingConfigSummary() string {
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	return autoModelRoutingConfigLog(settings, autoModelDiagnosticConfigSnapshot())
}

func autoModelRoutingConfigLog(settings operation_setting.AutoModelRoutingSettings, config AutoModelDiagnosticConfig) string {
	return fmt.Sprintf("cfg{rel=%g min_obs=%d min_lat_samples=%.0f decay=%dm fresh=%gm tol=%.0fms/%g%% explore=%g~%g cd=%dm~%dh attempts=%d silent_hang=%dms exploration_wait=%dms retry_status=%q priority=hard}",
		settings.ReliabilityThreshold,
		settings.MinObservations,
		autoModelMinLatencySamples,
		settings.ScoreDecayMinutes,
		float64(settings.ScoreDecayMinutes)/4,
		float64(settings.LatencyToleranceMs),
		settings.LatencyToleranceRatio*100,
		settings.ExploreRatio,
		settings.ExploreMaxRatio,
		config.CooldownBaseMinutes,
		config.CooldownMaxHours,
		config.MaxAttempts,
		autoModelSlowLatencyMs,
		autoModelSlowLatencyMs,
		config.RetryStatusCodes,
	)
}

// AutoModelPlanDiagnostic 输出计划顺序与逐路由证据。
// Evidence 为空只可能来自手工构造的测试快照，此时省略证据字段而不是输出零值。
func AutoModelPlanDiagnostic(plan AutoModelRoutePlan, group string) string {
	settings, config, now := autoModelDiagnosticSnapshot(plan.Meta)
	cooldowns := plan.Meta.Cooldowns
	if cooldowns == nil && plan.Meta.PlannedAt.IsZero() {
		// Compatibility for manually constructed plans; production uses its
		// existing snapshot so logging does not load or query the database.
		cooldowns = autoModelCooldowns.ReadAll()
	}
	var order []string
	for i, route := range plan.Routes {
		if i >= autoModelDiagnosticRouteLimit {
			order = append(order, fmt.Sprintf("...(+%d)", len(plan.Routes)-i))
			break
		}
		order = append(order, fmt.Sprintf("%d:%s", i+1, AutoModelRouteDiagnostic(plan.Meta, route)))
	}
	warnings := autoModelPlanWarnings(plan, settings)
	mode := "nonstream"
	if plan.Meta.Mode == autoModelRequestStream {
		mode = "stream"
	}
	return fmt.Sprintf("%s planned_at=%s mode=%s plan_complete=%t plan_error=%q exploration{decided=%t selected=%t ratio=%.3f} summary{%s} cooling=[%s] order=[%s] warnings=[%s] advice=[%s]",
		autoModelRoutingConfigLog(settings, config),
		now.UTC().Format(time.RFC3339Nano),
		mode, plan.Meta.PlanningError == "", autoModelLogText(plan.Meta.PlanningError, 160),
		plan.Meta.ExplorationDecided, plan.Meta.Explored, plan.Meta.ExplorationRatio,
		autoModelCategorySummary(plan),
		autoModelCoolingSnapshotLog(group, autoModelDiagnosticRouteLimit, now, cooldowns),
		strings.Join(order, " -> "),
		strings.Join(warnings, "; "),
		strings.Join(autoModelPlanAdvice(plan, settings, config), "; "),
	)
}

func autoModelDiagnosticSnapshot(meta AutoModelRoutePlanMeta) (operation_setting.AutoModelRoutingSettings, AutoModelDiagnosticConfig, time.Time) {
	settings, config, now := meta.Settings, meta.DiagnosticConfig, meta.PlannedAt
	if settings == (operation_setting.AutoModelRoutingSettings{}) {
		settings = operation_setting.AutoModelRoutingSettingsSnapshot()
	}
	if config == (AutoModelDiagnosticConfig{}) {
		config = autoModelDiagnosticConfigSnapshot()
	}
	if now.IsZero() {
		now = time.Now()
	}
	return settings, config, now
}

// AutoModelRouteDiagnostic also accompanies selected/result events, so routes
// past the truncated plan preview still retain their exact decision evidence.
func AutoModelRouteDiagnostic(meta AutoModelRoutePlanMeta, route AutoModelRoute) string {
	settings, _, now := autoModelDiagnosticSnapshot(meta)
	parts := []string{fmt.Sprintf("%s/%s#%d", autoModelLogText(route.Group, 80), autoModelLogText(route.ModelName, 100), route.ChannelID),
		"cat=" + orAutoModelDash(meta.Categories[route])}
	if parameters, ok := meta.Parameters[route]; ok {
		parts = append(parts, fmt.Sprintf("priority=%d channel_weight=%g model_weight=%g", parameters.Priority, parameters.ChannelWeight, parameters.ModelWeight))
	}
	if view, ok := meta.Evidence[route]; ok {
		parts = append(parts, autoModelEvidenceLog(view), autoModelEvidenceAgeLog(view, now, settings))
	}
	if state, ok := meta.Cooldowns[autoModelHealthKey(route.Group, route.ModelName, route.ChannelID)]; ok && state.Level > 0 {
		parts = append(parts, fmt.Sprintf("cooldown_level=%d cooldown_active=%t", state.Level, state.Until.After(now)))
	}
	if reason := strings.TrimSpace(meta.Reasons[route]); reason != "" {
		parts = append(parts, fmt.Sprintf("reason=%q", autoModelLogText(reason, 80)))
	}
	return strings.Join(parts, " ")
}

func autoModelCategorySummary(plan AutoModelRoutePlan) string {
	counts := make(map[string]int)
	for _, route := range plan.Routes {
		counts[orAutoModelDash(plan.Meta.Categories[route])]++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, " ")
}

// List active cooldowns from the planning snapshot without database I/O.
// group="auto" includes the real group of each cooling pair.
func autoModelCoolingSnapshotLog(group string, limit int, now time.Time, snapshot map[string]autoModelCooldownState) string {
	prefix := strings.TrimSpace(group) + "\x00"
	all := false
	if trimmed := strings.TrimSpace(group); trimmed == "" || trimmed == "auto" {
		all = true
	}
	keys := make([]string, 0, len(snapshot))
	for key, state := range snapshot {
		if !state.Until.After(now) {
			continue
		}
		if !all && !strings.HasPrefix(key, prefix) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, min(limit, len(keys)))
	for i, key := range keys {
		if i >= limit {
			parts = append(parts, fmt.Sprintf("...(+%d)", len(keys)-i))
			break
		}
		state := snapshot[key]
		realGroup, name, channel, ok := parseAutoModelHealthKey(key)
		if !ok {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s/%s#%d lv=%d left=%s persist_pending=%t reason=%q",
			autoModelLogText(realGroup, 80), autoModelLogText(name, 100), channel, state.Level, state.Until.Sub(now).Round(time.Second), state.pending != nil, autoModelLogText(state.Reason, 40)))
	}
	return strings.Join(parts, " | ")
}

func autoModelEvidenceLog(view autoModelEvidenceView) string {
	reliability := fmt.Sprintf("%g", view.Reliability)
	if !view.ReliabilityKnown {
		reliability += "?"
	}
	latency := fmt.Sprintf("%.0fms", view.LatencyMS)
	if !view.LatencyKnown {
		latency += "?"
	}
	return fmt.Sprintf("rel=%s obs=%g lat=%s lat_samples=%g pending=%t", reliability, view.Observations, latency, view.LatencySamples, view.PendingRecovery)
}

func autoModelEvidenceAgeLog(view autoModelEvidenceView, now time.Time, settings operation_setting.AutoModelRoutingSettings) string {
	age := func(at time.Time) string {
		if at.IsZero() {
			return "?"
		}
		return fmt.Sprintf("%ds", max(int64(0), int64(now.Sub(at).Seconds())))
	}
	var gates []string
	if view.PendingRecovery {
		gates = append(gates, "pending_recovery")
	}
	if view.Observations < float64(settings.MinObservations) {
		gates = append(gates, "business_samples")
	}
	if !autoModelEvidenceFreshFor(view.LastObservedAt, now, settings.ScoreDecayMinutes) {
		gates = append(gates, "business_stale")
	}
	if view.Observations > 0 && view.Reliability < settings.ReliabilityThreshold {
		gates = append(gates, "reliability")
	}
	if view.LatencySamples < autoModelMinLatencySamples {
		gates = append(gates, "latency_samples")
	}
	if view.LatencyMS <= 0 {
		gates = append(gates, "latency_missing")
	}
	if !autoModelEvidenceFreshFor(view.LatencyUpdatedAt, now, settings.ScoreDecayMinutes) {
		gates = append(gates, "latency_stale")
	}
	return fmt.Sprintf("obs_age=%s lat_age=%s gates=[%s]", age(view.LastObservedAt), age(view.LatencyUpdatedAt), strings.Join(gates, ","))
}

// AutoModelFeedbackDiagnostic is an in-memory read after publication. It shows
// whether cooldown persistence is pending without extra SQL or test requests.
func AutoModelFeedbackDiagnostic(route AutoModelRoute, stream bool) string {
	now := time.Now()
	mode := autoModelRequestNonStream
	if stream {
		mode = autoModelRequestStream
	}
	outcome, _ := autoModelHealth.Get(autoModelHealthKey(route.Group, route.ModelName, route.ChannelID))
	state, _ := autoModelCooldowns.Get(autoModelHealthKey(route.Group, route.ModelName, route.ChannelID))
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	view := autoModelRouteEvidenceView(outcome, state, mode, now, settings)
	return fmt.Sprintf("%s %s cooldown_active=%t cooldown_level=%d cooldown_remaining_ms=%d persist_pending=%t",
		autoModelEvidenceLog(view), autoModelEvidenceAgeLog(view, now, settings), state.Until.After(now), state.Level,
		max(int64(0), state.Until.Sub(now).Milliseconds()), state.pending != nil)
}

// autoModelPlanWarnings 只报告"能由现有事实直接判定"的问题，不做主动探测。
// 每条告警都对应一个可解释的后果，避免把正常配置也标成异常。
func autoModelPlanWarnings(plan AutoModelRoutePlan, settings operation_setting.AutoModelRoutingSettings) []string {
	d := plan.Meta.Diagnostics
	_, config, _ := autoModelDiagnosticSnapshot(plan.Meta)
	var warnings []string

	if plan.Meta.PlanningError == "" && len(plan.Routes) > 0 && d.Main == 0 && d.Backup == 0 {
		switch plan.Meta.Categories[plan.Routes[0]] {
		case "successful_trial":
			warnings = append(warnings, "main=0: 首选已有成功路线，同分组同优先级内优先复用，继续积累准入样本")
		case "pending_recovery", "unreliable":
			warnings = append(warnings, "main=0: 首选为失败恢复后备，仍遵循分组和优先级边界")
		default:
			warnings = append(warnings, "main=0: 首选仍需业务验证，成功路线只在同分组同优先级内优先复用")
		}
	}
	if plan.Meta.PlanningError == "" && len(plan.Routes) == 0 && d.Cooling > 0 {
		warnings = append(warnings, fmt.Sprintf("no_eligible_routes(cooling_excluded=%d)，同时检查其它排除计数", d.Cooling))
	}

	base := time.Duration(config.CooldownBaseMinutes) * time.Minute
	decay := time.Duration(settings.ScoreDecayMinutes) * time.Minute
	freshness := decay / 4
	if base > 0 && decay < 2*base {
		warnings = append(warnings, fmt.Sprintf("decay=%s < 2*cd_base=%s，第 2 档冷却到期时证据已基本归零", decay, 2*base))
	}
	if base > 0 && freshness < base {
		warnings = append(warnings, fmt.Sprintf("freshness=%s < cd_base=%s，冷却到期后证据已陈旧，失败记忆仍保留", freshness, base))
	}
	maximum := time.Duration(config.CooldownMaxHours) * time.Hour
	if decay < maximum {
		warnings = append(warnings, fmt.Sprintf("decay=%s < cd_max=%s，高等级冷却到期时证据可能已归零",
			decay, maximum))
	}
	if config.MaxAttempts <= 1 {
		warnings = append(warnings, "attempts=1: 没有换路机会")
	}
	return warnings
}

func autoModelPlanAdvice(plan AutoModelRoutePlan, settings operation_setting.AutoModelRoutingSettings, config AutoModelDiagnosticConfig) []string {
	var advice []string
	if config.MaxAttempts <= 1 {
		advice = append(advice, "AutoModelMaxAttempts=1，无换路预算；需要失败后备时设置至少2次真实派发")
	}
	if float64(settings.ScoreDecayMinutes)/4 < float64(config.CooldownBaseMinutes) {
		advice = append(advice, fmt.Sprintf("成功证据新鲜窗口短于首档冷却；如需解冻时保留近期成功证据，可将AutoModelScoreDecayMinutes设为至少%d分钟；失败记忆独立保留", 4*config.CooldownBaseMinutes))
	}
	if plan.Meta.PlanningError != "" {
		return append(advice, "候选计划读取失败，计数仅代表已读取部分；检查plan_error及数据库或候选缓存错误，勿据此调整路由阈值")
	}
	if len(plan.Routes) == 0 {
		if plan.Meta.Diagnostics.Cooling > 0 {
			advice = append(advice, "候选受冷却限制；先检查cooling原因，确认恢复后再清除，勿仅为增加候选缩短冷却")
		}
		if plan.Meta.Diagnostics.Unauthorized+plan.Meta.Diagnostics.WhitelistExcluded > 0 {
			advice = append(advice, "有候选被授权或白名单排除；检查令牌权限及AutoModelCandidates")
		}
		if plan.Meta.Diagnostics.ZeroWeight > 0 {
			advice = append(advice, "有模型权重为0而被排除；需要使用时检查AutoModelWeights")
		}
		if plan.Meta.Diagnostics.NoTargets > 0 {
			advice = append(advice, "候选模型没有启用的渠道目标；检查渠道状态、模型列表及分组能力配置")
		}
		if plan.Meta.Diagnostics.BillingOrEndpoint > 0 {
			advice = append(advice, "候选缺少计费配置或不支持当前聊天端点；检查模型计费及端点配置")
		}
		if plan.Meta.Diagnostics.RouteExcluded+plan.Meta.Diagnostics.ChannelExcluded+plan.Meta.Diagnostics.ModelExcluded > 0 {
			advice = append(advice, "本次请求已有路线或模型/渠道被排除；结合前序result与switch检查失败作用域，勿重复尝试同一路线")
		}
		return advice
	}
	first := plan.Routes[0]
	if plan.Meta.Explored && first == plan.Meta.ExplorationRoute && plan.Meta.Categories[first] != "main" &&
		plan.Meta.Categories[first] != "backup" && plan.Meta.Categories[first] != "successful_trial" {
		advice = append(advice, "本次首选来自未验证探索；若连续日志显示探索失败偏多，可降低AutoModelExploreRatio与AutoModelExploreMaxRatio，设为0可停止探索")
	}
	if view, ok := plan.Meta.Evidence[first]; ok && !view.PendingRecovery && view.Reliability >= settings.ReliabilityThreshold {
		if plan.Meta.Categories[first] == "successful_trial" && view.Observations < float64(settings.MinObservations) {
			advice = append(advice, fmt.Sprintf("首选已有成功但有效业务样本%g<%d；继续真实业务积累即可，低流量时可适度降低AutoModelMinObservations；延迟准入仍需%.0f份有效样本", view.Observations, settings.MinObservations, autoModelMinLatencySamples))
		}
		if plan.Meta.Categories[first] == "successful_trial" && view.LatencySamples < autoModelMinLatencySamples {
			advice = append(advice, fmt.Sprintf("首选有效成功延迟样本%g<%.0f；继续当前请求模式的真实成功流量，降低AutoModelMinObservations不能绕过延迟准入", view.LatencySamples, autoModelMinLatencySamples))
		}
		if view.LatencyMS > float64(autoModelSlowLatencyMs) && view.LatencySamples > 0 {
			if plan.Meta.Mode == autoModelRequestStream {
				advice = append(advice, fmt.Sprintf("首选历史流式成功首个有效输出约%.0fms；客户端首输出等待应覆盖该耗时及重试余量，整次响应还需生成时间，不要仅因慢响应降低可靠性门槛或冷却成功路线", view.LatencyMS))
			} else {
				advice = append(advice, fmt.Sprintf("首选历史成功耗时约%.0fms；客户端等待应覆盖成功耗时及重试余量，不要仅因慢响应降低可靠性门槛或冷却成功路线", view.LatencyMS))
			}
		}
	}
	return advice
}

func autoModelLogText(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}

func orAutoModelDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
