package service

// 旧 auto 按分选渪旁路已删除:统一规划在计划阶段排除冷却路线
// (见 auto_model_route_test.go 的冷却用例)。这里保留具名选渪
// 跳过 auto 冷却渠道、以及 affinity 不绕过冷却的约束。

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAutoModelCoolingChannelIsSkippedInNamedSelection(t *testing.T) {
	db := setupMultiGroupChannelSelectionTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelCooldown{}))
	autoModelTestReset()
	LoadAutoModelCooldowns()
	t.Cleanup(autoModelTestReset)
	high, low := int64(100), int64(0)
	channels := []model.Channel{
		{Id: 31, Type: 1, Key: "high", Status: common.ChannelStatusEnabled, Name: "high", Group: "g", Models: "m", Priority: &high},
		{Id: 32, Type: 1, Key: "low", Status: common.ChannelStatusEnabled, Name: "low", Group: "g", Models: "m", Priority: &low},
	}
	require.NoError(t, db.Create(&channels).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "g", Model: "m", ChannelId: 31, Enabled: true, Priority: &high},
		{Group: "g", Model: "m", ChannelId: 32, Enabled: true, Priority: &low},
	}).Error)
	model.InitChannelCache()
	tripAutoModelCooldown("g", "m", 31, "failed", time.Now())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "g")
	for _, cacheEnabled := range []bool{true, false} {
		common.MemoryCacheEnabled = cacheEnabled
		// 具名(非 auto)选渪跳过 auto 冷却中的渠道,不改动原优先级语义。
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "")
		selected, err := selectChannelWithUsedFallback(&RetryParam{Ctx: c}, "g", "m", 0, nil)
		require.NoError(t, err)
		require.NotNil(t, selected)
		require.Equal(t, 31, selected.Id, "named selection keeps original priority semantics; auto routing is planned separately")
	}
}

func TestAutoModelAffinityDoesNotBypassCooldown(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	tripAutoModelCooldown("default", "m", 1, "failed", time.Now())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.False(t, ShouldAvoidAutoModelAffinity(c, "default", "m", 1), "non-auto affinity is unchanged")
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	require.True(t, ShouldAvoidAutoModelAffinity(c, "default", "m", 1))
	require.False(t, ShouldAvoidAutoModelAffinity(c, "default", "other", 1))
	require.False(t, ShouldAvoidAutoModelAffinity(c, "vip", "m", 1))
	require.False(t, ShouldAvoidAutoModelAffinity(c, "default", "m", 2))
}
