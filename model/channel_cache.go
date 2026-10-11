package model

import (
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

var group2model2channels map[string]map[string][]int // enabled channel
var channelsIDM map[int]*Channel                     // all channels include disabled
var channelSyncLock sync.RWMutex

func InitChannelCache() {
	if !common.MemoryCacheEnabled {
		return
	}
	newChannelId2channel := make(map[int]*Channel)
	var channels []*Channel
	DB.Find(&channels)
	for _, channel := range channels {
		newChannelId2channel[channel.Id] = channel
	}
	var abilities []*Ability
	DB.Find(&abilities)
	groups := make(map[string]bool)
	for _, ability := range abilities {
		groups[ability.Group] = true
	}
	newGroup2model2channels := make(map[string]map[string][]int)
	for group := range groups {
		newGroup2model2channels[group] = make(map[string][]int)
	}
	for _, channel := range channels {
		if channel.Status != common.ChannelStatusEnabled {
			continue // skip disabled channels
		}
		groups := strings.Split(channel.Group, ",")
		for _, group := range groups {
			models := strings.Split(channel.Models, ",")
			for _, model := range models {
				if _, ok := newGroup2model2channels[group][model]; !ok {
					newGroup2model2channels[group][model] = make([]int, 0)
				}
				newGroup2model2channels[group][model] = append(newGroup2model2channels[group][model], channel.Id)
			}
		}
	}

	// sort by priority
	for group, model2channels := range newGroup2model2channels {
		for model, channels := range model2channels {
			sort.Slice(channels, func(i, j int) bool {
				return newChannelId2channel[channels[i]].GetPriority() > newChannelId2channel[channels[j]].GetPriority()
			})
			newGroup2model2channels[group][model] = channels
		}
	}

	channelSyncLock.Lock()
	group2model2channels = newGroup2model2channels
	//channelsIDM = newChannelId2channel
	for i, channel := range newChannelId2channel {
		if channel.ChannelInfo.IsMultiKey {
			channel.Keys = channel.GetKeys()
			if channel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
				if oldChannel, ok := channelsIDM[i]; ok {
					// 存在旧的渠道，如果是多key且轮询，保留轮询索引信息
					if oldChannel.ChannelInfo.IsMultiKey && oldChannel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
						channel.ChannelInfo.MultiKeyPollingIndex = oldChannel.ChannelInfo.MultiKeyPollingIndex
					}
				}
			}
		}
	}
	channelsIDM = newChannelId2channel
	channelSyncLock.Unlock()
	common.SysLog("channels synced from database")
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		common.SysLog("syncing channels from database")
		InitChannelCache()
	}
}

// GetRandomSatisfiedChannelWithUsedFallback 在完整优先级列表上定层，层内按权重加权随机。
// 层内挑选策略的定制（例如 auto 路由按健康分优先）走 ChooseSatisfiedChannelByScore。
func GetRandomSatisfiedChannelWithUsedFallback(group string, modelName string, retry int, hardExcluded, usedChannelIDs map[int]bool) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelWithUsedFallback(group, modelName, retry, hardExcluded, usedChannelIDs)
	}
	targets, err := tierChannelTargets(group, modelName, retry, hardExcluded, usedChannelIDs)
	if err != nil || len(targets) == 0 {
		return nil, err
	}
	return chooseWeightedChannel(targets)
}

// ChooseSatisfiedChannelByScore 在同一优先级层里按健康分挑渠道：分数最高的一档优先，
// 档内才按权重加权随机。两条实现路径都照顾到：
//   - 内存渠道缓存（MEMORY_CACHE_ENABLED=true）：层内候选取自缓存，权重用渠道权重；
//   - 直连数据库（默认路径，线上就是这条）：层内候选取自 abilities，权重用 ability 权重。
//
// 各自的权重语义保持原样，只是把"层内直接随机"换成"层内先看分数"。
// score 返回该 (模型, 渠道) 的健康分；未观测应回中性 0.5。score 为 nil 时退化为原逻辑。
func ChooseSatisfiedChannelByScore(group string, modelName string, retry int, hardExcluded, usedChannelIDs map[int]bool, score func(channelID int) float64, tieEpsilon float64) (*Channel, error) {
	if score == nil {
		return GetRandomSatisfiedChannelWithUsedFallback(group, modelName, retry, hardExcluded, usedChannelIDs)
	}
	if !common.MemoryCacheEnabled {
		return chooseAbilityByScore(group, modelName, retry, hardExcluded, usedChannelIDs, score, tieEpsilon)
	}
	targets, err := tierChannelTargets(group, modelName, retry, hardExcluded, usedChannelIDs)
	if err != nil || len(targets) == 0 {
		return nil, err
	}
	return ChooseChannelByScore(targets, score, tieEpsilon)
}

