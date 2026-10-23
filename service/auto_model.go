package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
)

// autoModelOutcome 是一个被动健康摘要,粒度为 (分组, 模型, 渠道) 的模型层证据。
// 数据仅来自真实请求的成功/失败反馈,绝不发送主动测活请求。
//
// 新策略不再用综合分决定路由。可靠性以成功/失败有效计数单独估计;流式与非流式
// 成功延迟各自保存 EWMA、有效样本数和最后成功观测时间。延迟数值本身不随时间
// 衰减到零,证据陈旧时由样本数和时间判断为"未知",避免长期未调用显得越来越快。
//
// ObservationSeq/WriterID 是快照排序元数据,与持久化行一一对应:LastObservedAt
// 保留真实业务观测时间,而同一时间刻度上的先后由(序号,实例标识)给出确定性顺序。
// 两者都只在评分锁内更新,见 nextAutoModelObservationSeq。
type autoModelOutcome struct {
	SuccessCount float64
	FailureCount float64
	// LastObservedAt 是最后一次真实业务成功/失败的时间,用于可靠性和新鲜度。
	LastObservedAt time.Time

	StreamLatencyMS           float64
	StreamLatencySamples      float64
	StreamLatencyUpdatedAt    time.Time
	NonStreamLatencyMS        float64
	NonStreamLatencySamples   float64
	NonStreamLatencyUpdatedAt time.Time
	PendingRecovery           bool
	ObservationSeq            int64
	WriterID                  string
}

// autoModelWriterID 标识"这个进程",按进程生成一次,整个生命周期不变。
// 它只用于同刻度快照的确定性冲突规则,不参与任何鉴权,也不写入日志之外的地方。
var autoModelWriterID = newAutoModelWriterID()

func newAutoModelWriterID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		// crypto/rand 不可用时退回本地唯一值:标识必须非空且同进程稳定,
		// 否则同刻度的跨实例冲突规则会退化成"永远平局"。
		return fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

// nextAutoModelObservationSeq 在评分锁内为一个三元组分配下一次观测的排序元数据。
//
// 序号是"同一进程、同一三元组"的单调计数,不是全局时钟:它唯一的职责是让同一
// 时间刻度上的两次观测有确定的先后,从而不必等墙钟前进就能接纳新快照。
// 从库里恢复的旧行带着它自己的序号继续递增(见 restore),不会回退。
func nextAutoModelObservationSeq(previous autoModelOutcome) (int64, string) {
	sequence := previous.ObservationSeq + 1
	if sequence <= 0 {
		sequence = 1
	}
	return sequence, autoModelWriterID
}

// Model evidence is scoped to (group, model, channel) and is the only signal the
// auto route planner ranks by. The former channel-aggregated health layer has
// been removed together with the channel-band ordering
// (docs/auto-routing-priority-model-bands-plan.md).
var autoModelHealth = types.NewRWMap[string, autoModelOutcome]()

// Serialize model feedback publication, persistence snapshots and restore
// merges. Lock order is this mutex -> RWMap; never hold it during SQL/cooldown I/O.
var autoModelScoreMutex sync.Mutex

// autoModelHalfLifeMs 是可靠性计数和成功延迟样本的时间衰减窗口。
// 它必须长于抖动冷却阶梯的前几档,否则冷却到期时证据已经失效,组合会带着
// “零记忆”重回候选池。默认 360 分钟,可在面板调整(AutoModelScoreDecayMinutes)。
func autoModelHalfLifeMs() int64 {
	return int64(operation_setting.AutoModelScoreDecayMinutes()) * 60 * 1000
}

// autoModelLatencyAlpha 是成功延迟 EWMA 平滑系数。
const autoModelLatencyAlpha = 0.2

func autoModelHealthKey(group, name string, channelID int) string {
	return group + "\x00" + name + "\x00" + strconv.Itoa(channelID)
}

