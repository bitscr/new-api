package service

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

func TestAutoModelChannelExclusions(t *testing.T) {
	targets := []model.AutoModelRoutingTarget{{ChannelID: 1, Priority: 100}, {ChannelID: 2, Priority: 0}}
	cooling := map[int]bool{1: true}
	hard := map[int]bool{3: true}
	excluded := autoModelChannelExclusions(hard, cooling, targets)
	require.True(t, excluded[1], "cooling high-priority route must yield to a healthy lower tier")
	require.False(t, excluded[2])
	require.True(t, excluded[3])
	require.NotContains(t, hard, 1, "must not mutate caller exclusions")

	allCooling := autoModelChannelExclusions(hard, map[int]bool{1: true, 2: true}, targets)
	require.Equal(t, map[int]bool{1: true, 2: true, 3: true}, allCooling, "all-cooling routes stay excluded")
	hard[2] = true
	excluded = autoModelChannelExclusions(hard, cooling, targets)
	require.True(t, excluded[2], "hard exclusions must never be relaxed")
	require.True(t, excluded[1], "no healthy alternative must not bypass cooldown")
	require.Equal(t, map[int]bool{1: true, 2: true, 3: true}, autoModelChannelExclusions(hard, cooling, nil),
		"cooldown filtering must not require a healthy target snapshot")
	require.Equal(t, map[int]bool{1: true}, autoModelChannelExclusions(nil, cooling, nil))
	require.Empty(t, autoModelChannelExclusions(nil, nil, nil))
}

func TestAutoModelCoolingChannelIsSkippedInBothSelectors(t *testing.T) {
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
		selected, err := selectChannelWithUsedFallback(&RetryParam{Ctx: c}, "g", "m", 0, nil)
		require.NoError(t, err)
		require.NotNil(t, selected)
		require.Equal(t, 32, selected.Id, "cooldown must override the failing route's higher priority")
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "")
		selected, err = selectChannelWithUsedFallback(&RetryParam{Ctx: c}, "g", "m", 0, nil)
		require.NoError(t, err)
		require.NotNil(t, selected)
		require.Equal(t, 31, selected.Id, "explicit non-auto model selection keeps original priority semantics")
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	}
}

func TestAutoModelChannelScoreUsesActualSelectionGroup(t *testing.T) {
	autoModelTestReset()
	t.Cleanup(autoModelTestReset)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "auto")
	now := time.Now()
	autoModelHealth.Set(autoModelHealthKey("default", "m", 1), autoModelOutcome{Score: 0.9, Observations: 10, UpdatedAt: now})
	autoModelHealth.Set(autoModelHealthKey("vip", "m", 1), autoModelOutcome{Score: 0.1, Observations: 10, UpdatedAt: now})
	require.Greater(t, autoModelChannelScoreFnForGroup(c, "default", "m")(1), 0.5)
	require.Less(t, autoModelChannelScoreFnForGroup(c, "vip", "m")(1), 0.5)
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "")
	require.Nil(t, autoModelChannelScoreFnForGroup(c, "default", "m"))
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
