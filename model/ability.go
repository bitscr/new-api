package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"

	"github.com/samber/lo"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Ability struct {
	Group     string  `json:"group" gorm:"type:varchar(64);primaryKey;autoIncrement:false"`
	Model     string  `json:"model" gorm:"type:varchar(255);primaryKey;autoIncrement:false"`
	ChannelId int     `json:"channel_id" gorm:"primaryKey;autoIncrement:false;index"`
	Enabled   bool    `json:"enabled"`
	Priority  *int64  `json:"priority" gorm:"bigint;default:0;index"`
	Weight    uint    `json:"weight" gorm:"default:0;index"`
	Tag       *string `json:"tag" gorm:"index"`
}

type AbilityWithChannel struct {
	Ability
	ChannelType int `json:"channel_type"`
}

func GetAllEnableAbilityWithChannels() ([]AbilityWithChannel, error) {
	var abilities []AbilityWithChannel
	err := DB.Table("abilities").
		Select("abilities.*, channels.type as channel_type").
		Joins("left join channels on abilities.channel_id = channels.id").
		Where("abilities.enabled = ?", true).
		Scan(&abilities).Error
	return abilities, err
}

func GetGroupEnabledModels(group string) []string {
	var models []string
	// Find distinct models
	DB.Table("abilities").Where(commonGroupCol+" = ? and enabled = ?", group, true).Distinct("model").Pluck("model", &models)
	return models
}

// GetGroupModelChannelIDs 返回某分组下每个模型当前可用的渠道 ID 列表。
// 供 auto 候选的冷却判断使用：只有当一个模型的所有渠道都在冷却，才把它剔除。
func GetGroupModelChannelIDs(group string) (map[string][]int, error) {
	type row struct {
		Model     string
		ChannelId int
	}
	var rows []row
	err := DB.Table("abilities").
		Select("model, channel_id").
		Where(commonGroupCol+" = ? and enabled = ?", group, true).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make(map[string][]int, len(rows))
	for _, item := range rows {
		result[item.Model] = append(result[item.Model], item.ChannelId)
	}
	return result, nil
}

func GetEnabledModels() []string {
	var models []string
	// Find distinct models
	DB.Table("abilities").Where("enabled = ?", true).Distinct("model").Pluck("model", &models)
	return models
}

func GetAllEnableAbilities() []Ability {
	var abilities []Ability
	DB.Find(&abilities, "enabled = ?", true)
	return abilities
}

func getPriority(group string, model string, retry int) (int, error) {

	var priorities []int
	err := DB.Model(&Ability{}).
		Select("DISTINCT(priority)").
		Where(clause.Eq{Column: clause.Column{Name: "group"}, Value: group}).
		Where("model = ? and enabled = ?", model, true).
		Order("priority DESC").              // 按优先级降序排序
		Pluck("priority", &priorities).Error // Pluck用于将查询的结果直接扫描到一个切片中

	if err != nil {
		// 处理错误
		return 0, err
	}

	if len(priorities) == 0 {
		// 如果没有查询到优先级，则返回错误
		return 0, errors.New("数据库一致性被破坏")
	}

	// 确定要使用的优先级
	var priorityToUse int
	if retry >= len(priorities) {
		// 如果重试次数大于优先级数，则使用最小的优先级
		priorityToUse = priorities[len(priorities)-1]
	} else {
		priorityToUse = priorities[retry]
	}
	return priorityToUse, nil
}

func getChannelQuery(group string, model string, retry int) (*gorm.DB, error) {
	maxPrioritySubQuery := DB.Model(&Ability{}).Select("MAX(priority)").
		Where(clause.Eq{Column: clause.Column{Name: "group"}, Value: group}).
		Where("model = ? and enabled = ?", model, true)
	channelQuery := DB.Where(clause.Eq{Column: clause.Column{Name: "group"}, Value: group}).
		Where("model = ? and enabled = ? and priority = (?)", model, true, maxPrioritySubQuery)
	if retry != 0 {
		priority, err := getPriority(group, model, retry)
		if err != nil {
			return nil, err
		} else {
			channelQuery = DB.Where(clause.Eq{Column: clause.Column{Name: "group"}, Value: group}).
				Where("model = ? and enabled = ? and priority = ?", model, true, priority)
		}
	}

	return channelQuery, nil
}

