package model

import (
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
	// Permanent 标记这是"确定性失败"(模型不存在/无权/不支持)的长冷却,
	// 与限流、超时、5xx 那类会自愈的抖动失败区分开。老行为 false。
	Permanent bool  `json:"permanent" gorm:"not null;default:false"`
	UpdatedAt int64 `json:"updated_at" gorm:"bigint"`
}

func (AutoModelCooldown) TableName() string {
	return "auto_model_cooldowns"
}

func GetAutoModelCooldowns() ([]AutoModelCooldown, error) {
	var rows []AutoModelCooldown
	err := DB.Find(&rows).Error
	return rows, err
}

// UpsertAutoModelCooldown 按 (分组, 模型, 渠道) 覆盖写入。
func UpsertAutoModelCooldown(row *AutoModelCooldown) error {
	return DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "group"}, {Name: "model"}, {Name: "channel_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"until", "level", "reason", "permanent", "updated_at"}),
	}).Create(row).Error
}

func DeleteAutoModelCooldown(group, name string, channelId int) error {
	return DB.Where(&AutoModelCooldown{Group: group, Model: name, ChannelId: channelId}).
		Delete(&AutoModelCooldown{}).Error
}