func autoModelHealthPrefix(group, name string) string {
	return group + "\x00" + name + "\x00"
}

// RecordAutoModelOutcome 记录一次已观察到的请求结果（被动反馈），粒度是
// (分组, 模型, 渠道)。默认按非流式处理；latencyMs <= 0 表示未采集到延迟。
func RecordAutoModelOutcome(group, name string, channelID int, success bool, latencyMs int64, elapsedMs int64) {
	RecordAutoModelOutcomeWithMode(group, name, channelID, success, false, latencyMs > 0, latencyMs, elapsedMs, "")
}

// RecordAutoModelOutcomeWithReason 是 RecordAutoModelOutcome 的带原因版本。
func RecordAutoModelOutcomeWithReason(group, name string, channelID int, success bool, latencyMs int64, elapsedMs int64, reason string) {
	RecordAutoModelOutcomeWithMode(group, name, channelID, success, false, latencyMs > 0, latencyMs, elapsedMs, reason)
}

// RecordAutoModelOutcomeWithMode 是真实业务反馈的入口。stream 决定写入哪一类
// 成功延迟证据；latencyKnown=false 时不写入任何速度样本，但仍更新可靠性。
func RecordAutoModelOutcomeWithMode(group, name string, channelID int, success, stream, latencyKnown bool, latencyMs int64, elapsedMs int64, reason string) {
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, success, stream, latencyKnown,
		latencyMs, elapsedMs, reason, time.Now())
}

// RecordAutoModelOutcomeWithModeAt 与上面的入口相同，但由调用方固定真实观测时间。
// 反馈可以在请求结束后的 flush 里发布，若使用发布时间，早先尝试的新鲜度会被
// 后续长请求“刷新”，冷却也会从错误的时间起算。
func RecordAutoModelOutcomeWithModeAt(group, name string, channelID int, success, stream, latencyKnown bool, latencyMs int64, elapsedMs int64, reason string, observedAt time.Time) {
	recordAutoModelOutcome(group, name, channelID, success, stream, latencyKnown, latencyMs, elapsedMs, false, reason, observedAt)
}

// RecordAutoModelUnusableAnswer 记录一次"HTTP 200 但没有有效回答"：
// 正文为空，或正文只是上游网关塞进来的告警横幅（实测样本见 relay/common/answer_observation.go）。
//
// 冷却只覆盖该 (分组, 模型, 渠道)，不会把该渠道上的其它模型也标为冷却。
func RecordAutoModelUnusableAnswer(group, name string, channelID int, reason string) {
	RecordAutoModelUnusableAnswerAt(group, name, channelID, reason, time.Now())
}

// RecordAutoModelUnusableAnswerAt 与 RecordAutoModelUnusableAnswer 相同，观测时间
// 由入队时固定，避免请求结束时的 flush 改写新鲜度。
func RecordAutoModelUnusableAnswerAt(group, name string, channelID int, reason string, observedAt time.Time) {
	recordAutoModelOutcome(group, name, channelID, false, false, false, 0, 0, true, reason, observedAt)
}

// RecordAutoModelTestOutcome 只保留手动测试的确定性失败处理。测试请求的
// 成功/延迟不是真实业务证据，不能写入新路由的可靠性或速度摘要，也不能清除
// 冷却或证明路线恢复。命中确定性关键词仍按既有策略摘除组合。
func RecordAutoModelTestOutcome(channel *model.Channel, modelName string, success bool, elapsedMs int64, testErr *types.NewAPIError) {
	if channel == nil || !operation_setting.AutoModelEnabled || success || testErr == nil {
		return
	}
	name := strings.TrimSpace(modelName)
	if name == "" || operation_setting.IsAutoModelName(name) {
		return
	}
	if !AutoModelPermanentFailure(testErr) {
		return
	}
	reason := testErr.Error()
	if runes := []rune(reason); len(runes) > 200 {
		reason = string(runes[:200])
	}
	for _, group := range strings.Split(channel.Group, ",") {
		group = strings.TrimSpace(group)
		if group != "" {
			RecordAutoModelPermanentFailure(group, name, channel.Id, "手动测试失败: "+reason)
		}
	}
}

