package controller

import (
	"net/http"

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