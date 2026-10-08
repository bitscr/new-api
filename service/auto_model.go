package service

import (
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// autoModelOutcome 是一个被动健康摘要，模型与渠道两层使用相同的计分口径。
// 数据仅来自真实请求的成功/失败反馈，绝不发送主动测活请求。
type autoModelOutcome struct {
	Score        float64
	LatencyMS    float64
	Observations float64
	UpdatedAt    time.Time
}

// Model evidence is scoped to (group, model, channel). Channel evidence is
// independently aggregated across actual outcomes in (group, channel); it must
// never be approximated by a model's best score or highest channel priority.
var autoModelHealth = types.NewRWMap[string, autoModelOutcome]()
var autoModelChannelHealth = types.NewRWMap[string, autoModelOutcome]()

// autoModelHalfLifeMs 用于读取时的评分时间衰减（线性衰减至 10 分钟）。
const autoModelHalfLifeMs = 600000

// autoModelLatencyAlpha 是延迟 EWMA 平滑系数，沿用现有的 0.2 / 0.8。
const autoModelLatencyAlpha = 0.2

// autoModelScoreAlpha 是成功率的指数加权移动平均系数：新样本权重 30%，
// 让一条渠道变坏时分数快速下跌，而不是被历史大量成功拖住。
const autoModelScoreAlpha = 0.30

// autoScoreTieEpsilon retains the legacy channel selector's tolerance. The new
// channel-first route planner uses its own floating-point-only tie tolerance.
const autoScoreTieEpsilon = 0.02

func autoModelHealthKey(group, name string, channelID int) string {
	return group + "\x00" + name + "\x00" + strconv.Itoa(channelID)
}

func autoModelHealthPrefix(group, name string) string {
	return group + "\x00" + name + "\x00"
}

func autoModelChannelHealthKey(group string, channelID int) string {
	return group + "\x00" + strconv.Itoa(channelID)
}

// RecordAutoModelOutcome 记录一次已观察到的请求结果（被动反馈）。
// 同时更新 (分组, 模型, 渠道) 与 (分组, 渠道) 摘要，latencyMs <= 0 表示未采集到延迟。
// elapsedMs 是本次尝试的冷却信号：成功流式请求用首包延迟，非流式或失败用本次尝试耗时。
func RecordAutoModelOutcome(group, name string, channelID int, success bool, latencyMs int64, elapsedMs int64) {
	recordAutoModelOutcome(group, name, channelID, success, latencyMs, elapsedMs, false, "")
}

// RecordAutoModelUnusableAnswer 记录一次"HTTP 200 但没有有效回答"：
// 正文为空，或正文只是上游网关塞进来的告警横幅（实测样本见 relay/common/answer_observation.go）。
//
// 冷却只覆盖该 (分组, 模型, 渠道)，不会把该渠道上的其它模型也标为冷却。
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
	autoModelChannelHealth.Update(autoModelChannelHealthKey(group, channelID), func(current autoModelOutcome, exists bool) autoModelOutcome {
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

// GetAutoModelCandidates exposes the unique models in a fresh channel-first
// plan for one concrete group. Models with no enabled, non-cooling route (or an
// explicit zero model weight) are absent, including when every model is cooling.
func GetAutoModelCandidates(group string) []string {
	if !operation_setting.AutoModelEnabled || strings.TrimSpace(group) == "" {
		return nil
	}
	result, err := buildAutoModelRoutesForGroup(strings.TrimSpace(group), nil, newAutoModelRouteSnapshot(nil))
	if err != nil {
		common.SysError("failed to load auto model routing targets: " + err.Error())
		return nil
	}
	return autoModelRouteModelNames(result.Routes)
}

// GetAutoModelCandidatesForRequest uses the same authorization and strict
// cooldown filtering as dispatch. It is a model-list view, not a route plan:
// callers dispatching requests must retain the actual channel/model pairs.
func GetAutoModelCandidatesForRequest(c *gin.Context, group string) []string {
	routes, err := BuildAutoModelRoutesForRequest(c, group)
	if err != nil && !errors.Is(err, ErrAutoModelNoAuthorizedCandidates) {
		common.SysError("failed to build auto model routes: " + err.Error())
	}
	return autoModelRouteModelNames(routes)
}