func GetChannel(group string, model string, retry int) (*Channel, error) {
	return GetChannelWithExclusions(group, model, retry, nil)
}

// GetChannelWithUsedFallback preserves the priority tiers defined by hardExcluded,
// prefers unused channels inside the selected tier, and reuses channels only on the
// final tier when that tier has no unused channel left.
func GetChannelWithUsedFallback(group string, model string, retry int, hardExcluded, usedChannelIDs map[int]bool) (*Channel, error) {
	abilities, err := tierAbilities(group, model, retry, hardExcluded, usedChannelIDs)
	if err != nil || len(abilities) == 0 {
		return nil, err
	}
	channelID := chooseAbilityByWeight(abilities)
	if channelID <= 0 {
		return nil, nil
	}
	channel := Channel{}
	if err := DB.First(&channel, "id = ?", channelID).Error; err != nil {
		return nil, err
	}
	return &channel, nil
}

// tierAbilities 返回该 (分组, 模型) 在 retry 指定的优先级层里的可用 ability。
// 层级语义与原 GetChannelWithUsedFallback 完全一致：越界钳到最后一层；
// 先排除 hardExcluded，再优先未用过的渠道，只有最后一层才允许重用。
func tierAbilities(group string, model string, retry int, hardExcluded, usedChannelIDs map[int]bool) ([]Ability, error) {
	var abilities []Ability
	if err := DB.Where(clause.Eq{Column: clause.Column{Name: "group"}, Value: group}).
		Where("model = ? and enabled = ?", model, true).
		Order("priority DESC, weight DESC").Find(&abilities).Error; err != nil {
		return nil, err
	}

	available := make([]Ability, 0, len(abilities))
	for _, ability := range abilities {
		if !hardExcluded[ability.ChannelId] {
			available = append(available, ability)
		}
	}
	if len(available) == 0 {
		return nil, nil
	}
	priorities := make(map[int64]bool)
	for _, ability := range available {
		priority := int64(0)
		if ability.Priority != nil {
			priority = *ability.Priority
		}
		priorities[priority] = true
	}
	if retry >= len(priorities) {
		retry = len(priorities) - 1
	}
	abilities = filterExcludedAbilitiesForRetry(available, retry, nil)
	finalTier := retry == len(priorities)-1

	unused := make([]Ability, 0, len(abilities))
	for _, ability := range abilities {
		if !usedChannelIDs[ability.ChannelId] {
			unused = append(unused, ability)
		}
	}
	if len(unused) > 0 {
		return unused, nil
	}
	if !finalTier {
		return nil, nil
	}
	return abilities, nil
}

// chooseAbilityByWeight 在给定 abilities 里按 ability.Weight + 10 加权随机（沿用原逻辑）。
func chooseAbilityByWeight(abilities []Ability) int {
	weightSum := uint(0)
	for _, ability := range abilities {
		weightSum += ability.Weight + 10
	}
	if weightSum == 0 {
		return 0
	}
	weight := common.GetRandomInt(int(weightSum))
	for _, ability := range abilities {
		weight -= int(ability.Weight) + 10
		if weight <= 0 {
			return ability.ChannelId
		}
	}
	return 0
}

// chooseAbilityByScore 与 GetChannelWithUsedFallback 相同，但同一层内先看健康分：
// 分数最高的一档优先，档内才按 ability 权重加权随机。给 auto 路由用（直连数据库路径）。
func chooseAbilityByScore(group string, model string, retry int, hardExcluded, usedChannelIDs map[int]bool, score func(channelID int) float64, tieEpsilon float64) (*Channel, error) {
	abilities, err := tierAbilities(group, model, retry, hardExcluded, usedChannelIDs)
	if err != nil || len(abilities) == 0 {
		return nil, err
	}
	best, haveBest := 0.0, false
	for _, ability := range abilities {
		if s := score(ability.ChannelId); !haveBest || s > best {
			best, haveBest = s, true
		}
	}
	eligible := make([]Ability, 0, len(abilities))
	for _, ability := range abilities {
		if !haveBest || score(ability.ChannelId) >= best-tieEpsilon {
			eligible = append(eligible, ability)
		}
	}
	if len(eligible) == 0 {
		eligible = abilities
	}
	channelID := chooseAbilityByWeight(eligible)
	if channelID <= 0 {
		return nil, nil
	}
	channel := Channel{}
	if err := DB.First(&channel, "id = ?", channelID).Error; err != nil {
		return nil, err
	}
	return &channel, nil
}

