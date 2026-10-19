package controller

import (
	"net/http"
	"strconv"
	"strings"

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