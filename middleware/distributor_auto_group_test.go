package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const distributorAutoGroupTestModel = "pg-routeplan-group-test-model"

func setupDistributeAutoGroupTest(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	oldDB := model.DB
	oldMemory, oldRedis := common.MemoryCacheEnabled, common.RedisEnabled
	oldSQLite, oldMySQL, oldPostgreSQL := common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL
	oldAutoEnabled := operation_setting.AutoModelEnabled
	oldCandidates := operation_setting.AutoModelCandidatesToJSONString()
	oldWeights := operation_setting.AutoModelWeightsToJSONString()
	oldModelRatios := ratio_setting.ModelRatio2JSONString()
	oldAutoGroups := setting.AutoGroups2JsonString()
	oldUsableGroups := setting.UserUsableGroups2JSONString()
	specialGroups := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
	oldSpecialGroups := specialGroups.ReadAll()
	t.Cleanup(func() {
		model.DB = oldDB
		common.MemoryCacheEnabled, common.RedisEnabled = oldMemory, oldRedis
		common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = oldSQLite, oldMySQL, oldPostgreSQL
		operation_setting.AutoModelEnabled = oldAutoEnabled
		require.NoError(t, operation_setting.SetAutoModelCandidates(oldCandidates))
		require.NoError(t, operation_setting.SetAutoModelWeights(oldWeights))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(oldModelRatios))
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(oldAutoGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(oldUsableGroups))
		specialGroups.Clear()
		specialGroups.AddAll(oldSpecialGroups)
		require.NoError(t, sqlDB.Close())
	})

	model.DB = db
	common.MemoryCacheEnabled, common.RedisEnabled = false, false
	common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = true, false, false
	operation_setting.AutoModelEnabled = true
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.AutoModelCooldown{}))
	require.NoError(t, operation_setting.SetAutoModelCandidates("{}"))
	require.NoError(t, operation_setting.SetAutoModelWeights("{}"))
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"pg-routeplan-group-test-model":1}`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"pg-route-default":"Default","auto":"Auto"}`))
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["pg-route-default"]`))
	specialGroups.Clear()
	specialGroups.AddAll(map[string]map[string]string{
		"pg-route-default": {"-:auto": "this group's users cannot select auto"},
	})
	for i, group := range []string{"pg-route-default", "pg-route-vip", "pg-route-restricted"} {
		channelID := 8101 + i
		require.NoError(t, db.Create(&model.Channel{
			Id: channelID, Type: constant.ChannelTypeOpenAI, Key: "route-test-key",
			Status: common.ChannelStatusEnabled, Group: group, Models: distributorAutoGroupTestModel,
		}).Error)
		require.NoError(t, db.Create(&model.Ability{
			Group: group, Model: distributorAutoGroupTestModel, ChannelId: channelID, Enabled: true,
		}).Error)
	}
}

func TestDistributeAutoPlaygroundGroups(t *testing.T) {
	setupDistributeAutoGroupTest(t)
	// The original caller may select auto, but the concrete group's own users
	// may not. Rechecking after ActivateAutoModelRoute would wrongly return 403.
	require.True(t, service.GroupInUserUsableGroups("pg-route-vip", "auto"))
	require.False(t, service.GroupInUserUsableGroups("pg-route-default", "auto"))
	require.False(t, service.GroupInUserUsableGroups("pg-route-vip", "pg-route-restricted"))

	for _, tc := range []struct {
		name       string
		body       string
		status     int
		group      string
		tokenGroup string
		channelID  int
	}{
		{
			name: "auto_override_keeps_original_authorization_basis",
			body: `{"model":"auto","group":"auto","messages":[{"role":"user","content":"hello"}]}`,
			status: http.StatusNoContent, group: "pg-route-default", tokenGroup: "auto", channelID: 8101,
		},
		{
			name: "authorized_concrete_override",
			body: `{"model":"auto","group":"pg-route-default","messages":[{"role":"user","content":"hello"}]}`,
			status: http.StatusNoContent, group: "pg-route-default", tokenGroup: "pg-route-default", channelID: 8101,
		},
		{
			name: "forbidden_override_rejected_despite_enabled_route",
			body: `{"model":"auto","group":"pg-route-restricted","messages":[{"role":"user","content":"hello"}]}`,
			status: http.StatusForbidden,
		},
		{
			name: "empty_group_inherits_caller_group",
			body: `{"model":"auto","group":"","messages":[{"role":"user","content":"hello"}]}`,
			status: http.StatusNoContent, group: "pg-route-vip", channelID: 8102,
		},
		{
			name: "omitted_group_inherits_caller_group",
			body: `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`,
			status: http.StatusNoContent, group: "pg-route-vip", channelID: 8102,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := gin.New()
			var observed *gin.Context
			downstreamReached := false
			engine.Use(func(c *gin.Context) {
				defer common.CleanupBodyStorage(c)
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "pg-route-vip")
				common.SetContextKey(c, constant.ContextKeyUserGroup, "pg-route-vip")
				c.Next()
				observed = c.Copy()
			})
			engine.POST("/pg/chat/completions", Distribute(), func(c *gin.Context) {
				downstreamReached = true
				c.Status(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, "/pg/chat/completions", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, request)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.NotNil(t, observed)
			if tc.status != http.StatusNoContent {
				require.False(t, downstreamReached)
				require.Empty(t, service.GetAutoModelRoutePlan(observed), "reject the group before constructing or activating any route")
				require.Zero(t, common.GetContextKeyInt(observed, constant.ContextKeyChannelId))
				return
			}
			require.True(t, downstreamReached)
			route, ok := service.GetCurrentAutoModelRoute(observed)
			require.True(t, ok)
			require.Equal(t, service.AutoModelRoute{Group: tc.group, ModelName: distributorAutoGroupTestModel, ChannelID: tc.channelID}, route)
			require.Equal(t, tc.group, common.GetContextKeyString(observed, constant.ContextKeyUsingGroup))
			require.Equal(t, tc.tokenGroup, common.GetContextKeyString(observed, constant.ContextKeyTokenGroup))
			require.Equal(t, "pg-route-vip", common.GetContextKeyString(observed, constant.ContextKeyUserGroup))
			require.Equal(t, tc.channelID, common.GetContextKeyInt(observed, constant.ContextKeyChannelId))
			require.Equal(t, distributorAutoGroupTestModel, common.GetContextKeyString(observed, constant.ContextKeyOriginalModel))
			require.Equal(t, "auto", common.GetContextKeyString(observed, constant.ContextKeyAutoModelClientName))
		})
	}
}
