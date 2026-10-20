package service

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAutoModelRouteRequestTest(t *testing.T) *gorm.DB {
	t.Helper()
	oldDB, oldMemory, oldSQLite := model.DB, common.MemoryCacheEnabled, common.UsingSQLite
	oldEnabled, oldSelfUse := operation_setting.AutoModelEnabled, operation_setting.SelfUseModeEnabled
	oldCandidates, oldWeights := operation_setting.AutoModelCandidatesToJSONString(), operation_setting.AutoModelWeightsToJSONString()
	oldRatios := ratio_setting.ModelRatio2JSONString()
	oldHealth, oldCooldowns := autoModelHealth.ReadAll(), autoModelCooldowns.ReadAll()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.AutoModelCooldown{}))
	model.DB, common.MemoryCacheEnabled, common.UsingSQLite = db, false, true
	// 路由目标缓存是 model 包级全局,换库后必须丢弃,否则上一个用例的快照
	// 会串进本次断言。
	model.InvalidateAutoModelRoutingTargetsCache()
	operation_setting.AutoModelEnabled, operation_setting.SelfUseModeEnabled = true, false
	autoModelTestReset()
	autoModelCooldownLoadOnce = sync.Once{}
	LoadAutoModelCooldowns()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"route-a":1,"route-b":1,"route-denied":1,"route-outside":1,"route-disabled":1}`))
	t.Cleanup(func() {
		model.DB, common.MemoryCacheEnabled, common.UsingSQLite = oldDB, oldMemory, oldSQLite
		operation_setting.AutoModelEnabled, operation_setting.SelfUseModeEnabled = oldEnabled, oldSelfUse
		require.NoError(t, operation_setting.SetAutoModelCandidates(oldCandidates))
		require.NoError(t, operation_setting.SetAutoModelWeights(oldWeights))
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(oldRatios))
		autoModelHealth.Clear()
		autoModelHealth.AddAll(oldHealth)
		autoModelCooldowns.Clear()
		autoModelCooldowns.AddAll(oldCooldowns)
		autoModelCooldownLoadOnce = sync.Once{}
		require.NoError(t, sqlDB.Close())
	})
	return db
}

func seedAutoModelRouteRequestChannels(t *testing.T, db *gorm.DB) {
	t.Helper()
	priority := int64(0)
	channels := []model.Channel{
		{Id:1, Type:1, Key:"first", Status:common.ChannelStatusEnabled, Group:"g", Models:"route-a,route-b", Priority:&priority},
		{Id:2, Type:1, Key:"second", Status:common.ChannelStatusEnabled, Group:"g", Models:"route-a,route-b", Priority:&priority},
	}
	require.NoError(t, db.Create(&channels).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group:"g", Model:"route-a", ChannelId:1, Enabled:true},
		{Group:"g", Model:"route-b", ChannelId:1, Enabled:true},
		{Group:"g", Model:"route-a", ChannelId:2, Enabled:true},
		{Group:"g", Model:"route-b", ChannelId:2, Enabled:true},
	}).Error)
}

func TestBuildAutoModelRoutesFiltersEnabledAuthorizedBilledAllowlist(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	require.NoError(t, db.Create(&[]model.Channel{
		{Id:1, Key:"enabled", Status:common.ChannelStatusEnabled, Group:"g", Models:"route-a,route-b,route-denied,route-unbilled,route-outside,route-disabled"},
		{Id:2, Key:"disabled", Status:common.ChannelStatusManuallyDisabled, Group:"g", Models:"route-disabled"},
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group:"g", Model:"route-a", ChannelId:1, Enabled:true},
		{Group:"g", Model:"route-b", ChannelId:1, Enabled:true},
		{Group:"g", Model:"route-denied", ChannelId:1, Enabled:true},
		{Group:"g", Model:"route-unbilled", ChannelId:1, Enabled:true},
		{Group:"g", Model:"route-outside", ChannelId:1, Enabled:true},
		{Group:"g", Model:"route-disabled", ChannelId:1, Enabled:false},
		{Group:"g", Model:"route-disabled", ChannelId:2, Enabled:true},
	}).Error)
	require.NoError(t, operation_setting.SetAutoModelCandidates(`{"g":["route-a"," route-a ","route-b","route-denied","route-unbilled","route-disabled","missing","auto"]}`))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{
		"route-a":false, "route-b":true, "route-unbilled":true, "route-disabled":true,
	})
	routes, err := BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err)
	require.ElementsMatch(t, []AutoModelRoute{
		{Group:"g", ModelName:"route-a", ChannelID:1},
		{Group:"g", ModelName:"route-b", ChannelID:1},
	}, routes, "whitelist presence grants access; disabled, unbilled and unlisted routes are absent")
	require.NoError(t, operation_setting.SetAutoModelCandidates(`{"g":[]}`))
	routes, err = BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err)
	require.Empty(t, routes, "an explicitly empty group allowlist must not fall back")
}

func TestBuildAutoModelRoutesStrictCooldownListAndAuthorizationErrors(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	require.NoError(t, db.Create(&model.Channel{Id:1, Key:"one", Status:common.ChannelStatusEnabled, Group:"g", Models:"route-a,route-b"}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group:"g", Model:"route-a", ChannelId:1, Enabled:true},
		{Group:"g", Model:"route-b", ChannelId:1, Enabled:true},
	}).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"route-a":true})
	tripAutoModelCooldown("g", "route-a", 1, "test", time.Now())
	routes, err := BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err, "authorized but cooling is exhausted routing, not denied authorization")
	require.Empty(t, routes, "an unauthorized healthy route cannot revive the authorized cooling route")
	require.Empty(t, GetAutoModelCandidatesForRequest(c, "g"))
	require.Equal(t, []string{"route-b"}, GetAutoModelCandidates("g"))
	tripAutoModelCooldown("g", "route-b", 1, "test", time.Now())
	require.Empty(t, GetAutoModelCandidates("g"), "the list view also excludes every fully cooled model")
	common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{})
	routes, err = BuildAutoModelRoutesForRequest(c, "g")
	require.ErrorIs(t, err, ErrAutoModelNoAuthorizedCandidates)
	require.Nil(t, routes)
	require.NoError(t, operation_setting.SetAutoModelWeights(`{"g":{"route-a":0,"route-b":0}}`))
	routes, err = BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err, "all-zero model weights intentionally produce no routes, not a token error")
	require.Empty(t, routes)
}

func TestBuildAutoModelRoutesRespectsAutoGroupOrderAndRetrySwitch(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	low, high := int64(0), int64(999)
	require.NoError(t, db.Create(&[]model.Channel{
		{Id:1, Key:"g", Status:common.ChannelStatusEnabled, Group:"g", Models:"route-a", Priority:&low},
		{Id:2, Key:"vip", Status:common.ChannelStatusEnabled, Group:"vip", Models:"route-a", Priority:&high},
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group:"g", Model:"route-a", ChannelId:1, Enabled:true},
		{Group:"vip", Model:"route-a", ChannelId:2, Enabled:true},
	}).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "auto")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "auto")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "g")
	common.SetContextKey(c, constant.ContextKeyTokenGroups, []string{"empty","g","vip"})
	common.SetContextKey(c, constant.ContextKeyAutoGroup, "vip")
	common.SetContextKey(c, constant.ContextKeyAutoGroupIndex, 99)
	common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, false)
	routes, err := BuildAutoModelRoutesForRequest(c, "auto")
	require.NoError(t, err)
	require.Equal(t, []AutoModelRoute{{Group:"g", ModelName:"route-a", ChannelID:1}}, routes,
		"new planning starts from authorized group order, not a previous selected group")
	common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, true)
	routes, err = BuildAutoModelRoutesForRequest(c, "auto")
	require.NoError(t, err)
	require.Equal(t, []AutoModelRoute{
		{Group:"g", ModelName:"route-a", ChannelID:1},
		{Group:"vip", ModelName:"route-a", ChannelID:2},
	}, routes, "cross-group plans concatenate groups; a later group's channel priority cannot jump ahead")
	common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, false)
	MarkChannelDailySuccessLimitSkipped(c, 1)
	routes, err = BuildAutoModelRoutesForRequest(c, "auto")
	require.NoError(t, err)
	require.Equal(t, []AutoModelRoute{{Group:"vip", ModelName:"route-a", ChannelID:2}}, routes,
		"before selecting any route, skip an empty group even when cross-group retry is disabled")
	routes, err = BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err)
	require.Empty(t, routes, "an explicit group never falls through to other token groups")
	MarkChannelRPMLimitSkipped(c, 2)
	routes, err = BuildAutoModelRoutesForRequest(c, "auto")
	require.NoError(t, err)
	require.Empty(t, routes)
}

func TestBuildAutoModelRoutesReranksEachRequestWithoutStickyChannel(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	seedAutoModelRouteRequestChannels(t, db)
	now := time.Now()
	autoModelHealth.Set(autoModelHealthKey("g", "route-a", 1), autoModelRouteTestOutcome(0.9, now))
	autoModelHealth.Set(autoModelHealthKey("g", "route-b", 1), autoModelRouteTestOutcome(0.2, now))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	routes, err := BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err)
	require.Equal(t, AutoModelRoute{Group:"g", ModelName:"route-a", ChannelID:1}, routes[0])
	SetAutoModelRoutePlan(c, routes)
	c.Set("use_channel", []string{"1","1","1","1"})
	// route-a@1 掉到 0.1 后,两条中性 0.5 的路(route-a@2、route-b@2)组成更好的
	// 档跨渠道领先——排序不再受渠道健康分或"渠道整块"约束。同档内两条路都在
	// 渠道 2 上,具体先哪个由权重随机决定。
	autoModelHealth.Set(autoModelHealthKey("g", "route-a", 1), autoModelRouteTestOutcome(0.1, time.Now()))
	routes, err = BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err)
	require.Equal(t, 2, routes[0].ChannelID, "中性 0.5 档领先 0.1/0.2 档,跨渠道交错")
	require.Len(t, routes, 4)
	// route-a@2 的新模型证据把它提到全场第一;先前的计划与 used-channel 历史
	// 不能制造"粘在上一次用过的组合"的偏好。
	autoModelHealth.Set(autoModelHealthKey("g", "route-a", 2), autoModelRouteTestOutcome(1, time.Now()))
	routes, err = BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err)
	require.Equal(t, AutoModelRoute{Group:"g", ModelName:"route-a", ChannelID:2}, routes[0])
	MarkChannelRPMLimitSkipped(c, 2)
	routes, err = BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err)
	require.Equal(t, 1, routes[0].ChannelID, "dispatch history is not a channel-wide hard exclusion for remaining models")
}

func TestBuildAutoModelRoutesFailsClosedOnSnapshotFailureAndDisable(t *testing.T) {
	setupAutoModelRouteRequestTest(t)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	model.DB = nil
	routes, err := BuildAutoModelRoutesForRequest(c, "g")
	require.Error(t, err)
	require.Nil(t, routes)
	require.Empty(t, GetAutoModelCandidates("g"))
	operation_setting.AutoModelEnabled = false
	routes, err = BuildAutoModelRoutesForRequest(c, "g")
	require.NoError(t, err)
	require.Nil(t, routes)
	require.Empty(t, GetAutoModelCandidates("g"))
	routes, err = BuildAutoModelRoutesForRequest(nil, "g")
	require.NoError(t, err)
	require.Nil(t, routes)
}

func TestAutoModelChatCompatibilityFilter(t *testing.T) {
	for _, tc := range []struct {
		name string
		endpoints []constant.EndpointType
		want bool
	}{
		{name:"unknown", want:true},
		{name:"chat", endpoints:[]constant.EndpointType{constant.EndpointTypeOpenAI}, want:true},
		{name:"responses", endpoints:[]constant.EndpointType{constant.EndpointTypeOpenAIResponse}, want:true},
		{name:"mixed", endpoints:[]constant.EndpointType{constant.EndpointTypeEmbeddings,constant.EndpointTypeOpenAI}, want:true},
		{name:"embeddings", endpoints:[]constant.EndpointType{constant.EndpointTypeEmbeddings}},
		{name:"image and video", endpoints:[]constant.EndpointType{constant.EndpointTypeImageGeneration,constant.EndpointTypeOpenAIVideo}},
	} {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, autoModelSupportsChat(tc.endpoints)) })
	}
}