func GetChannelWithExclusions(group string, model string, retry int, excludedChannelIDs map[int]bool) (*Channel, error) {
	var abilities []Ability

	var err error = nil
	if len(excludedChannelIDs) > 0 {
		err = DB.Where(clause.Eq{Column: clause.Column{Name: "group"}, Value: group}).
			Where("model = ? and enabled = ?", model, true).
			Order("priority DESC, weight DESC").
			Find(&abilities).Error
	} else {
		channelQuery, queryErr := getChannelQuery(group, model, retry)
		if queryErr != nil {
			return nil, queryErr
		}
		err = channelQuery.Order("weight DESC").Find(&abilities).Error
	}
	if err != nil {
		return nil, err
	}

	if len(excludedChannelIDs) > 0 {
		abilities = filterExcludedAbilitiesForRetry(abilities, retry, excludedChannelIDs)
	}

	channel := Channel{}
	if len(abilities) > 0 {
		// Randomly choose one
		weightSum := uint(0)
		for _, ability_ := range abilities {
			weightSum += ability_.Weight + 10
		}
		// Randomly choose one
		weight := common.GetRandomInt(int(weightSum))
		for _, ability_ := range abilities {
			weight -= int(ability_.Weight) + 10
			//log.Printf("weight: %d, ability weight: %d", weight, *ability_.Weight)
			if weight <= 0 {
				channel.Id = ability_.ChannelId
				break
			}
		}
	} else {
		return nil, nil
	}
	err = DB.First(&channel, "id = ?", channel.Id).Error
	return &channel, err
}

func filterExcludedAbilitiesForRetry(abilities []Ability, retry int, excludedChannelIDs map[int]bool) []Ability {
	if len(abilities) == 0 {
		return abilities
	}
	filtered := make([]Ability, 0, len(abilities))
	for _, ability := range abilities {
		if excludedChannelIDs[ability.ChannelId] {
			continue
		}
		filtered = append(filtered, ability)
	}
	if len(filtered) == 0 {
		return filtered
	}

	uniquePriorities := make(map[int]bool)
	for _, ability := range filtered {
		priority := int64(0)
		if ability.Priority != nil {
			priority = *ability.Priority
		}
		uniquePriorities[int(priority)] = true
	}
	priorities := make([]int, 0, len(uniquePriorities))
	for priority := range uniquePriorities {
		priorities = append(priorities, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(priorities)))
	if retry >= len(priorities) {
		retry = len(priorities) - 1
	}
	targetPriority := int64(priorities[retry])
	target := make([]Ability, 0, len(filtered))
	for _, ability := range filtered {
		priority := int64(0)
		if ability.Priority != nil {
			priority = *ability.Priority
		}
		if priority == targetPriority {
			target = append(target, ability)
		}
	}
	return target
}

func (channel *Channel) AddAbilities(tx *gorm.DB) error {
	models_ := strings.Split(channel.Models, ",")
	groups_ := strings.Split(channel.Group, ",")
	abilitySet := make(map[string]struct{})
	abilities := make([]Ability, 0, len(models_))
	for _, model := range models_ {
		for _, group := range groups_ {
			key := group + "|" + model
			if _, exists := abilitySet[key]; exists {
				continue
			}
			abilitySet[key] = struct{}{}
			ability := Ability{
				Group:     group,
				Model:     model,
				ChannelId: channel.Id,
				Enabled:   channel.Status == common.ChannelStatusEnabled,
				Priority:  channel.Priority,
				Weight:    uint(channel.GetWeight()),
				Tag:       channel.Tag,
			}
			abilities = append(abilities, ability)
		}
	}
	if len(abilities) == 0 {
		return nil
	}
	// choose DB or provided tx
	useDB := DB
	if tx != nil {
		useDB = tx
	}
	for _, chunk := range lo.Chunk(abilities, 50) {
		err := useDB.Clauses(clause.OnConflict{DoNothing: true}).Create(&chunk).Error
		if err != nil {
			return err
		}
	}
	return nil
}

func (channel *Channel) DeleteAbilities() error {
	return DB.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
}