func recordAutoModelOutcome(group, name string, channelID int, success, stream, latencyKnown bool, latencyMs int64, elapsedMs int64, modelOnlyCooldown bool, cooldownReason string, observedAt time.Time) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 || operation_setting.IsAutoModelName(name) {
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	key := autoModelHealthKey(group, name, channelID)
	// Feedback publication is a whole-route action: it must not interleave with
	// a deterministic retirement or an administrator clear for the same route.
	unlock := lockAutoModelRoute(key)
	defer unlock()
	autoModelScoreMutex.Lock()
	if tombstone, cleared := autoModelClearTombstones[key]; cleared && !observedAt.After(tombstone.ClearedAt) {
		// A feedback queued before an administrator clear must not recreate
		// evidence in memory after that clear. Only a genuinely later observation
		// may start learning again. This check and the update share one lock so a
		// clear cannot slip between them.
		autoModelScoreMutex.Unlock()
		return
	}
	autoModelHealth.Update(key, func(current autoModelOutcome, exists bool) autoModelOutcome {
		return updateAutoModelOutcome(current, exists, success, stream, latencyKnown, latencyMs, observedAt)
	})
	autoModelScoreMutex.Unlock()
	if modelOnlyCooldown {
		reason := strings.TrimSpace(cooldownReason)
		if reason == "" {
			reason = "响应无有效内容"
		}
		tripAutoModelCooldown(group, name, channelID, reason, observedAt)
		return
	}
	// 慢或者失败进入冷却（落库，跨重启生效），正常速度的成功清除冷却。
	applyAutoModelCooldownWithReasonAt(group, name, channelID, success, elapsedMs, cooldownReason, observedAt)
}

const (
	autoModelMaxEvidenceCount  = 100.0
	autoModelMinLatencySamples = 3.0
)

type autoModelRequestMode int

const (
	autoModelRequestNonStream autoModelRequestMode = iota
	autoModelRequestStream
)

// autoModelEvidenceView 是规划器读取的只读证据视图。证据陈旧、样本不足或
// 未观测时 Known 为 false；不能把未知值当成 0.5 分或 0ms。
type autoModelEvidenceView struct {
	Reliability      float64
	ReliabilityKnown bool
	Observations     float64
	LastObservedAt   time.Time
	LatencyMS        float64
	LatencySamples   float64
	LatencyKnown     bool
	LatencyUpdatedAt time.Time
	PendingRecovery  bool
}

// autoModelObservationCleared reports whether an observation predates the
// administrator's latest clear. It is shared by evidence and cooldown/retire
// publication so a delayed pre-clear failure cannot take either effect.
func autoModelObservationCleared(key string, observedAt time.Time) bool {
	autoModelScoreMutex.Lock()
	defer autoModelScoreMutex.Unlock()
	tombstone, cleared := autoModelClearTombstones[key]
	return cleared && !observedAt.After(tombstone.ClearedAt)
}

func autoModelHalfLifeMsFor(decayMinutes int) int64 {
	if decayMinutes < 1 {
		decayMinutes = 1
	}
	return int64(decayMinutes) * 60 * 1000
}

func autoModelEvidenceFreshFor(updatedAt, now time.Time, decayMinutes int) bool {
	if updatedAt.IsZero() {
		return false
	}
	window := time.Duration(decayMinutes) * time.Minute / 4
	if window <= 0 {
		window = time.Minute
	}
	return now.Sub(updatedAt) <= window
}

func autoModelEvidenceFresh(updatedAt, now time.Time) bool {
	return autoModelEvidenceFreshFor(updatedAt, now, operation_setting.AutoModelScoreDecayMinutes())
}

