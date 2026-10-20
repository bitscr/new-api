package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// mergeCoolingAutoModelChannelIDs:冷却中的渠道 ID 合并进排除集;分组解析与
// 选路一致(令牌分组,退回用户分组);其它模型/其它分组的冷却不串。
func TestMergeCoolingAutoModelChannelIDs(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = true })

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")

	excluded := map[int]bool{}
	mergeCoolingAutoModelChannelIDs(c, "default", "m1", excluded)
	require.Empty(t, excluded, "无冷却时排除集为空")
	tripAutoModelCooldown("default", "m1", 1, "测试", time.Now())
	tripAutoModelCooldown("default", "m1", 2, "测试", time.Now())
	excluded = map[int]bool{}
	mergeCoolingAutoModelChannelIDs(c, "default", "m1", excluded)
	require.True(t, excluded[1], "冷却中的渠道必须被排除")
	require.True(t, excluded[2], "冷却中的渠道必须被排除")

	other := map[int]bool{}
	mergeCoolingAutoModelChannelIDs(c, "default", "m2", other)
	require.Empty(t, other, "其它模型的冷却不应串到 m1")
	mergeCoolingAutoModelChannelIDs(c, "vip", "m1", other)
	require.Empty(t, other, "其它分组的冷却不应串到 default")
}

// 具名选路(固定模型)跳过冷却中的 (分组,模型,渠道):渠道 1 冷却后,
// 具名选路只会选中渠道 2;两个渠道都冷却后按"无可用渠道"处理。
func TestNamedSelectionSkipsCoolingChannels(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = true })

	require.NoError(t, db.Create(&[]model.Channel{
		{Id: 1, Key: "a", Status: common.ChannelStatusEnabled, Group: "g", Models: "m1"},
		{Id: 2, Key: "b", Status: common.ChannelStatusEnabled, Group: "g", Models: "m1"},
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "g", Model: "m1", ChannelId: 1, Enabled: true},
		{Group: "g", Model: "m1", ChannelId: 2, Enabled: true},
	}).Error)
	tripAutoModelCooldown("g", "m1", 1, "测试", time.Now())

	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyUserGroup, "g")
	param := &RetryParam{Ctx: c, TokenGroup: "g", ModelName: "m1"}
	for i := 0; i < 8; i++ {
		channel, _, err := CacheGetRandomSatisfiedChannel(param)
		require.NoError(t, err)
		require.NotNil(t, channel)
		require.Equal(t, 2, channel.Id, "冷却中的渠道 1 不得被具名选路选中")
	}
	tripAutoModelCooldown("g", "m1", 2, "测试", time.Now())
	channel, _, err := CacheGetRandomSatisfiedChannel(param)
	require.Nil(t, channel, "全部冷却时不得选中任何渠道")
	_ = err
}
