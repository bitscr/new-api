package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// autoModelRetireMaxAttempts 是单次永久摘除的有限 CAS 重试上限。
// 两个请求并发摘除同一渠道的两个模型时,后者会碰上前者刚提交的新 models 值,
// CAS 失效;多实例也写同一张表,进程内互斥锁解决不了它。重试让每个请求都在
// 最新模型列表上重新计算,绝不允许用一个旧快照把刚摘掉的模型写回来。
const autoModelRetireMaxAttempts = 5

var errAutoModelRetireConflict = errors.New("auto model retire: channel models changed concurrently")

// ChannelModelRetireResult 描述一次原子摘除的结果,供调用方决定日志与冷却清理。
type ChannelModelRetireResult struct {
	// Removed 为 true 表示本次真的把模型从 models 里删掉了。
	Removed bool
	// Emptied 为 true 表示删除后渠道已无模型,并在同一事务里被自动禁用。
	Emptied bool
}

// RetireChannelModel 原子地从一个渠道的 models 里移除 modelName 并重建 abilities。
//
// 同一时间发生在同一渠道上的并发摘除,最终两种模型都不会复活:每次尝试都
// 在事务里读取最新 models,以"models = 刚才读到的值"为 CAS 条件提交,冲突时
// 重新读取后有限重试。SQLite 的写竞争/忙错误同样按有限重试处理并明确返回失败。
//
// 不删除渠道、不覆盖凭据、priority、weight、模型映射等无关配置。摘空最后一个
// 模型时,同一事务里把渠道标记为自动禁用,并写入禁用原因;abilities 重建与
// models 更新始终共用同一个 tx,不存在"只提交一半"。
//
// 幂等:模型已经不在 models 里时不做无谓写库,但仍会核对并修复遗留的 ability
// (历史不一致),不允许继续路由一个已经不在列表里的模型。
func RetireChannelModel(ctx context.Context, channelID int, modelName string, reason string) (ChannelModelRetireResult, error) {
	modelName = strings.TrimSpace(modelName)
	if channelID <= 0 || modelName == "" {
		return ChannelModelRetireResult{}, nil
	}
	if DB == nil {
		return ChannelModelRetireResult{}, errors.New("database not initialized")
	}
	var lastErr error
	for attempt := 0; attempt < autoModelRetireMaxAttempts; attempt++ {
		result, err := retireChannelModelOnce(ctx, channelID, modelName, reason)
		if err == nil {
			return result, nil
		}
		lastErr = err
		if errors.Is(err, errAutoModelRetireConflict) || isAutoModelRetireBusy(err) {
			// 有限重试 + 小退避。冲突与忙错都说明别的写入正在进行,重新读最新
			// 状态再试;超过上限就带着明确错误返回,不假报成功。
			time.Sleep(time.Duration(attempt+1) * 5 * time.Millisecond)
			continue
		}
		return ChannelModelRetireResult{}, err
	}
	return ChannelModelRetireResult{}, fmt.Errorf("retire model %s from channel %d: %w", modelName, channelID, lastErr)
}

func retireChannelModelOnce(ctx context.Context, channelID int, modelName, reason string) (ChannelModelRetireResult, error) {
	tx := DB.WithContext(ctx).Begin()
	if tx.Error != nil {
		return ChannelModelRetireResult{}, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	var channel Channel
	if err := tx.First(&channel, "id = ?", channelID).Error; err != nil {
		tx.Rollback()
		return ChannelModelRetireResult{}, fmt.Errorf("load channel %d: %w", channelID, err)
	}
	originalModels := channel.Models
	remaining := make([]string, 0, len(channel.GetModels()))
	removed := false
	for _, m := range channel.GetModels() {
		if strings.TrimSpace(m) == modelName {
			removed = true
			continue
		}
		remaining = append(remaining, m)
	}

	if !removed {
		// 模型已不在 models 里:幂等返回,但先修复历史不一致——如果还挂着
		// 指向该模型的 ability,它就是"已摘除却仍可被路由"的遗留行。
		var stale int64
		if err := tx.Model(&Ability{}).Where("channel_id = ? AND model = ?", channelID, modelName).Count(&stale).Error; err != nil {
			tx.Rollback()
			return ChannelModelRetireResult{}, err
		}
		if stale == 0 {
			return ChannelModelRetireResult{}, tx.Commit().Error
		}
		if err := updateAbilitiesInTx(tx, &channel); err != nil {
			tx.Rollback()
			return ChannelModelRetireResult{}, err
		}
		if err := tx.Commit().Error; err != nil {
			return ChannelModelRetireResult{}, err
		}
		InvalidateAutoModelRoutingTargetsCache()
		return ChannelModelRetireResult{}, nil
	}

	newModels := strings.Join(remaining, ",")
	channel.Models = newModels
	emptied := len(remaining) == 0
	if emptied {
		disableReason := fmt.Sprintf("模型 %s 确定性失败已被移除,渠道已无可用模型", modelName)
		if strings.TrimSpace(reason) != "" {
			disableReason = fmt.Sprintf("%s(最后一次失败:%s)", disableReason, strings.TrimSpace(reason))
		}
		info := channel.GetOtherInfo()
		info["status_reason"] = disableReason
		info["status_time"] = common.GetTimestamp()
		otherInfoJSON, err := common.Marshal(info)
		if err != nil {
			tx.Rollback()
			return ChannelModelRetireResult{}, err
		}
		channel.OtherInfo = string(otherInfoJSON)
		channel.Status = common.ChannelStatusAutoDisabled
		res := tx.Model(&Channel{}).Where("id = ? AND models = ?", channelID, originalModels).
			Updates(map[string]interface{}{"models": newModels, "status": common.ChannelStatusAutoDisabled, "other_info": channel.OtherInfo})
		if res.Error != nil {
			tx.Rollback()
			return ChannelModelRetireResult{}, res.Error
		}
		if res.RowsAffected == 0 {
			tx.Rollback()
			return ChannelModelRetireResult{}, errAutoModelRetireConflict
		}
	} else {
		res := tx.Model(&Channel{}).Where("id = ? AND models = ?", channelID, originalModels).Update("models", newModels)
		if res.Error != nil {
			tx.Rollback()
			return ChannelModelRetireResult{}, res.Error
		}
		if res.RowsAffected == 0 {
			tx.Rollback()
			return ChannelModelRetireResult{}, errAutoModelRetireConflict
		}
	}

	// abilities 用同一个 tx 重建;自动禁用会让新 abilities 的 Enabled=false。
	if err := updateAbilitiesInTx(tx, &channel); err != nil {
		tx.Rollback()
		return ChannelModelRetireResult{}, err
	}
	if err := tx.Commit().Error; err != nil {
		return ChannelModelRetireResult{}, err
	}
	// 快照/缓存失效放在提交之后:不能在提交前失效又让并发读按旧数据回填。
	InvalidateAutoModelRoutingTargetsCache()
	if emptied {
		CacheUpdateChannelStatus(channelID, common.ChannelStatusAutoDisabled)
	}
	return ChannelModelRetireResult{Removed: true, Emptied: emptied}, nil
}

// isAutoModelRetireBusy 识别 SQLite 的写竞争/忙错误,使摘除路径能有限重试。
// 非 SQLite 分支不做字符串匹配,避免误判其它数据库的普通错误为重试目标。
func isAutoModelRetireBusy(err error) bool {
	if err == nil || !common.UsingSQLite {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") || strings.Contains(message, "busy")
}
