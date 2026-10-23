package model

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AutoModelCooldown 是 auto 路由的候选冷却记录：某个"分组 + 模型 + 渠道"在
// Until 之前不要再被选中。写进数据库，重启后依然生效。
//
// 为什么需要落库：内存里的健康分只做短期反应（约 10 分钟衰减归零），
// 重启或空闲一会儿就全丢，auto 又会去挑排在最前面、但上游正在排队几十秒的候选。
// 冷却窗口有明确到期时间，重复犯错时逐级翻倍，正常速度的成功立刻清除。
type AutoModelCooldown struct {
	Id        int    `json:"id" gorm:"primaryKey"`
	Group     string `json:"group" gorm:"type:varchar(64);index;uniqueIndex:idx_auto_model_cooldown,priority:1"`
	Model     string `json:"model" gorm:"type:varchar(255);index;uniqueIndex:idx_auto_model_cooldown,priority:2"`
	ChannelId int    `json:"channel_id" gorm:"index;uniqueIndex:idx_auto_model_cooldown,priority:3"`
	Until     int64  `json:"until" gorm:"bigint;index"`
	Level     int    `json:"level"`
	Reason    string `json:"reason" gorm:"type:varchar(255)"`
	// Permanent 是历史遗留列:旧版本用它区分"确定性失败"的长冷却,现在确定性
	// 失败不再产生冷却(命中关键词的组合会被直接摘除),新写入恒为 false,
	// 存量 Permanent=true 的行在载入时被清掉。列保留是为了不动三库迁移。
	Permanent bool  `json:"permanent" gorm:"not null;default:false"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint"`
}

func (AutoModelCooldown) TableName() string {
	return "auto_model_cooldowns"
}

func GetAutoModelCooldowns() ([]AutoModelCooldown, error) {
	return GetAutoModelCooldownsContext(context.Background(), DB)
}

// GetAutoModelCooldownsContext bounds both the query and the caller's wait on
// the database handle. The startup/read paths use the same budget as writes.
func GetAutoModelCooldownsContext(ctx context.Context, db *gorm.DB) ([]AutoModelCooldown, error) {
	if db == nil {
		return nil, errors.New("auto model cooldown database is not initialized")
	}
	var rows []AutoModelCooldown
	err := db.WithContext(ctx).Find(&rows).Error
	return rows, err
}

// UpsertAutoModelCooldown 按 (分组, 模型, 渠道) 覆盖写入。
func UpsertAutoModelCooldown(row *AutoModelCooldown) error {
	return UpsertAutoModelCooldownContext(context.Background(), DB, row)
}

// UpsertAutoModelCooldownContext uses a stable DB handle and the caller's
// deadline so a blocked database cannot outlive the persistence worker budget.
func UpsertAutoModelCooldownContext(ctx context.Context, db *gorm.DB, row *AutoModelCooldown) error {
	if db == nil {
		return errors.New("auto model cooldown database is not initialized")
	}
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "group"}, {Name: "model"}, {Name: "channel_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"until", "level", "reason", "permanent", "updated_at"}),
	}).Create(row).Error
}

func DeleteAutoModelCooldown(group, name string, channelId int) error {
	return DeleteAutoModelCooldownContext(context.Background(), DB, group, name, channelId)
}

// DeleteAutoModelCooldownContext is the bounded variant used by background
// retries and administrator clears.
func DeleteAutoModelCooldownContext(ctx context.Context, db *gorm.DB, group, name string, channelId int) error {
	if db == nil {
		return errors.New("auto model cooldown database is not initialized")
	}
	return db.WithContext(ctx).Where(&AutoModelCooldown{Group: group, Model: name, ChannelId: channelId}).
		Delete(&AutoModelCooldown{}).Error
}
