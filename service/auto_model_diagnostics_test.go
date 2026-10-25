package service

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAutoModelPlanDiagnosticIncludesConfigEvidenceAndWarnings(t *testing.T) {
	route := AutoModelRoute{Group: "g", ModelName: "m", ChannelID: 1}
	plan := AutoModelRoutePlan{
		Routes: []AutoModelRoute{route},
		Meta: AutoModelRoutePlanMeta{
			Categories:  map[AutoModelRoute]string{route: "trial"},
			Reasons:     map[AutoModelRoute]string{route: "试用：未观察"},
			Evidence:    map[AutoModelRoute]autoModelEvidenceView{route: {}},
			Diagnostics: AutoModelRouteDiagnostics{Trial: 1},
		},
	}

	text := AutoModelPlanDiagnostic(plan, "g")

	require.Contains(t, text, "cfg{")
	require.Contains(t, text, "cat=trial")
	require.Contains(t, text, "rel=0?")
	require.Contains(t, text, "main=0")
}

func TestAutoModelPlanDiagnosticDoesNotAttributeMixedExclusionsOnlyToCooling(t *testing.T) {
	plan := AutoModelRoutePlan{Meta: AutoModelRoutePlanMeta{
		Categories:  map[AutoModelRoute]string{},
		Reasons:     map[AutoModelRoute]string{},
		Evidence:    map[AutoModelRoute]autoModelEvidenceView{},
		Diagnostics: AutoModelRouteDiagnostics{Cooling: 3, Unauthorized: 2, ZeroWeight: 1},
	}}

	text := AutoModelPlanDiagnostic(plan, "g")

	require.Contains(t, text, "no_eligible_routes(cooling_excluded=3)")
	require.NotContains(t, text, "all_candidates_cooling")
	require.Contains(t, text, "AutoModelCandidates")
	require.Contains(t, text, "AutoModelWeights")
}

func TestAutoModelPlanDiagnosticExplainsSuccessfulTrialReuse(t *testing.T) {
	route := AutoModelRoute{Group: "g", ModelName: "slow", ChannelID: 1}
	plan := AutoModelRoutePlan{
		Routes: []AutoModelRoute{route},
		Meta: AutoModelRoutePlanMeta{
			Categories:  map[AutoModelRoute]string{route: "successful_trial"},
			Diagnostics: AutoModelRouteDiagnostics{Trial: 1},
		},
	}
	diagnostic := AutoModelPlanDiagnostic(plan, "g")
	require.Contains(t, diagnostic, "首选已有成功路线")
	require.Contains(t, diagnostic, "同分组同优先级内优先复用")
	require.NotContains(t, diagnostic, "加权随机试用")
}

func TestAutoModelPlanDiagnosticKeepsDecisionSnapshotAfterConfigAndCooldownChange(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	settings.MinObservations, settings.ScoreDecayMinutes = 7, 65
	settings.ReliabilityThreshold = 0.9501
	route := AutoModelRoute{Group: "g", ModelName: "m", ChannelID: 7}
	cooled := autoModelHealthKey("other", "offline", 99)
	snapshot := autoModelRouteSnapshot{
		Now: now, Settings: settings, Mode: autoModelRequestStream,
		Targets:      map[string][]model.AutoModelRoutingTarget{"m": {{ChannelID: 7, Priority: 123, Weight: 2}}},
		ModelWeights: map[string]map[string]float64{"g": {"m": 0.234567}},
		ModelHealth: map[string]autoModelOutcome{autoModelHealthKey("g", "m", 7): {
			SuccessCount: 19, FailureCount: 1, LastObservedAt: now.Add(-30 * time.Second),
			StreamLatencyMS: 1200, StreamLatencySamples: 4, StreamLatencyUpdatedAt: now.Add(-20 * time.Second),
		}},
		Cooldowns: map[string]autoModelCooldownState{cooled: {
			Until: now.Add(2 * time.Minute), Level: 3, Reason: "rate\r\n limit\tfailure",
			pending: &autoModelCooldownPendingWrite{},
		}},
	}
	routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"m"}, snapshot, false, func() float64 { return 0.99 })
	meta.DiagnosticConfig = AutoModelDiagnosticConfig{CooldownBaseMinutes: 15, CooldownMaxHours: 24, MaxAttempts: 2, RetryStatusCodes: "500-599"}
	plan := AutoModelRoutePlan{Routes: routes, Meta: meta}
	oldMin, oldAttempts := operation_setting.AutoModelMinObservations(), operation_setting.AutoModelMaxAttempts()
	operation_setting.SetAutoModelMinObservations(11)
	operation_setting.SetAutoModelMaxAttempts(8)
	t.Cleanup(func() {
		operation_setting.SetAutoModelMinObservations(oldMin)
		operation_setting.SetAutoModelMaxAttempts(oldAttempts)
	})
	newKey := autoModelHealthKey(t.Name(), "new_failure", 88)
	autoModelCooldowns.Set(newKey, autoModelCooldownState{Until: time.Now().Add(time.Hour), Level: 1})
	t.Cleanup(func() { autoModelCooldowns.Delete(newKey) })

	diagnostic := AutoModelPlanDiagnostic(plan, "auto")
	require.Contains(t, diagnostic, "rel=0.9501 min_obs=7")
	require.Contains(t, diagnostic, "fresh=16.25m")
	require.Contains(t, diagnostic, "attempts=2")
	require.Contains(t, diagnostic, "planned_at=2023-11-14T22:13:20Z mode=stream")
	require.Contains(t, diagnostic, "priority=123 channel_weight=2 model_weight=0.234567")
	require.Contains(t, diagnostic, "rel=0.950")
	require.Contains(t, diagnostic, "obs_age=30s lat_age=20s gates=[reliability]")
	require.Contains(t, diagnostic, `other/offline#99 lv=3 left=2m0s persist_pending=true reason="rate limit failure"`)
	require.NotContains(t, diagnostic, "new_failure", "logging must not replace the planning cooldown snapshot with later state")
	require.Equal(t, "unreliable", meta.Categories[route])
	require.NotContains(t, diagnostic, "\r")
	require.NotContains(t, diagnostic, "\t")
}