func autoModelDecayFactorFor(updatedAt, now time.Time, halfLifeMs int64) float64 {
	if updatedAt.IsZero() {
		return 0
	}
	age := now.Sub(updatedAt)
	if age <= 0 {
		return 1
	}
	factor := 1 - float64(age.Milliseconds())/float64(halfLifeMs)
	return math.Max(0, factor)
}

func autoModelDecayFactor(updatedAt, now time.Time) float64 {
	return autoModelDecayFactorFor(updatedAt, now, autoModelHalfLifeMs())
}

// decayAutoModelOutcome 衰减证据的“有效数量”，但不衰减延迟数值本身。
// 延迟值保持最后一次成功观测，证据是否可用由样本数和新鲜度决定。
func decayAutoModelOutcomeFor(outcome autoModelOutcome, now time.Time, halfLifeMs int64) autoModelOutcome {
	reliabilityFactor := autoModelDecayFactorFor(outcome.LastObservedAt, now, halfLifeMs)
	outcome.SuccessCount *= reliabilityFactor
	outcome.FailureCount *= reliabilityFactor
	outcome.StreamLatencySamples *= autoModelDecayFactorFor(outcome.StreamLatencyUpdatedAt, now, halfLifeMs)
	outcome.NonStreamLatencySamples *= autoModelDecayFactorFor(outcome.NonStreamLatencyUpdatedAt, now, halfLifeMs)
	if outcome.SuccessCount+outcome.FailureCount < 1e-9 {
		outcome.SuccessCount, outcome.FailureCount = 0, 0
	}
	if outcome.StreamLatencySamples < 1e-9 {
		outcome.StreamLatencySamples = 0
	}
	if outcome.NonStreamLatencySamples < 1e-9 {
		outcome.NonStreamLatencySamples = 0
	}
	return outcome
}

func decayAutoModelOutcome(outcome autoModelOutcome, now time.Time) autoModelOutcome {
	return decayAutoModelOutcomeFor(outcome, now, autoModelHalfLifeMs())
}

func capAutoModelEvidence(outcome *autoModelOutcome) {
	total := outcome.SuccessCount + outcome.FailureCount
	if total > autoModelMaxEvidenceCount {
		scale := autoModelMaxEvidenceCount / total
		outcome.SuccessCount *= scale
		outcome.FailureCount *= scale
	}
}

func autoModelEvidenceViewFor(outcome autoModelOutcome, mode autoModelRequestMode, now time.Time) autoModelEvidenceView {
	return autoModelEvidenceViewForSettings(outcome, mode, now, operation_setting.AutoModelRoutingSettingsSnapshot())
}

// autoModelEvidenceViewForSettings evaluates one evidence snapshot with one
// configuration snapshot. The planner and the status page must use this same
// function so "sample sufficient" and "entered the main pool" cannot disagree.
func autoModelEvidenceViewForSettings(outcome autoModelOutcome, mode autoModelRequestMode, now time.Time, settings operation_setting.AutoModelRoutingSettings) autoModelEvidenceView {
	outcome = decayAutoModelOutcomeFor(outcome, now, autoModelHalfLifeMsFor(settings.ScoreDecayMinutes))
	view := autoModelEvidenceView{
		Observations:    outcome.SuccessCount + outcome.FailureCount,
		LastObservedAt:  outcome.LastObservedAt,
		PendingRecovery: outcome.PendingRecovery,
	}
	if view.Observations > 0 {
		view.Reliability = outcome.SuccessCount / view.Observations
	}
	view.ReliabilityKnown = view.Observations >= float64(settings.MinObservations) &&
		autoModelEvidenceFreshFor(outcome.LastObservedAt, now, settings.ScoreDecayMinutes)
	switch mode {
	case autoModelRequestStream:
		view.LatencyMS = outcome.StreamLatencyMS
		view.LatencySamples = outcome.StreamLatencySamples
		view.LatencyUpdatedAt = outcome.StreamLatencyUpdatedAt
	default:
		view.LatencyMS = outcome.NonStreamLatencyMS
		view.LatencySamples = outcome.NonStreamLatencySamples
		view.LatencyUpdatedAt = outcome.NonStreamLatencyUpdatedAt
	}
	view.LatencyKnown = view.LatencySamples >= autoModelMinLatencySamples &&
		view.LatencyMS > 0 && autoModelEvidenceFreshFor(view.LatencyUpdatedAt, now, settings.ScoreDecayMinutes)
	return view
}

