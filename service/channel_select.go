package service

import (
	"errors"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

const channelDailySuccessLimitSkippedIDsKey = "channel_daily_success_limit_skipped_ids"
const channelRPMLimitSkippedIDsKey = "channel_rpm_limit_skipped_ids"

type RetryParam struct {
	Ctx                 *gin.Context
	TokenGroup          string
	ModelName           string
	Retry               *int
	EffectiveRetryTimes *int
	resetNextTry        bool
}

func MarkChannelDailySuccessLimitSkipped(c *gin.Context, channelId int) {
	if c == nil || channelId <= 0 {
		return
	}
	skipped := GetChannelDailySuccessLimitSkippedIDs(c)
	skipped[channelId] = true
	c.Set(channelDailySuccessLimitSkippedIDsKey, skipped)
}

func IsChannelDailySuccessLimitSkipped(c *gin.Context, channelId int) bool {
	if c == nil || channelId <= 0 {
		return false
	}
	skipped := GetChannelDailySuccessLimitSkippedIDs(c)
	return skipped[channelId]
}

func HasChannelDailySuccessLimitSkipped(c *gin.Context) bool {
	return len(GetChannelDailySuccessLimitSkippedIDs(c)) > 0
}

func GetChannelDailySuccessLimitSkippedIDs(c *gin.Context) map[int]bool {
	return getChannelSkippedIDs(c, channelDailySuccessLimitSkippedIDsKey)
}

func MarkChannelRPMLimitSkipped(c *gin.Context, channelId int) {
	if c == nil || channelId <= 0 {
		return
	}
	skipped := GetChannelRPMLimitSkippedIDs(c)
	skipped[channelId] = true
	c.Set(channelRPMLimitSkippedIDsKey, skipped)
}

func IsChannelRPMLimitSkipped(c *gin.Context, channelId int) bool {
	return GetChannelRPMLimitSkippedIDs(c)[channelId]
}

func HasChannelRPMLimitSkipped(c *gin.Context) bool {
	return len(GetChannelRPMLimitSkippedIDs(c)) > 0
}

func GetChannelRPMLimitSkippedIDs(c *gin.Context) map[int]bool {
	return getChannelSkippedIDs(c, channelRPMLimitSkippedIDsKey)
}

// maxChannelAttemptsPerRequest 同一个渠道在一次请求里最多尝试的次数。
// 排队型/免费上游（例如上游返回"排队中，请 30 秒后重试"）重试同一个渠道不会有任何
// 收益：实测有一次请求对同一个渠道重试了 51 次、白等两分钟，最后客户端超时断开。
const maxChannelAttemptsPerRequest = 2

func GetChannelSelectionExcludedIDs(c *gin.Context) map[int]bool {
	excluded := GetChannelDailySuccessLimitSkippedIDs(c)
	for id, skipped := range GetChannelRPMLimitSkippedIDs(c) {
		if skipped {
			excluded[id] = true
		}
	}
	// 试满上限的渠道不再参与选择；如果该模型只剩这些渠道，选择会返回空，
	// 由上层决定是换候选模型（auto）还是把真实的上游报错返回。
	for id, count := range countChannelAttempts(c) {
		if count >= maxChannelAttemptsPerRequest {
			excluded[id] = true
		}
	}
	return excluded
}

// countChannelAttempts 统计本次请求里每个渠道已经被尝试的次数（数据来自 use_channel）。
func countChannelAttempts(c *gin.Context) map[int]int {
	counts := map[int]int{}
	if c == nil {
		return counts
	}
	for _, raw := range c.GetStringSlice("use_channel") {
		id, err := strconv.Atoi(raw)
		if err != nil {
			continue
		}
		counts[id]++
	}
	return counts
}

// GetUsedChannelIDs 返回"本次请求已经打过（并失败）的渠道"。
// 数据来自 controller.addUsedChannel 维护的 use_channel 上下文。
func GetUsedChannelIDs(c *gin.Context) map[int]bool {
	result := map[int]bool{}
	if c == nil {
		return result
	}
	for _, raw := range c.GetStringSlice("use_channel") {
		id, err := strconv.Atoi(raw)
		if err != nil {
			continue
		}
		result[id] = true
	}
	return result
}

// selectChannelWithUsedFallback 在完整优先级列表上先确定目标层，再排除已用渠道。
// 中间层若已无未用渠道，返回 nil 让外层 retry 继续到下一优先级；
// 只有目标层是最后一层且未用渠道耗尽时，才允许重用已试渠道。
func selectChannelWithUsedFallback(param *RetryParam, group string, modelName string, retry int, hardExcluded map[int]bool) (*model.Channel, error) {
	used := GetUsedChannelIDs(param.Ctx)
	var usedIDs map[int]bool
	if len(used) > 0 {
		usedIDs = used
	}
	// auto 路由：同一优先级层内按健康分优先挑渠道（分数并列的档内按权重加权随机），
	// 而不是纯权重随机——纯权重会一直给已知很慢的渠道派流量。
	if scoreFn := autoModelChannelScoreFnForGroup(param.Ctx, group, modelName); scoreFn != nil {
		cooling := GetAutoModelCoolingChannelIDs(group, modelName)
		if len(cooling) > 0 {
			targets, err := model.GetAutoModelRoutingTargets(group)
			if err != nil {
				return nil, err
			}
			modelTargets := targets[modelName]
			if len(modelTargets) == 0 {
				modelTargets = targets[ratio_setting.FormatMatchingModelName(modelName)]
			}
			hardExcluded = autoModelChannelExclusions(hardExcluded, cooling, modelTargets)
		}
		return model.ChooseSatisfiedChannelByScore(group, modelName, retry, hardExcluded, usedIDs, scoreFn, autoScoreTieEpsilon)
	}
	if len(used) == 0 {
		return model.GetRandomSatisfiedChannelWithExclusions(group, modelName, retry, hardExcluded)
	}
	return model.GetRandomSatisfiedChannelWithUsedFallback(group, modelName, retry, hardExcluded, used)
}

// autoModelChannelExclusions excludes cooling routes before priority selection.
// Preserve the existing availability-first policy only when every otherwise
// eligible route is cooling; hard exclusions are never relaxed.
func autoModelChannelExclusions(hardExcluded, cooling map[int]bool, targets []model.AutoModelRoutingTarget) map[int]bool {
	hasHealthy := false
	for _, target := range targets {
		if !hardExcluded[target.ChannelID] && !cooling[target.ChannelID] {
			hasHealthy = true
			break
		}
	}
	if !hasHealthy {
		return hardExcluded
	}
	excluded := make(map[int]bool, len(hardExcluded)+len(cooling))
	for id, blocked := range hardExcluded {
		excluded[id] = blocked
	}
	for id, blocked := range cooling {
		if blocked {
			excluded[id] = true
		}
	}
	return excluded
}

// ShouldAvoidAutoModelAffinity keeps affinity from bypassing model/channel cooldown.
// The normal selector decides whether all-cooling fallback is necessary.
func ShouldAvoidAutoModelAffinity(c *gin.Context, group, modelName string, channelID int) bool {
	return c != nil && common.GetContextKeyString(c, constant.ContextKeyAutoModelClientName) != "" &&
		GetAutoModelCoolingChannelIDs(group, modelName)[channelID]
}

// autoModelChannelScoreFn retains the context-based helper for callers with a
// resolved group. Cross-group selection uses its actual group explicitly.
func autoModelChannelScoreFn(c *gin.Context, modelName string) func(int) float64 {
	if c == nil {
		return nil
	}
	return autoModelChannelScoreFnForGroup(c, common.GetContextKeyString(c, constant.ContextKeyUsingGroup), modelName)
}

// Each channel-selection operation uses one health snapshot, not a request-wide
// snapshot: retries may observe feedback from other completed requests.
func autoModelChannelScoreFnForGroup(c *gin.Context, group, modelName string) func(int) float64 {
	if c == nil || modelName == "" || group == "" {
		return nil
	}
	if common.GetContextKeyString(c, constant.ContextKeyAutoModelClientName) == "" {
		return nil
	}
	health := autoModelHealth.ReadAll()
	now := time.Now()
	return func(channelID int) float64 {
		outcome, ok := health[autoModelHealthKey(group, modelName, channelID)]
		if !ok {
			return 0.5
		}
		return effectiveScore(outcome, now)
	}
}

func getChannelSkippedIDs(c *gin.Context, key string) map[int]bool {
	if c == nil {
		return map[int]bool{}
	}
	value, ok := c.Get(key)
	if !ok {
		return map[int]bool{}
	}
	source, ok := value.(map[int]bool)
	if !ok {
		return map[int]bool{}
	}
	target := make(map[int]bool, len(source))
	for id, skipped := range source {
		if skipped {
			target[id] = true
		}
	}
	return target
}

func (p *RetryParam) GetRetry() int {
	if p.Retry == nil {
		return 0
	}
	return *p.Retry
}

func (p *RetryParam) SetRetry(retry int) {
	p.Retry = &retry
}

func (p *RetryParam) IncreaseRetry() {
	if p.resetNextTry {
		p.resetNextTry = false
		return
	}
	if p.Retry == nil {
		p.Retry = new(int)
	}
	*p.Retry++
}

func (p *RetryParam) ResetRetryNextTry() {
	p.resetNextTry = true
}

func (p *RetryParam) GetEffectiveRetryTimes() int {
	if p.EffectiveRetryTimes == nil {
		return common.RetryTimes
	}
	return *p.EffectiveRetryTimes
}

func (p *RetryParam) SetEffectiveRetryTimes(retryTimes int) {
	if retryTimes < 0 {
		retryTimes = 0
	}
	p.EffectiveRetryTimes = &retryTimes
}

func (p *RetryParam) SetEffectiveRetryTimesFromChannel(channel *model.Channel) {
	if channel == nil || channel.RetryTimes == nil {
		p.EffectiveRetryTimes = nil
		return
	}
	p.SetEffectiveRetryTimes(*channel.RetryTimes)
}

func (p *RetryParam) GetRemainingRetryTimes() int {
	return p.GetEffectiveRetryTimes() - p.GetRetry()
}

// CacheGetRandomSatisfiedChannel tries to get a random channel that satisfies the requirements.
// 尝试获取一个满足要求的随机渠道。
//
// For "auto" tokenGroup with cross-group Retry enabled:
// 对于启用了跨分组重试的 "auto" tokenGroup：
//
//   - Each group will exhaust all its priorities before moving to the next group.
//     每个分组会用完所有优先级后才会切换到下一个分组。
//
//   - Uses ContextKeyAutoGroupIndex to track current group index.
//     使用 ContextKeyAutoGroupIndex 跟踪当前分组索引。
//
//   - Uses ContextKeyAutoGroupRetryIndex to track the global Retry count when current group started.
//     使用 ContextKeyAutoGroupRetryIndex 跟踪当前分组开始时的全局重试次数。
//
//   - priorityRetry = Retry - startRetryIndex, represents the priority level within current group.
//     priorityRetry = Retry - startRetryIndex，表示当前分组内的优先级级别。
//
//   - When GetRandomSatisfiedChannel returns nil (priorities exhausted), moves to next group.
//     当 GetRandomSatisfiedChannel 返回 nil（优先级用完）时，切换到下一个分组。
//
// Example flow (2 groups, each with 2 priorities, RetryTimes=3):
// 示例流程（2个分组，每个有2个优先级，RetryTimes=3）：
//
//	Retry=0: GroupA, priority0 (startRetryIndex=0, priorityRetry=0)
//	         分组A, 优先级0
//
//	Retry=1: GroupA, priority1 (startRetryIndex=0, priorityRetry=1)
//	         分组A, 优先级1
//
//	Retry=2: GroupA exhausted → GroupB, priority0 (startRetryIndex=2, priorityRetry=0)
//	         分组A用完 → 分组B, 优先级0
//
//	Retry=3: GroupB, priority1 (startRetryIndex=2, priorityRetry=1)
//	         分组B, 优先级1
func CacheGetRandomSatisfiedChannel(param *RetryParam) (*model.Channel, string, error) {
	var channel *model.Channel
	var err error
	selectGroup := param.TokenGroup
	userGroup := common.GetContextKeyString(param.Ctx, constant.ContextKeyUserGroup)
	excludedChannelIDs := GetChannelSelectionExcludedIDs(param.Ctx)

	if param.TokenGroup == "auto" {
		candidateGroups := GetRequestGroupCandidates(param.Ctx, userGroup, param.TokenGroup)
		if len(candidateGroups) == 0 {
			return nil, selectGroup, errors.New("auto groups is not enabled")
		}

		// startGroupIndex: the group index to start searching from
		// startGroupIndex: 开始搜索的分组索引
		startGroupIndex := 0
		crossGroupRetry := common.GetContextKeyBool(param.Ctx, constant.ContextKeyTokenCrossGroupRetry)

		if lastGroupIndex, exists := common.GetContextKey(param.Ctx, constant.ContextKeyAutoGroupIndex); exists {
			if idx, ok := lastGroupIndex.(int); ok {
				startGroupIndex = idx
			}
		}
		selectedGroup := common.GetContextKeyString(param.Ctx, constant.ContextKeyAutoGroup)
		if selectedGroup != "" && !crossGroupRetry {
			for i, group := range candidateGroups {
				if group == selectedGroup {
					startGroupIndex = i
					break
				}
			}
		}

		for i := startGroupIndex; i < len(candidateGroups); i++ {
			autoGroup := candidateGroups[i]
			// Calculate priorityRetry for current group
			// 计算当前分组的 priorityRetry
			priorityRetry := param.GetRetry()
			// If moved to a new group, reset priorityRetry and update startRetryIndex
			// 如果切换到新分组，重置 priorityRetry 并更新 startRetryIndex
			if i > startGroupIndex {
				priorityRetry = 0
			}
			logger.LogDebug(param.Ctx, "Auto selecting group: %s, priorityRetry: %d", autoGroup, priorityRetry)

			channel, _ = selectChannelWithUsedFallback(param, autoGroup, param.ModelName, priorityRetry, excludedChannelIDs)
			if channel == nil {
				// Before the first channel is chosen, unsupported groups may be
				// skipped regardless of the retry switch. Once a group has handled
				// the request, disabled cross-group retry keeps subsequent retries
				// pinned to that group.
				if selectedGroup != "" && !crossGroupRetry {
					break
				}
				// Current group has no available channel for this model, try next group
				// 当前分组没有该模型的可用渠道，尝试下一个分组
				logger.LogDebug(param.Ctx, "No available channel in group %s for model %s at priorityRetry %d, trying next group", autoGroup, param.ModelName, priorityRetry)
				// 重置状态以尝试下一个分组
				common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroupIndex, i+1)
				common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroupRetryIndex, 0)
				// Reset retry counter so outer loop can continue for next group
				// 重置重试计数器，以便外层循环可以为下一个分组继续
				param.SetRetry(0)
				continue
			}
			common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroup, autoGroup)
			selectGroup = autoGroup
			logger.LogDebug(param.Ctx, "Auto selected group: %s", autoGroup)

			// Prepare state for next retry
			// 为下一次重试准备状态
			if crossGroupRetry && priorityRetry >= param.GetEffectiveRetryTimes() {
				// Current group has exhausted all retries, prepare to switch to next group
				// This request still uses current group, but next retry will use next group
				// 当前分组已用完所有重试次数，准备切换到下一个分组
				// 本次请求仍使用当前分组，但下次重试将使用下一个分组
				logger.LogDebug(param.Ctx, "Current group %s retries exhausted (priorityRetry=%d >= RetryTimes=%d), preparing switch to next group for next retry", autoGroup, priorityRetry, param.GetEffectiveRetryTimes())
				common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroupIndex, i+1)
				// Reset retry counter so outer loop can continue for next group
				// 重置重试计数器，以便外层循环可以为下一个分组继续
				param.SetRetry(0)
				param.ResetRetryNextTry()
			} else {
				// Stay in current group, save current state
				// 保持在当前分组，保存当前状态
				common.SetContextKey(param.Ctx, constant.ContextKeyAutoGroupIndex, i)
			}
			break
		}
	} else {
		channel, err = selectChannelWithUsedFallback(param, param.TokenGroup, param.ModelName, param.GetRetry(), excludedChannelIDs)
		if err != nil {
			return nil, param.TokenGroup, err
		}
	}
	return channel, selectGroup, nil
}