func TestAutoModelPlanDiagnosticDoesNotPromiseLowerPrioritySuccessWillBeFirst(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	snapshot := autoModelRouteSnapshot{
		Now: now, Settings: settings, Diagnostics: &AutoModelRouteDiagnostics{},
		Targets: map[string][]model.AutoModelRoutingTarget{
			"manual_first": {{ChannelID: 1, Priority: 100, Weight: 1}},
			"working":      {{ChannelID: 2, Priority: 10, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{autoModelHealthKey("g", "working", 2): autoModelPlanTestOutcome(1, 0, 30000, 1, now)},
	}
	routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"manual_first", "working"}, snapshot, false, func() float64 { return 0.99 })
	require.Equal(t, "manual_first", routes[0].ModelName)
	diagnostic := AutoModelPlanDiagnostic(AutoModelRoutePlan{Routes: routes, Meta: meta}, "g")
	require.Contains(t, diagnostic, "首选仍需业务验证")
	require.Contains(t, diagnostic, "同分组同优先级内优先复用")
	require.NotContains(t, diagnostic, "首选已有成功")
	require.NotContains(t, diagnostic, "首选历史成功耗时")
}

func TestAutoModelPlanDiagnosticShowsSlowSuccessAndRetainedFailureMemory(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	settings.MinObservations = 5
	snapshot := autoModelRouteSnapshot{
		Now: now, Settings: settings, Diagnostics: &AutoModelRouteDiagnostics{},
		Targets: map[string][]model.AutoModelRoutingTarget{
			"slow_success": {{ChannelID: 1, Weight: 1}},
			"old_failure":  {{ChannelID: 2, Weight: 1}},
		},
		ModelHealth: map[string]autoModelOutcome{autoModelHealthKey("g", "slow_success", 1): autoModelPlanTestOutcome(1, 0, 45000, 1, now)},
		Cooldowns: map[string]autoModelCooldownState{autoModelHealthKey("g", "old_failure", 2): {
			Until: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Hour), Level: 4,
		}},
	}
	routes, meta := buildAutoModelRoutesFromSnapshot("g", []string{"old_failure", "slow_success"}, snapshot, false, func() float64 { return 0.99 })
	diagnostic := AutoModelPlanDiagnostic(AutoModelRoutePlan{Routes: routes, Meta: meta}, "g")
	require.Equal(t, "slow_success", routes[0].ModelName)
	require.Contains(t, diagnostic, "有效业务样本1<5")
	require.Contains(t, diagnostic, "AutoModelMinObservations")
	require.Contains(t, diagnostic, "首选历史成功耗时约45000ms")
	require.Contains(t, diagnostic, "不要仅因慢响应")
	require.Contains(t, diagnostic, "cat=pending_recovery")
	require.Contains(t, diagnostic, "cooldown_level=4 cooldown_active=false")
	require.NotContains(t, diagnostic, "cat=unobserved")
	meta.Mode = autoModelRequestStream
	streamDiagnostic := AutoModelPlanDiagnostic(AutoModelRoutePlan{Routes: routes, Meta: meta}, "g")
	require.Contains(t, streamDiagnostic, "历史流式成功首个有效输出约45000ms")
	require.Contains(t, streamDiagnostic, "整次响应还需生成时间")
}