// UpdateAbilities updates abilities of this channel.
// Make sure the channel is completed before calling this function.
func (channel *Channel) UpdateAbilities(tx *gorm.DB) error {
	isNewTx := false
	// 如果没有传入事务，创建新的事务
	if tx == nil {
		tx = DB.Begin()
		if tx.Error != nil {
			return tx.Error
		}
		isNewTx = true
		defer func() {
			if r := recover(); r != nil {
				tx.Rollback()
			}
		}()
	}

	// First delete all abilities of this channel
	err := tx.Where("channel_id = ?", channel.Id).Delete(&Ability{}).Error
	if err != nil {
		if isNewTx {
			tx.Rollback()
		}
		return err
	}

	// Then add new abilities
	models_ := strings.Split(channel.Models, ",")
	groups_ := strings.Split(channel.Group, ",")
	abilitySet := make(map[string]struct{})
	abilities := make([]Ability, 0, len(models_))
	for _, model := range models_ {
		for _, group := range groups_ {
			key := group + "|" + model
			if _, exists := abilitySet[key]; exists {
				continue
			}
			abilitySet[key] = struct{}{}
			ability := Ability{
				Group:     group,
				Model:     model,
				ChannelId: channel.Id,
				Enabled:   channel.Status == common.ChannelStatusEnabled,
				Priority:  channel.Priority,
				Weight:    uint(channel.GetWeight()),
				Tag:       channel.Tag,
			}
			abilities = append(abilities, ability)
		}
	}

	if len(abilities) > 0 {
		for _, chunk := range lo.Chunk(abilities, 50) {
			err = tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&chunk).Error
			if err != nil {
				if isNewTx {
					tx.Rollback()
				}
				return err
			}
		}
	}

	// 如果是新创建的事务，需要提交
	if isNewTx {
		return tx.Commit().Error
	}

	return nil
}

func UpdateAbilityStatus(channelId int, status bool) error {
	return DB.Model(&Ability{}).Where("channel_id = ?", channelId).Select("enabled").Update("enabled", status).Error
}

func UpdateAbilityStatusByTag(tag string, status bool) error {
	return DB.Model(&Ability{}).Where("tag = ?", tag).Select("enabled").Update("enabled", status).Error
}

func UpdateAbilityByTag(tag string, newTag *string, priority *int64, weight *uint) error {
	ability := Ability{}
	if newTag != nil {
		ability.Tag = newTag
	}
	if priority != nil {
		ability.Priority = priority
	}
	if weight != nil {
		ability.Weight = *weight
	}
	return DB.Model(&Ability{}).Where("tag = ?", tag).Updates(ability).Error
}

var fixLock = sync.Mutex{}

func FixAbility() (int, int, error) {
	lock := fixLock.TryLock()
	if !lock {
		return 0, 0, errors.New("已经有一个修复任务在运行中，请稍后再试")
	}
	defer fixLock.Unlock()

	// truncate abilities table
	if common.UsingSQLite {
		err := DB.Exec("DELETE FROM abilities").Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Delete abilities failed: %s", err.Error()))
			return 0, 0, err
		}
	} else {
		err := DB.Exec("TRUNCATE TABLE abilities").Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Truncate abilities failed: %s", err.Error()))
			return 0, 0, err
		}
	}
	var channels []*Channel
	// Find all channels
	err := DB.Model(&Channel{}).Find(&channels).Error
	if err != nil {
		return 0, 0, err
	}
	if len(channels) == 0 {
		return 0, 0, nil
	}
	successCount := 0
	failCount := 0
	for _, chunk := range lo.Chunk(channels, 50) {
		ids := lo.Map(chunk, func(c *Channel, _ int) int { return c.Id })
		// Delete all abilities of this channel
		err = DB.Where("channel_id IN ?", ids).Delete(&Ability{}).Error
		if err != nil {
			common.SysLog(fmt.Sprintf("Delete abilities failed: %s", err.Error()))
			failCount += len(chunk)
			continue
		}
		// Then add new abilities
		for _, channel := range chunk {
			err = channel.AddAbilities(nil)
			if err != nil {
				common.SysLog(fmt.Sprintf("Add abilities for channel %d failed: %s", channel.Id, err.Error()))
				failCount++
			} else {
				successCount++
			}
		}
	}
	InitChannelCache()
	return successCount, failCount, nil
}
