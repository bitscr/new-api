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

func TestAutoModelAllCoolingRoutesAreSkippedInBothSelectors(t *testing.T) {
	db := setupMultiGroupChannelSelectionTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelCooldown{}))
	autoModelTestReset()
	LoadAutoModelCooldowns()
	t.Cleanup(autoModelTestReset)
	high, low := int64(100), int64(0)
	require.NoError(t, db.Create(&[]model.Channel{
		{Id: 31, Key: "high", Status: common.ChannelStatusEnabled, Group: "g", Models: "m,other-failed,other-healthy", Priority: &high},
		{Id: 32, Key: "low", Status: common.ChannelStatusEnabled, Group: "g", Models: "m", Priority: &low},
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "g", Model: "m", ChannelId: 31, Enabled: true, Priority: &high},
		{Group: "g", Model: "m", ChannelId: 32, Enabled: true, Priority: &low},
		{Group: "g", Model: "other-failed", ChannelId: 31, Enabled: true, Priority: &high},
		{Group: "g", Model: "other-healthy", ChannelId: 31, Enabled: true, Priority: &high},
	}).Error)
	model.InitChannelCache()
	tripAutoModelCooldown("g", "m", 31, "failed", time.Now())
	tripAutoModelCooldown("g", "m", 32, "failed", time.Now())
	tripAutoModelCooldown("g", "other-failed", 31, "failed", time.Now())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "g")
	c.Set("use_channel", []string{"31", "31", "32", "32"})

	for _, memory := range []bool{true, false} {
		common.MemoryCacheEnabled = memory
		for _, retry := range []int{0, 1, 20} {
			selected, err := selectChannelWithUsedFallback(&RetryParam{Ctx: c}, "g", "m", retry, GetAutoModelHardExcludedChannelIDs(c))
			require.NoError(t, err)
			require.Nil(t, selected, "all-cooling routes cannot be revived by priority or used-channel fallback")
		}
		selected, err := selectChannelWithUsedFallback(&RetryParam{Ctx: c}, "g", "other-healthy", 0, GetAutoModelHardExcludedChannelIDs(c))
		require.NoError(t, err)
		require.NotNil(t, selected, "two other failed models must not hard-exclude this healthy model's channel")
		require.Equal(t, 31, selected.Id)
	}

	// Expiry, not an all-cooling fallback, makes a route eligible again.
	state, ok := autoModelCooldowns.Get(autoModelHealthKey("g", "m", 32))
	require.True(t, ok)
	state.Until = time.Now().Add(-time.Second)
	autoModelCooldowns.Set(autoModelHealthKey("g", "m", 32), state)
	for _, memory := range []bool{true, false} {
		common.MemoryCacheEnabled = memory
		selected, err := selectChannelWithUsedFallback(&RetryParam{Ctx: c}, "g", "m", 0, GetAutoModelHardExcludedChannelIDs(c))
		require.NoError(t, err)
		require.NotNil(t, selected)
		require.Equal(t, 32, selected.Id)
	}
}

func TestExcludeCoolingAutoModelCandidatesRequiresHealthyEnabledRoute(t *testing.T) {
	db := setupMultiGroupChannelSelectionTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelCooldown{}))
	autoModelTestReset()
	LoadAutoModelCooldowns()
	t.Cleanup(autoModelTestReset)
	require.NoError(t, db.Create(&[]model.Channel{
		{Id: 51, Key: "enabled", Status: common.ChannelStatusEnabled, Group: "g", Models: "m,other"},
		{Id: 52, Key: "disabled-channel", Status: common.ChannelStatusManuallyDisabled, Group: "g", Models: "m"},
		{Id: 53, Key: "disabled-ability", Status: common.ChannelStatusEnabled, Group: "g", Models: "m"},
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "g", Model: "m", ChannelId: 51, Enabled: true},
		{Group: "g", Model: "other", ChannelId: 51, Enabled: true},
		{Group: "g", Model: "m", ChannelId: 52, Enabled: true},
		{Group: "g", Model: "m", ChannelId: 53, Enabled: false},
		{Group: "g", Model: "m", ChannelId: 54, Enabled: true},
	}).Error)
	model.InitChannelCache()
	tripAutoModelCooldown("g", "m", 51, "failed", time.Now())
	for _, memory := range []bool{true, false} {
		common.MemoryCacheEnabled = memory
		require.Empty(t, excludeCoolingAutoModelCandidates("g", []string{"m"}),
			"a sole cooling candidate cannot be revived by disabled or orphaned routes")
		require.Equal(t, []string{"other"}, excludeCoolingAutoModelCandidates("g", []string{"m", "other", "missing"}))
		require.Empty(t, excludeCoolingAutoModelCandidates("g", nil))
		require.Empty(t, excludeCoolingAutoModelCandidates("g", []string{"missing"}))
	}

	clearAutoModelCooldown("g", "m", 51, time.Now())
	for _, memory := range []bool{true, false} {
		common.MemoryCacheEnabled = memory
		require.Equal(t, []string{"m"}, excludeCoolingAutoModelCandidates("g", []string{"m"}))
	}
}

func TestExcludeCoolingAutoModelCandidatesDoesNotBypassUnavailableSnapshot(t *testing.T) {
	oldDB := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = oldDB })
	require.Empty(t, excludeCoolingAutoModelCandidates("g", []string{"m"}))
}