func TestAutoModelPlanDiagnosticCountsRoutesBeyondPreviewAndSelectedRouteKeepsEvidence(t *testing.T) {
	plan := AutoModelRoutePlan{Meta: AutoModelRoutePlanMeta{
		PlannedAt: time.Unix(1700000000, 0), Settings: operation_setting.AutoModelRoutingSettingsSnapshot(),
		Categories: make(map[AutoModelRoute]string), Parameters: make(map[AutoModelRoute]AutoModelRouteParameters),
	}}
	for i := 1; i <= autoModelDiagnosticRouteLimit+2; i++ {
		route := AutoModelRoute{Group: "g", ModelName: fmt.Sprintf("m%d", i), ChannelID: i}
		plan.Routes = append(plan.Routes, route)
		plan.Meta.Categories[route] = "unobserved"
		plan.Meta.Parameters[route] = AutoModelRouteParameters{Priority: int64(100 - i), ChannelWeight: 1, ModelWeight: 2}
	}
	last := plan.Routes[len(plan.Routes)-1]
	diagnostic := AutoModelPlanDiagnostic(plan, "g")
	require.Contains(t, diagnostic, "summary{unobserved=12}")
	require.Contains(t, diagnostic, "...(+2)")
	require.NotContains(t, diagnostic, "g/m12#12")
	require.Contains(t, AutoModelRouteDiagnostic(plan.Meta, last), "g/m12#12 cat=unobserved priority=88")
}

func TestAutoModelRebuildDiagnosticPreservesAuthorizationExclusions(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	group := "g"
	require.NoError(t, operation_setting.SetAutoModelCandidates(`{"g":["route-a"]}`))
	require.NoError(t, operation_setting.SetAutoModelWeights(`{}`))
	require.NoError(t, db.Create(&model.Channel{Id: 1, Key: "one", Status: common.ChannelStatusEnabled, Group: group, Models: "route-a"}).Error)
	require.NoError(t, db.Create(&model.Ability{Group: group, Model: "route-a", ChannelId: 1, Enabled: true}).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"not-authorized": true})
	plan, status := RebuildAutoModelRoutePlanForRetry(c, group, map[AutoModelRoute]bool{{Group: group, ModelName: "unrelated", ChannelID: 999}: true})
	require.Equal(t, AutoModelRebuildEmpty, status)
	require.Equal(t, 1, plan.Meta.Diagnostics.Unauthorized, "an authorization failure must retain its counted cause")
	require.False(t, plan.Meta.PlannedAt.IsZero())
	require.Contains(t, AutoModelPlanDiagnostic(plan, group), "AutoModelCandidates")
}

func TestAutoModelPlanDiagnosticMarksIncompletePlanAndAvoidsThresholdAdvice(t *testing.T) {
	plan := AutoModelRoutePlan{Meta: AutoModelRoutePlanMeta{
		PlanningError: "candidate read\r\nfailed", Diagnostics: AutoModelRouteDiagnostics{Cooling: 2},
	}}
	diagnostic := AutoModelPlanDiagnostic(plan, "g")
	require.Contains(t, diagnostic, `plan_complete=false plan_error="candidate read failed"`)
	require.Contains(t, diagnostic, "计数仅代表已读取部分")
	require.NotContains(t, diagnostic, "no_eligible_routes")
	require.NotContains(t, diagnostic, "候选受冷却限制")
	require.Equal(t, 0, strings.Count(diagnostic, "\n"))
}

func TestAutoModelRebuildDiagnosticRetainsSnapshotOnDatabaseReadFailure(t *testing.T) {
	setupAutoModelRouteRequestTest(t)
	model.DB = nil
	model.InvalidateAutoModelRoutingTargetsCache()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	serviceMeta := AutoModelRoutePlanMeta{Mode: autoModelRequestStream}
	SetAutoModelRoutePlanMeta(c, serviceMeta)
	plan, status := RebuildAutoModelRoutePlanForRetry(c, "g", map[AutoModelRoute]bool{{Group: "g", ModelName: "old", ChannelID: 1}: true})
	require.Equal(t, AutoModelRebuildFailed, status)
	require.False(t, plan.Meta.PlannedAt.IsZero())
	require.NotEmpty(t, plan.Meta.PlanningError)
	require.Equal(t, autoModelRequestStream, plan.Meta.Mode)
	diagnostic := AutoModelPlanDiagnostic(plan, "g")
	require.Contains(t, diagnostic, "plan_complete=false")
	require.Contains(t, diagnostic, "计数仅代表已读取部分")
}

func TestAutoModelDiagnosticDoesNotRoundRejectedSamplesUpToAdmissionGate(t *testing.T) {
	now := time.Unix(1700000000, 0)
	settings := operation_setting.AutoModelRoutingSettingsSnapshot()
	view := autoModelEvidenceView{
		Reliability: 0.949999, Observations: float64(settings.MinObservations) - 0.000001,
		LatencySamples: autoModelMinLatencySamples - 0.000001, LastObservedAt: now, LatencyUpdatedAt: now,
	}
	require.Contains(t, autoModelEvidenceLog(view), "rel=0.949999?")
	require.Contains(t, autoModelEvidenceLog(view), fmt.Sprintf("obs=%g", view.Observations))
	require.Contains(t, autoModelEvidenceLog(view), "lat_samples=2.999999")
	require.Contains(t, autoModelEvidenceAgeLog(view, now, settings), "business_samples,reliability,latency_samples,latency_missing")
}
