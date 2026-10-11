package controller

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// timeNow 便于测试注入;生产路径就是 time.Now。
var timeNow = time.Now

// GetAutoModelCooldowns 列出 auto 当前仍在冷却的组合。
//
// 冷却阶梯是自我强化的(24h 起、翻倍、封顶 30 天),而唯一的自动清除条件是
// "一次 20 秒内的快速成功"——冷却中的组合不会被路由,所以它只能等窗口自然到期。
// 这个只读接口让管理员能看到 auto 挡住了谁、什么原因、还要多久。
func GetAutoModelCooldowns(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    service.ListAutoModelCooldowns(timeNow()),
	})
}

// DeleteAutoModelCooldown 手工解除一条冷却(管理员操作)。
// 只删指定的 (分组, 模型, 渠道);参数不全一律拒绝,避免误删整片冷却。
func DeleteAutoModelCooldown(c *gin.Context) {
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
	if err := service.DeleteAutoModelCooldown(group, modelName, channelID); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}