func updateAutoModelOutcome(current autoModelOutcome, exists, success, stream, latencyKnown bool, latencyMs int64, observedAt time.Time) autoModelOutcome {
	if !exists {
		current = autoModelOutcome{}
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	previousObservedAt := current.LastObservedAt
	// 并发请求的 flush 可能乱序发布:较早观测晚到,不得把证据时间拨回过去。
	effectiveAt := observedAt
	if effectiveAt.Before(previousObservedAt) {
		effectiveAt = previousObservedAt
	}
	// 可靠性计数每次新观测都折叠历史。两类延迟样本只在各自取得新成功延迟时
	// 折叠:另一模式的样本保持原始计数和真实观测时间,由读取方统一衰减。
	// 早先在这里同时衰减两种样本,连续非流式反馈会把流式样本按次数重复扣减。
	reliabilityFactor := autoModelDecayFactor(previousObservedAt, effectiveAt)
	current.SuccessCount *= reliabilityFactor
	current.FailureCount *= reliabilityFactor
	isLatest := !observedAt.Before(previousObservedAt)
	if success {
		current.SuccessCount++
	} else {
		current.FailureCount++
		if isLatest {
			current.PendingRecovery = true
		}
	}
	if observedAt.After(current.LastObservedAt) {
		current.LastObservedAt = observedAt
	}
	if success && latencyKnown && latencyMs > 0 {
		latency := float64(latencyMs)
		if stream {
			latencyAt := observedAt
			if latencyAt.Before(current.StreamLatencyUpdatedAt) {
				latencyAt = current.StreamLatencyUpdatedAt
			}
			current.StreamLatencySamples *= autoModelDecayFactor(current.StreamLatencyUpdatedAt, latencyAt)
			if current.StreamLatencySamples < 1e-9 || current.StreamLatencyMS == 0 {
				current.StreamLatencyMS = latency
			} else {
				current.StreamLatencyMS = current.StreamLatencyMS*(1-autoModelLatencyAlpha) + latency*autoModelLatencyAlpha
			}
			current.StreamLatencySamples++
			current.StreamLatencyUpdatedAt = latencyAt
		} else {
			latencyAt := observedAt
			if latencyAt.Before(current.NonStreamLatencyUpdatedAt) {
				latencyAt = current.NonStreamLatencyUpdatedAt
			}
			current.NonStreamLatencySamples *= autoModelDecayFactor(current.NonStreamLatencyUpdatedAt, latencyAt)
			if current.NonStreamLatencySamples < 1e-9 || current.NonStreamLatencyMS == 0 {
				current.NonStreamLatencyMS = latency
			} else {
				current.NonStreamLatencyMS = current.NonStreamLatencyMS*(1-autoModelLatencyAlpha) + latency*autoModelLatencyAlpha
			}
			current.NonStreamLatencySamples++
			current.NonStreamLatencyUpdatedAt = latencyAt
		}
		if isLatest {
			current.PendingRecovery = latencyMs > autoModelSlowLatencyMs
		}
	}
	capAutoModelEvidence(&current)
	// 每次真实观测都产生一个新快照:即使它落在与上一次相同的时间刻度上,序号
	// 也让"后来者胜"成立。LastObservedAt 仍是真实观测时间,不在保存时刷新。
	current.ObservationSeq, current.WriterID = nextAutoModelObservationSeq(current)
	return current
}
