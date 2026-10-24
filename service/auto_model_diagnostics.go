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

// AutoModelRoutingConfigSummary 输出本次规划实际生效的参数快照。
// 配置在请求之间可能变化，所以每条计划日志都带一份，而不是只在启动时打印。
func AutoModelRoutingConfigSummary() string {
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	return fmt.Sprintf("cfg{rel=%.2f min_obs=%d decay=%dm tol=%.0fms/%.0f%% explore=%.3f~%.3f cd=%dm~%dh attempts=%d silent_hang=%dms}",
		settings.ReliabilityThreshold,
		settings.MinObservations,
		settings.ScoreDecayMinutes,
		float64(settings.LatencyToleranceMs),
		settings.LatencyToleranceRatio*100,
		settings.ExploreRatio,
		settings.ExploreMaxRatio,
		operation_setting.AutoModelCooldownBaseMinutes(),
		operation_setting.AutoModelCooldownMaxHours(),
		operation_setting.AutoModelMaxAttempts(),
		autoModelSlowLatencyMs,
	)
}

// AutoModelPlanDiagnostic 输出计划顺序与逐路由证据。
// Evidence 为空只可能来自手工构造的测试快照，此时省略证据字段而不是输出零值。
func AutoModelPlanDiagnostic(plan AutoModelRoutePlan, group string) string {
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	var order []string
	for i, route := range plan.Routes {
		if i >= autoModelDiagnosticRouteLimit {
			order = append(order, fmt.Sprintf("...(+%d)", len(plan.Routes)-i))
			break
		}
		parts := []string{
			fmt.Sprintf("%d:%s/%s#%d", i+1, route.Group, route.ModelName, route.ChannelID),
			"cat=" + orAutoModelDash(plan.Meta.Categories[route]),
		}
		if view, ok := plan.Meta.Evidence[route]; ok {
			parts = append(parts, autoModelEvidenceLog(view))
		}
		if reason := strings.TrimSpace(plan.Meta.Reasons[route]); reason != "" {
			parts = append(parts, fmt.Sprintf("reason=%q", autoModelLogText(reason, 60)))
		}
		order = append(order, strings.Join(parts, " "))
	}
	warnings := autoModelPlanWarnings(plan, settings)
	return fmt.Sprintf("%s cooling=[%s] order=[%s] warnings=[%s]",
		AutoModelRoutingConfigSummary(),
		autoModelCoolingLog(group, autoModelDiagnosticRouteLimit),
		strings.Join(order, " -> "),
		strings.Join(warnings, "; "),
	)
}

// autoModelCoolingLog lists active cooldowns in memory only. The planner already
// loaded them for this request, so this adds no database I/O. group="auto" means
// the token spans several real groups, so it shows every cooling pair.
func autoModelCoolingLog(group string, limit int) string {
	now := time.Now()
	prefix := strings.TrimSpace(group) + "\x00"
	all := false
	if trimmed := strings.TrimSpace(group); trimmed == "" || trimmed == "auto" {
		all = true
	}
	autoModelCooldownMutex.Lock()
	snapshot := autoModelCooldowns.ReadAll()
	autoModelCooldownMutex.Unlock()

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
		name, channel := key, "?"
		if first, last := strings.Index(key, "\x00"), strings.LastIndex(key, "\x00"); first >= 0 && last > first {
			name, channel = key[first+1:last], key[last+1:]
		}
		parts = append(parts, fmt.Sprintf("%s#%s lv=%d left=%s reason=%q",
			name, channel, state.Level, state.Until.Sub(now).Round(time.Second), autoModelLogText(state.Reason, 40)))
	}
	return strings.Join(parts, " | ")
}

func autoModelEvidenceLog(view autoModelEvidenceView) string {
	reliability := fmt.Sprintf("%.2f", view.Reliability)
	if !view.ReliabilityKnown {
		reliability += "?"
	}
	latency := fmt.Sprintf("%.0fms", view.LatencyMS)
	if !view.LatencyKnown {
		latency += "?"
	}
	return fmt.Sprintf("rel=%s obs=%.1f lat=%s pending=%t", reliability, view.Observations, latency, view.PendingRecovery)
}

// autoModelPlanWarnings 只报告"能由现有事实直接判定"的问题，不做主动探测。
// 每条告警都对应一个可解释的后果，避免把正常配置也标成异常。
func autoModelPlanWarnings(plan AutoModelRoutePlan, settings operation_setting.AutoModelRoutingSettings) []string {
	d := plan.Meta.Diagnostics
	var warnings []string

	if len(plan.Routes) > 0 && d.Main == 0 && d.Backup == 0 {
		warnings = append(warnings, "main=0: 所有候选都缺新鲜可靠性或延迟证据，本次顺序是加权随机试用")
	}
	if len(plan.Routes) == 0 && d.Cooling > 0 {
		warnings = append(warnings, fmt.Sprintf("all_candidates_cooling(cooling=%d)", d.Cooling))
	}

	base := time.Duration(operation_setting.AutoModelCooldownBaseMinutes()) * time.Minute
	decay := time.Duration(settings.ScoreDecayMinutes) * time.Minute
	freshness := decay / 4
	if base > 0 && decay < 2*base {
		warnings = append(warnings, fmt.Sprintf("decay=%s < 2*cd_base=%s，第 2 档冷却到期时证据已基本归零", decay, 2*base))
	}
	if base > 0 && freshness < base {
		warnings = append(warnings, fmt.Sprintf("freshness=%s < cd_base=%s，冷却到期后路由会以未知状态回到试用池", freshness, base))
	}
	if decay < operation_setting.AutoModelCooldownMax() {
		warnings = append(warnings, fmt.Sprintf("decay=%s < cd_max=%s，高等级冷却到期时证据可能已归零",
			decay, operation_setting.AutoModelCooldownMax()))
	}
	if operation_setting.AutoModelMaxAttempts() <= 1 {
		warnings = append(warnings, "attempts=1: 没有换路机会")
	}
	return warnings
}

func autoModelLogText(value string, limit int) string {
	value = strings.ReplaceAll(value, "\n", " ")
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
