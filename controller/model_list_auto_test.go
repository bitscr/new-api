package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// /v1/models 里 auto 必须置顶:客户端模型选择器默认取列表头部。
func TestListModelsPutsAutoFirst(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	operation_setting.AutoModelEnabled = true
	// 无计费配置的模型会被 HasModelBillingConfig 过滤,自用模式跳过该过滤
	operation_setting.SelfUseModeEnabled = true
	t.Cleanup(func() {
		operation_setting.AutoModelEnabled = true
		operation_setting.SelfUseModeEnabled = false
	})

	require.NoError(t, db.Create(&model.User{
		Id:       2001,
		Username: "auto-list-user",
		Password: "password",
		Group:    "default",
		Status:   common.UserStatusEnabled,
	}).Error)
	priority := int64(1)
	require.NoError(t, db.Create(&model.Channel{
		Id: 1, Type: 1, Key: "k", Status: common.ChannelStatusEnabled,
		Name: "auto-channel", Group: "default", Models: "test-auto-model",
		Priority: &priority,
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group: "default", Model: "test-auto-model", ChannelId: 1, Enabled: true,
	}).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	ctx.Set("id", 2001)

	ListModels(ctx, constant.ChannelTypeOpenAI)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload listModelsResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)
	require.NotEmpty(t, payload.Data)
	require.Equal(t, "auto", payload.Data[0].Id, "auto 必须是列表第一个")
	require.Equal(t, "system", payload.Data[0].OwnedBy)

	found := false
	for _, item := range payload.Data {
		if item.Id == "test-auto-model" {
			found = true
		}
	}
	require.True(t, found, "auto 的候选模型应仍在列表中")
}

func TestListModelsTokenLimitWithAuto(t *testing.T) {
	setupModelListControllerTestDB(t)
	operation_setting.AutoModelEnabled = true

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	common.SetContextKey(ctx, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(ctx, constant.ContextKeyTokenModelLimit, map[string]bool{
		"auto":   true,
		"gpt-4o": true,
	})

	ListModels(ctx, constant.ChannelTypeOpenAI)

	require.Equal(t, http.StatusOK, recorder.Code)
	var payload listModelsResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
	require.True(t, payload.Success)

	autoCount := 0
	for _, item := range payload.Data {
		if item.Id == "auto" {
			autoCount++
		}
	}
	require.Equal(t, 1, autoCount, "auto 只出现一次")
	require.NotEmpty(t, payload.Data)
	require.Equal(t, "auto", payload.Data[0].Id, "auto 置顶")
}
