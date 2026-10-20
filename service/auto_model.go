package service

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// autoModelOutcome 是一个被动健康摘要,粒度为 (分组, 模型, 渠道) 的模型层证据。
// 数据仅来自真实请求的成功/失败反馈,绝不发送主动测活请求。
//
// ObservationSeq/WriterID 是快照排序元数据,与持久化行一一对应:UpdatedAt 保留
// 真实观测时间(衰减与恢复都读它),而同一时间刻度上的先后由(序号,实例标识)
// 给出确定性顺序。两者都只在评分锁内更新,见 nextAutoModelObservationSeq。
type autoModelOutcome struct {
	Score          float64
	LatencyMS      float64
	Observations   float64
	UpdatedAt      time.Time
	ObservationSeq int64
	WriterID       string
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

// autoModelHalfLifeMs 用于读取时的评分时间衰减。
//
// 它必须长于抖动冷却窗口(15 分钟),否则冷却到期时评分已经归零回到中立 0.5,
// 组合带着"零记忆"重回候选池,惩罚与教训脱节;评分持久化也只剩"重启很快"这一种
// 场景有意义。默认 30 分钟,可在面板调整(AutoModelScoreDecayMinutes)。
func autoModelHalfLifeMs() int64 {
	return int64(operation_setting.AutoModelScoreDecayMinutes()) * 60 * 1000
}

// autoModelLatencyAlpha 是延迟 EWMA 平滑系数，沿用现有的 0.2 / 0.8。
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

// RecordAutoModelOutcome 记录一次已观察到的请求结果（被动反馈），粒度是
// (分组, 模型, 渠道)。latencyMs <= 0 表示未采集到延迟。
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

// RecordAutoModelTestOutcome 把「渠道手动测试(单个/批量测试)」的结果并入 auto
// 健康体系:成功提分并清除既有冷却;失败按与真实流量相同的判据分档——命中
// 管理员配置的确定性关键词的组合被直接摘除(从渠道模型列表删除,不再有冷却),
// 其余按抖动阶梯冷却并扣分。
//
// 与真实流量反馈共用同一套评分与冷却存储,一点差异:对渠道声明的每一个分组
// 各记一条(打分键是 分组+模型+渠道)。
//
// 真实流量路径不经过这里,原有逻辑不变。定时渠道测试(若管理员开启)同样汇入。
func RecordAutoModelTestOutcome(channel *model.Channel, modelName string, success bool, elapsedMs int64, testErr *types.NewAPIError) {
	if channel == nil || !operation_setting.AutoModelEnabled {
		return
	}
	name := strings.TrimSpace(modelName)
	if name == "" || operation_setting.IsAutoModelName(name) {
		return
	}
	for _, group := range strings.Split(channel.Group, ",") {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		if !success && testErr != nil && AutoModelPermanentFailure(testErr) {
			reason := testErr.Error()
			if runes := []rune(reason); len(runes) > 200 {
				reason = string(runes[:200])
			}
			RecordAutoModelPermanentFailure(group, name, channel.Id, "手动测试失败: "+reason)
			continue
		}
		RecordAutoModelOutcome(group, name, channel.Id, success, elapsedMs, elapsedMs)
	}
}

func recordAutoModelOutcome(group, name string, channelID int, success bool, latencyMs int64, elapsedMs int64, modelOnlyCooldown bool, cooldownReason string) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 || operation_setting.IsAutoModelName(name) {
		return
	}
	key := autoModelHealthKey(group, name, channelID)
	autoModelScoreMutex.Lock()
	now := time.Now()
	autoModelHealth.Update(key, func(current autoModelOutcome, exists bool) autoModelOutcome {
		return updateAutoModelOutcome(current, exists, success, latencyMs, now)
	})
	autoModelScoreMutex.Unlock()
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
	ageFactor := 1.0 - math.Max(0, float64(now.Sub(outcome.UpdatedAt).Milliseconds()))/float64(autoModelHalfLifeMs())
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
	// 每次真实观测都产生一个新快照:即使它落在与上一次相同的时间刻度上,序号
	// 也让"后来者胜"成立。UpdatedAt 仍是真实观测时间,不在保存时刷新。
	current.ObservationSeq, current.WriterID = nextAutoModelObservationSeq(current)
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

// GetAutoModelCandidates exposes the unique models in a fresh priority →
// model-score-band plan for one concrete group. Models with no enabled,
// non-cooling route (or an explicit zero model weight) are absent, including
// when every model is cooling.
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