// tierChannelTargets 返回该 (分组, 模型) 在 retry 指定的优先级层里可用的渠道（内存缓存路径）。
//
// 层级语义（与原 GetRandomSatisfiedChannelWithUsedFallback 完全一致）：
//   - retry 从 0 开始，对应优先级从高到低；越界钳到最后一层。
//   - 先排除 usedChannelIDs；若该层渠道都被用过，且不是最后一层，返回空让外层继续下一层；
//     只有最后一层才允许重用已试渠道。
func tierChannelTargets(group string, modelName string, retry int, hardExcluded, usedChannelIDs map[int]bool) ([]*Channel, error) {
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	channels := filterExcludedChannelIDs(group2model2channels[group][modelName], hardExcluded)
	if len(channels) == 0 {
		normalized := ratio_setting.FormatMatchingModelName(modelName)
		channels = filterExcludedChannelIDs(group2model2channels[group][normalized], hardExcluded)
	}
	if len(channels) == 0 {
		return nil, nil
	}

	priorities := make([]int, 0)
	seen := make(map[int]bool)
	for _, id := range channels {
		channel, ok := channelsIDM[id]
		if !ok {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", id)
		}
		p := int(channel.GetPriority())
		if !seen[p] {
			seen[p] = true
			priorities = append(priorities, p)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))
	targetIndex := retry
	if targetIndex >= len(priorities) {
		targetIndex = len(priorities) - 1
	}
	targetPriority := int64(priorities[targetIndex])

	var target []*Channel
	for _, id := range channels {
		channel := channelsIDM[id]
		if channel.GetPriority() == targetPriority && !usedChannelIDs[id] {
			target = append(target, channel)
		}
	}
	if len(target) == 0 {
		if targetIndex != len(priorities)-1 {
			return nil, nil
		}
		for _, id := range channels {
			channel := channelsIDM[id]
			if channel.GetPriority() == targetPriority {
				target = append(target, channel)
			}
		}
	}
	return target, nil
}

func chooseWeightedChannel(targetChannels []*Channel) (*Channel, error) {
	if len(targetChannels) == 0 {
		return nil, nil
	}
	var sumWeight int
	for _, channel := range targetChannels {
		sumWeight += channel.GetWeight()
	}
	smoothingFactor, smoothingAdjustment := 1, 0
	if sumWeight == 0 {
		sumWeight = len(targetChannels) * 100
		smoothingAdjustment = 100
	} else if sumWeight/len(targetChannels) < 10 {
		smoothingFactor = 100
	}
	randomWeight := rand.Intn(sumWeight * smoothingFactor)
	for _, channel := range targetChannels {
		randomWeight -= channel.GetWeight()*smoothingFactor + smoothingAdjustment
		if randomWeight < 0 {
			return channel, nil
		}
	}
	return nil, errors.New("channel not found")
}

// ChooseChannelByScore 在候选渠道里按健康分挑，规则与 auto 挑模型一致：
// 分数最高的一档优先，档内（分数差在 tieEpsilon 以内）按渠道权重加权随机。
// score 返回该 (模型, 渠道) 的当前分数；未观测的应返回中性 0.5。
// score 为 nil 时退化为原来的纯权重加权随机。
func ChooseChannelByScore(targets []*Channel, score func(channelID int) float64, tieEpsilon float64) (*Channel, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	if score == nil {
		return chooseWeightedChannel(targets)
	}
	best := 0.0
	haveBest := false
	for _, channel := range targets {
		s := score(channel.Id)
		if !haveBest || s > best {
			best, haveBest = s, true
		}
	}
	eligible := make([]*Channel, 0, len(targets))
	for _, channel := range targets {
		if !haveBest || score(channel.Id) >= best-tieEpsilon {
			eligible = append(eligible, channel)
		}
	}
	if len(eligible) == 0 {
		eligible = targets
	}
	return chooseWeightedChannel(eligible)
}

func GetRandomSatisfiedChannel(group string, model string, retry int) (*Channel, error) {
	return GetRandomSatisfiedChannelWithExclusions(group, model, retry, nil)
}

