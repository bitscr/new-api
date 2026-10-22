package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

// GetAutoModelScores 返回模型得分明细(登录可见),供前台按得分排序展示。
// refresh_seconds 是页面自动刷新间隔,可在后台配置(单位秒)。
func GetAutoModelScores(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success":         true,
		"message":         "",
		"data":            service.ListAutoModelScores(timeNow()),
		"refresh_seconds": operation_setting.AutoModelScoreRefreshSeconds(),
	})
}

// ClearAutoModelNegativeEffects 一键清除某个 (分组, 模型, 渠道) 组合的负面效果:
// 得分恢复中立 0.5 并解除冷却。参数不全一律拒绝,避免误清整片观测。
func ClearAutoModelNegativeEffects(c *gin.Context) {
	group := strings.TrimSpace(c.Query("group"))
	modelName := strings.TrimSpace(c.Query("model"))
	channelID, err := strconv.Atoi(strings.TrimSpace(c.Query("channel_id")))
	if err != nil || channelID <= 0 {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "无效的 channel_id",
		})
		return
	}
	if group == "" || modelName == "" {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "group 与 model 都必须给出",
		})
		return
	}
	if !service.ClearAutoModelNegativeEffects(group, modelName, channelID) {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "该组合暂无观测数据,本就是中立得分",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

// autoModelScoreClearBatchLimit 限制单次批量清除的行数,避免一个请求长时间
// 持有评分/冷却锁或产生无界日志。
const autoModelScoreClearBatchLimit = 1000

type autoModelScoreClearItem struct {
	Group     string `json:"group"`
	Model     string `json:"model"`
	ChannelID int    `json:"channel_id"`
}

type autoModelScoreClearBatchRequest struct {
	Items []autoModelScoreClearItem `json:"items"`
}

// ClearAutoModelNegativeEffectsBatch 批量清除所选 (分组, 模型, 渠道) 的负面效果。
// 重复行只执行一次;没有观测数据的行计入 skipped,不回滚其它已清除行。
func ClearAutoModelNegativeEffectsBatch(c *gin.Context) {
	var request autoModelScoreClearBatchRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "请求格式无效"})
		return
	}
	if len(request.Items) == 0 {
		c.JSON(http.StatusOK, gin.H{"success": false, "message": "请选择要清除的得分行"})
		return
	}
	if len(request.Items) > autoModelScoreClearBatchLimit {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": "单次最多清除 " + strconv.Itoa(autoModelScoreClearBatchLimit) + " 行",
		})
		return
	}

	seen := make(map[autoModelScoreClearItem]bool, len(request.Items))
	items := make([]autoModelScoreClearItem, 0, len(request.Items))
	for _, item := range request.Items {
		item.Group = strings.TrimSpace(item.Group)
		item.Model = strings.TrimSpace(item.Model)
		if item.Group == "" || item.Model == "" || item.ChannelID <= 0 {
			c.JSON(http.StatusOK, gin.H{"success": false, "message": "存在无效的得分行"})
			return
		}
		if seen[item] {
			continue
		}
		seen[item] = true
		items = append(items, item)
	}

	cleared, skipped := 0, 0
	for _, item := range items {
		if service.ClearAutoModelNegativeEffects(item.Group, item.Model, item.ChannelID) {
			cleared++
		} else {
			skipped++
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"cleared_count": cleared,
			"skipped_count": skipped,
		},
	})
}