func GetRandomSatisfiedChannelWithExclusions(group string, model string, retry int, excludedChannelIDs map[int]bool) (*Channel, error) {
	// if memory cache is disabled, get channel directly from database
	if !common.MemoryCacheEnabled {
		return GetChannelWithExclusions(group, model, retry, excludedChannelIDs)
	}

	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	// First, try to find channels with the exact model name.
	channels := filterExcludedChannelIDs(group2model2channels[group][model], excludedChannelIDs)

	// If no channels found, try to find channels with the normalized model name.
	if len(channels) == 0 {
		normalizedModel := ratio_setting.FormatMatchingModelName(model)
		channels = filterExcludedChannelIDs(group2model2channels[group][normalizedModel], excludedChannelIDs)
	}

	if len(channels) == 0 {
		return nil, nil
	}

	if len(channels) == 1 {
		if channel, ok := channelsIDM[channels[0]]; ok {
			return channel, nil
		}
		return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channels[0])
	}

	uniquePriorities := make(map[int]bool)
	for _, channelId := range channels {
		if channel, ok := channelsIDM[channelId]; ok {
			uniquePriorities[int(channel.GetPriority())] = true
		} else {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channelId)
		}
	}
	var sortedUniquePriorities []int
	for priority := range uniquePriorities {
		sortedUniquePriorities = append(sortedUniquePriorities, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(sortedUniquePriorities)))

	if retry >= len(uniquePriorities) {
		retry = len(uniquePriorities) - 1
	}
	targetPriority := int64(sortedUniquePriorities[retry])

	// get the priority for the given retry number
	var sumWeight = 0
	var targetChannels []*Channel
	for _, channelId := range channels {
		if channel, ok := channelsIDM[channelId]; ok {
			if channel.GetPriority() == targetPriority {
				sumWeight += channel.GetWeight()
				targetChannels = append(targetChannels, channel)
			}
		} else {
			return nil, fmt.Errorf("数据库一致性错误，渠道# %d 不存在，请联系管理员修复", channelId)
		}
	}

	if len(targetChannels) == 0 {
		return nil, errors.New(fmt.Sprintf("no channel found, group: %s, model: %s, priority: %d", group, model, targetPriority))
	}

	// smoothing factor and adjustment
	smoothingFactor := 1
	smoothingAdjustment := 0

	if sumWeight == 0 {
		// when all channels have weight 0, set sumWeight to the number of channels and set smoothing adjustment to 100
		// each channel's effective weight = 100
		sumWeight = len(targetChannels) * 100
		smoothingAdjustment = 100
	} else if sumWeight/len(targetChannels) < 10 {
		// when the average weight is less than 10, set smoothing factor to 100
		smoothingFactor = 100
	}

	// Calculate the total weight of all channels up to endIdx
	totalWeight := sumWeight * smoothingFactor

	// Generate a random value in the range [0, totalWeight)
	randomWeight := rand.Intn(totalWeight)

	// Find a channel based on its weight
	for _, channel := range targetChannels {
		randomWeight -= channel.GetWeight()*smoothingFactor + smoothingAdjustment
		if randomWeight < 0 {
			return channel, nil
		}
	}
	// return null if no channel is not found
	return nil, errors.New("channel not found")
}

func filterExcludedChannelIDs(channelIDs []int, excludedChannelIDs map[int]bool) []int {
	if len(channelIDs) == 0 || len(excludedChannelIDs) == 0 {
		return channelIDs
	}
	filtered := make([]int, 0, len(channelIDs))
	for _, channelID := range channelIDs {
		if excludedChannelIDs[channelID] {
			continue
		}
		filtered = append(filtered, channelID)
	}
	return filtered
}

func CacheGetChannel(id int) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	return c, nil
}

func CacheGetChannelInfo(id int) (*ChannelInfo, error) {
	if !common.MemoryCacheEnabled {
		channel, err := GetChannelById(id, true)
		if err != nil {
			return nil, err
		}
		return &channel.ChannelInfo, nil
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("渠道# %d，已不存在", id)
	}
	return &c.ChannelInfo, nil
}

func CacheUpdateChannelStatus(id int, status int) {
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	if channel, ok := channelsIDM[id]; ok {
		channel.Status = status
	}
	if status != common.ChannelStatusEnabled {
		// delete the channel from group2model2channels
		for group, model2channels := range group2model2channels {
			for model, channels := range model2channels {
				for i, channelId := range channels {
					if channelId == id {
						// remove the channel from the slice
						group2model2channels[group][model] = append(channels[:i], channels[i+1:]...)
						break
					}
				}
			}
		}
	}
}

func CacheUpdateChannel(channel *Channel) {
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	if channel == nil {
		return
	}

	println("CacheUpdateChannel:", channel.Id, channel.Name, channel.Status, channel.ChannelInfo.MultiKeyPollingIndex)

	println("before:", channelsIDM[channel.Id].ChannelInfo.MultiKeyPollingIndex)
	channelsIDM[channel.Id] = channel
	println("after :", channelsIDM[channel.Id].ChannelInfo.MultiKeyPollingIndex)
}
