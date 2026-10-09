package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func openAutoModelRouteControllerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	oldDB, oldLogDB := model.DB, model.LOG_DB
	oldSQLite, oldMySQL, oldPostgreSQL := common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL
	oldMemory, oldRedis, oldAuto := common.MemoryCacheEnabled, common.RedisEnabled, operation_setting.AutoModelEnabled
	gin.SetMode(gin.TestMode)
	dsn := fmt.Sprintf("file:auto_route_%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.AutoModelCooldown{}, &model.User{}))
	model.DB, model.LOG_DB = db, db
	common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = true, false, false
	common.MemoryCacheEnabled, common.RedisEnabled, operation_setting.AutoModelEnabled = false, false, true
	t.Cleanup(func() {
		model.DB, model.LOG_DB = oldDB, oldLogDB
		common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = oldSQLite, oldMySQL, oldPostgreSQL
		common.MemoryCacheEnabled, common.RedisEnabled, operation_setting.AutoModelEnabled = oldMemory, oldRedis, oldAuto
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func createAutoModelRouteTestChannel(t *testing.T, db *gorm.DB, group string, names ...string) *model.Channel {
	t.Helper()
	channel := &model.Channel{
		Type: constant.ChannelTypeOpenAI, Key: "route-key", Status: common.ChannelStatusEnabled,
		Name: "route-channel", Group: group, Models: strings.Join(names, ","),
	}
	require.NoError(t, db.Create(channel).Error)
	for _, name := range names {
		require.NoError(t, db.Create(&model.Ability{
			Group: group, Model: name, ChannelId: channel.Id, Enabled: true,
		}).Error)
	}
	return channel
}

func newAutoModelRouteTestContext(t *testing.T, group string, routes []service.AutoModelRoute) (*gin.Context, *relaycommon.RelayInfo, *service.RetryParam) {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"auto","messages":[{"role":"user","content":"hello"}],"temperature":0,"stream":false,"unknown":{"integer":9007199254740993,"zero":0}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	common.SetContextKey(c, constant.ContextKeyTokenGroup, group)
	common.SetContextKey(c, constant.ContextKeyUserGroup, group)
	service.SetAutoModelRoutePlan(c, routes)
	info := &relaycommon.RelayInfo{
		TokenGroup: group, UserGroup: group, UsingGroup: group,
		RelayFormat: types.RelayFormatOpenAI,
		Request:     &dto.GeneralOpenAIRequest{Model: "auto"},
		UserSetting: dto.UserSetting{AcceptUnsetRatioModel: true},
	}
	if len(routes) > 0 {
		info.OriginModelName = routes[0].ModelName
		info.ClientModelName = routes[0].ModelName
		c.Set("original_model", routes[0].ModelName)
	}
	retry := &service.RetryParam{Ctx: c, TokenGroup: group, ModelName: info.OriginModelName, Retry: common.GetPointer(0)}
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	return c, info, retry
}

func bindInitialAutoModelRouteForTest(t *testing.T, c *gin.Context, info *relaycommon.RelayInfo) *model.Channel {
	t.Helper()
	channel, err := prepareAutoModelRouteFromIndex(c, info, nil, 0, nil, service.GetAutoModelRouteIndex(c), false)
	require.Nil(t, err)
	require.NotNil(t, channel)
	return channel
}

func TestGetChannelAutoPinsFirstRouteInsteadOfContextOrRandomChannel(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	first := createAutoModelRouteTestChannel(t, db, "default", "a")
	other := createAutoModelRouteTestChannel(t, db, "default", "a")
	c, info, retry := newAutoModelRouteTestContext(t, "default", []service.AutoModelRoute{
		{Group: "default", ModelName: "a", ChannelID: first.Id},
		{Group: "default", ModelName: "a", ChannelID: other.Id},
	})
	c.Set("channel_id", other.Id)
	channel, err := getChannel(c, info, retry)
	require.Nil(t, err)
	require.Equal(t, first.Id, channel.Id, "ChannelMeta==nil must not reopen the context/random path")

	service.SetAutoModelRoutePlan(c, nil)
	channel, err = getChannel(c, info, retry)
	require.Nil(t, channel)
	require.NotNil(t, err, "a missing plan must fail closed rather than reuse the context channel")
}

func TestAutoModelRoutesExhaustChannelModelsThenSwitchSameModelAcrossChannels(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	first := createAutoModelRouteTestChannel(t, db, "default", "a", "b", "c")
	other := createAutoModelRouteTestChannel(t, db, "default", "c")
	routes := []service.AutoModelRoute{
		{Group: "default", ModelName: "a", ChannelID: first.Id},
		{Group: "default", ModelName: "b", ChannelID: first.Id},
		{Group: "default", ModelName: "a", ChannelID: first.Id}, // duplicate snapshot entry must not retry a failed pair
		{Group: "default", ModelName: "c", ChannelID: first.Id},
		{Group: "default", ModelName: "c", ChannelID: other.Id},
	}
	c, info, retry := newAutoModelRouteTestContext(t, "default", routes)
	bindInitialAutoModelRouteForTest(t, c, info)
	for _, nextIndex := range []int{1, 3, 4} {
		markCurrentAutoModelRouteAttempted(c)
		addUsedChannel(c, info.ChannelId)
		require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
		require.Equal(t, nextIndex, service.GetAutoModelRouteIndex(c))
		require.Equal(t, routes[nextIndex].ChannelID, info.ChannelId)
		require.Equal(t, routes[nextIndex].ModelName, info.OriginModelName)
		require.Equal(t, routes[nextIndex].ModelName, info.ClientModelName)
		require.Equal(t, routes[nextIndex].ModelName, info.Request.(*dto.GeneralOpenAIRequest).Model)
		selected, err := getChannel(c, info, retry)
		require.Nil(t, err)
		require.Equal(t, routes[nextIndex].ChannelID, selected.Id)
	}
	require.Equal(t, []string{fmt.Sprint(first.Id), fmt.Sprint(first.Id), fmt.Sprint(first.Id)}, c.GetStringSlice("use_channel"),
		"channel history remains intact; the third model must not hit the legacy channel max2")
	require.False(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
}

func TestAutoModelRouteLimitsHardExcludeAllModelsOnChannel(t *testing.T) {
	for _, limit := range []string{"daily", "rpm"} {
		t.Run(limit, func(t *testing.T) {
			db := openAutoModelRouteControllerTestDB(t)
			first := createAutoModelRouteTestChannel(t, db, "default", "a", "b")
			other := createAutoModelRouteTestChannel(t, db, "default", "a")
			c, info, retry := newAutoModelRouteTestContext(t, "default", []service.AutoModelRoute{
				{Group: "default", ModelName: "a", ChannelID: first.Id},
				{Group: "default", ModelName: "b", ChannelID: first.Id},
				{Group: "default", ModelName: "a", ChannelID: other.Id},
			})
			bindInitialAutoModelRouteForTest(t, c, info)
			if limit == "daily" {
				require.True(t, shouldSkipDailyLimitedChannel(c, first))
			} else {
				require.True(t, shouldSkipRPMLimitedChannel(c, first))
			}
			selected, err := getChannel(c, info, retry)
			require.Nil(t, selected)
			require.NotNil(t, err)
			require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
			require.Equal(t, 2, service.GetAutoModelRouteIndex(c))
			require.Equal(t, other.Id, info.ChannelId)
			require.True(t, service.GetAutoModelHardExcludedChannelIDs(c)[first.Id])
		})
	}
}

func TestAutoModelRouteRevalidatesAbilityChannelAndCooldown(t *testing.T) {
	for _, memoryCache := range []bool{false, true} {
		t.Run(fmt.Sprint(memoryCache), func(t *testing.T) {
			db := openAutoModelRouteControllerTestDB(t)
			common.MemoryCacheEnabled = memoryCache
			group := "revalidate-" + t.Name()
			channel := createAutoModelRouteTestChannel(t, db, group, "a")
			c, info, retry := newAutoModelRouteTestContext(t, group, []service.AutoModelRoute{
				{Group: group, ModelName: "a", ChannelID: channel.Id},
			})
			bindInitialAutoModelRouteForTest(t, c, info)
			require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", false).Error)
			selected, err := getChannel(c, info, retry)
			require.Nil(t, selected)
			require.NotNil(t, err, "an enabled channel with a disabled ability must be rejected even in cache mode")
			require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", true).Error)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", 2).Error)
			selected, err = getChannel(c, info, retry)
			require.Nil(t, selected)
			require.NotNil(t, err)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusEnabled).Error)
			service.RecordAutoModelUnusableAnswer(group, "a", channel.Id, "empty")
			require.True(t, service.GetAutoModelCoolingChannelIDs(group, "a")[channel.Id])
			selected, err = getChannel(c, info, retry)
			require.Nil(t, selected)
			require.NotNil(t, err, "even the only available route must stay excluded while cooling")
		})
	}
}

func TestAutoModelRouteSkipsCoolingPairWithoutSkippingOtherChannelModels(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	group := "cooling-pair-" + t.Name()
	channel := createAutoModelRouteTestChannel(t, db, group, "a", "b", "c")
	c, info, retry := newAutoModelRouteTestContext(t, group, []service.AutoModelRoute{
		{Group: group, ModelName: "a", ChannelID: channel.Id},
		{Group: group, ModelName: "b", ChannelID: channel.Id},
		{Group: group, ModelName: "c", ChannelID: channel.Id},
	})
	bindInitialAutoModelRouteForTest(t, c, info)
	service.RecordAutoModelUnusableAnswer(group, "b", channel.Id, "empty")
	require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
	require.Equal(t, 2, service.GetAutoModelRouteIndex(c))
	require.Equal(t, "c", info.OriginModelName)
	require.Equal(t, channel.Id, info.ChannelId)
}

func TestAutoModelRouteAuthorizationDoesNotExpandGroupOrTokenScope(t *testing.T) {
	c := newAutoModelFeedbackTestContext()
	common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
	common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"allowed": false, "auto": true})
	common.SetContextKey(c, constant.ContextKeyTokenGroups, []string{"g1", "g2"})
	info := &relaycommon.RelayInfo{TokenGroup: "auto", UserGroup: "default", UsingGroup: "g1"}
	route := service.AutoModelRoute{Group: "g1", ModelName: "allowed", ChannelID: 1}
	require.True(t, isAutoModelRouteAuthorized(c, info, route), "match by whitelist key presence, like explicit requests")
	route.ModelName = "denied"
	require.False(t, isAutoModelRouteAuthorized(c, info, route), "the virtual auto name grants no concrete-model permission")
	route.ModelName, route.Group = "allowed", "g2"
	require.False(t, isAutoModelRouteAuthorized(c, info, route), "cross-group retry stays disabled")
	common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, true)
	require.True(t, isAutoModelRouteAuthorized(c, info, route))
	route.Group = "not-in-token"
	require.False(t, isAutoModelRouteAuthorized(c, info, route))
	info.TokenGroup, route.Group = "g1", "g2"
	require.False(t, isAutoModelRouteAuthorized(c, info, route), "normal-group plans cannot grant another group")
	route.Group = "g1"
	common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "1")
	require.False(t, isAutoModelRouteAuthorized(c, info, route), "a plan cannot override a pinned-channel request")
}

func TestAutoModelRouteSwitchRefreshesMetadataAndPreservesRawRequestFields(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	first := createAutoModelRouteTestChannel(t, db, "default", "old")
	other := createAutoModelRouteTestChannel(t, db, "default", "new")
	other.Key, other.BaseURL = "new-key", common.GetPointer("https://new.example")
	other.SetSetting(dto.ChannelSettings{})
	require.NoError(t, db.Save(other).Error)
	c, info, retry := newAutoModelRouteTestContext(t, "default", []service.AutoModelRoute{
		{Group: "default", ModelName: "old", ChannelID: first.Id},
		{Group: "default", ModelName: "new", ChannelID: other.Id},
	})
	bindInitialAutoModelRouteForTest(t, c, info)
	oldStorage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	for _, key := range []string{"api_version", "region", "plugin", "bot_id", "channel_organization"} {
		c.Set(key, "old-value")
	}
	common.SetContextKey(c, constant.ContextKeyChannelMultiKeyIndex, 9)
	info.ChannelMeta = &relaycommon.ChannelMeta{ChannelId: first.Id, ChannelType: constant.ChannelTypeAzure, ApiKey: "old-key", Organization: "old-org"}
	info.TieredBillingSnapshot = &billingexpr.BillingSnapshot{ModelName: "old"}
	info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(`{"model":"old"}`)}
	info.PriceData.ModelPrice = 999999
	info.ModelMappingTargetName, info.ModelMappingBypassed = "old-mapped", true
	info.UseRuntimeHeadersOverride = true
	info.RuntimeHeadersOverride = map[string]interface{}{"Authorization": "old-key"}
	require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
	require.Equal(t, other.Id, info.ChannelId)
	require.Equal(t, "new-key", info.ApiKey)
	require.Equal(t, "https://new.example", info.ChannelBaseUrl)
	require.Equal(t, "new", info.UpstreamModelName)
	require.Empty(t, info.Organization)
	require.Empty(t, info.ApiVersion)
	require.Zero(t, info.ChannelMultiKeyIndex)
	require.Nil(t, info.TieredBillingSnapshot, "a ratio-priced route cannot settle using a prior model's tiered snapshot")
	require.Nil(t, info.BillingRequestInput)
	require.NotEqual(t, float64(999999), info.PriceData.ModelPrice)
	require.Empty(t, info.ModelMappingTargetName)
	require.False(t, info.ModelMappingBypassed)
	require.False(t, info.UseRuntimeHeadersOverride)
	require.Nil(t, info.RuntimeHeadersOverride)
	for _, key := range []string{"region", "plugin", "bot_id", "channel_organization"} {
		require.Empty(t, c.GetString(key))
	}
	require.Equal(t, "new", info.Request.(*dto.GeneralOpenAIRequest).Model)
	storage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	body, err := storage.Bytes()
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, common.Unmarshal(body, &fields))
	require.JSONEq(t, `"new"`, string(fields["model"]))
	require.Equal(t, "0", string(fields["temperature"]))
	require.Equal(t, "false", string(fields["stream"]))
	require.JSONEq(t, `{"integer":9007199254740993,"zero":0}`, string(fields["unknown"]))
	_, err = oldStorage.Bytes()
	require.ErrorIs(t, err, common.ErrStorageClosed, "a committed replacement releases the previous storage")
}

func TestAutoModelRouteRewriteHandlesJSONContentTypesWithoutLosingZeroes(t *testing.T) {
	for _, contentType := range []string{"", "application/json; charset=utf-8", "application/vnd.api+json"} {
		t.Run(contentType, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
				`{"model":"auto","zero":0,"flag":false,"extra":{"integer":9007199254740993}}`))
			if contentType != "" {
				c.Request.Header.Set("Content-Type", contentType)
			}
			t.Cleanup(func() { common.CleanupBodyStorage(c) })
			old, replacement, err := rewriteAutoModelRequestBody(c, "actual-model")
			require.NoError(t, err)
			require.NotNil(t, replacement)
			t.Cleanup(func() { _ = old.Close() })
			body, err := replacement.Bytes()
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, common.Unmarshal(body, &fields))
			require.Equal(t, `"actual-model"`, string(fields["model"]))
			require.Equal(t, "0", string(fields["zero"]))
			require.Equal(t, "false", string(fields["flag"]))
			require.Contains(t, string(fields["extra"]), "9007199254740993")
		})
	}
}

func TestAutoModelRoutePricingFailureLeavesOldSnapshotAndBodyIntact(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	channel := createAutoModelRouteTestChannel(t, db, "default", "old", "unpriced-auto-route-controller-model")
	c, info, retry := newAutoModelRouteTestContext(t, "default", []service.AutoModelRoute{
		{Group: "default", ModelName: "old", ChannelID: channel.Id},
		{Group: "default", ModelName: "unpriced-auto-route-controller-model", ChannelID: channel.Id},
	})
	bindInitialAutoModelRouteForTest(t, c, info)
	info.UserSetting.AcceptUnsetRatioModel = false
	oldSnapshot := &billingexpr.BillingSnapshot{ModelName: "old", EstimatedTier: "old-tier"}
	oldInput := &billingexpr.RequestInput{Body: []byte(`{"model":"old"}`)}
	info.TieredBillingSnapshot, info.BillingRequestInput = oldSnapshot, oldInput
	info.PriceData.ModelPrice = 123
	oldMeta := info.ChannelMeta
	storage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	body, err := storage.Bytes()
	require.NoError(t, err)
	require.False(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
	require.Equal(t, 0, service.GetAutoModelRouteIndex(c))
	require.Same(t, oldSnapshot, info.TieredBillingSnapshot)
	require.Same(t, oldInput, info.BillingRequestInput)
	require.Same(t, oldMeta, info.ChannelMeta)
	require.Equal(t, float64(123), info.PriceData.ModelPrice)
	require.Equal(t, "old", retry.ModelName)
	require.Equal(t, "old", info.Request.(*dto.GeneralOpenAIRequest).Model)
	currentStorage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	require.Same(t, storage, currentStorage)
	currentBody, err := currentStorage.Bytes()
	require.NoError(t, err)
	require.Equal(t, body, currentBody)
}

func TestAutoModelRouteTieredSwitchPricesConcreteRawModel(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error { saved[key] = value; return nil }))
	t.Cleanup(func() { require.NoError(t, config.GlobalConfig.LoadFromDB(saved)) })
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"route-tiered":"tiered_expr"}`,
		"billing_setting.billing_expr": `{"route-tiered":"param(\"model\") == \"route-tiered\" ? tier(\"new\", p * 3) : tier(\"stale\", p * 99)"}`,
	}))
	channel := createAutoModelRouteTestChannel(t, db, "default", "old", "route-tiered")
	c, info, retry := newAutoModelRouteTestContext(t, "default", []service.AutoModelRoute{
		{Group: "default", ModelName: "old", ChannelID: channel.Id},
		{Group: "default", ModelName: "route-tiered", ChannelID: channel.Id},
	})
	bindInitialAutoModelRouteForTest(t, c, info)
	info.BillingRequestInput = &billingexpr.RequestInput{Body: []byte(`{"model":"old"}`)}
	require.True(t, trySwitchAutoModel(c, info, retry, 1000, &types.TokenCountMeta{}))
	require.NotNil(t, info.TieredBillingSnapshot)
	require.Equal(t, "route-tiered", info.TieredBillingSnapshot.ModelName)
	require.Equal(t, "new", info.TieredBillingSnapshot.EstimatedTier)
	input, err := helper.ResolveIncomingBillingExprRequestInput(c, info)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, common.Unmarshal(input.Body, &fields))
	require.JSONEq(t, `"route-tiered"`, string(fields["model"]))
}

func TestAutoModelAttemptBudgetFreezesFirstDispatchOverride(t *testing.T) {
	oldRetryTimes := common.RetryTimes
	t.Cleanup(func() { common.RetryTimes = oldRetryTimes })
	for _, tc := range []struct {
		name         string
		global       int
		override     *int
		wantAttempts int
	}{
		{name: "global", global: 3, wantAttempts: 4},
		{name: "zero", global: 0, wantAttempts: 1},
		{name: "negative-global", global: -1, wantAttempts: 1},
		{name: "override", global: 20, override: common.GetPointer(2), wantAttempts: 3},
		{name: "zero-override", global: 20, override: common.GetPointer(0), wantAttempts: 1},
		{name: "negative-override", global: 20, override: common.GetPointer(-5), wantAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common.RetryTimes = tc.global
			var budget autoModelAttemptBudget
			retry := &service.RetryParam{}
			require.True(t, budget.beginAttempt(&model.Channel{RetryTimes: tc.override}, retry))
			for budget.canAttempt() {
				require.Less(t, budget.attempts, tc.wantAttempts)
				require.True(t, budget.beginAttempt(&model.Channel{RetryTimes: common.GetPointer(1000)}, retry))
			}
			require.Equal(t, tc.wantAttempts, budget.attempts)
			require.Zero(t, budget.remainingRetries)
			require.Equal(t, tc.wantAttempts-1, retry.GetEffectiveRetryTimes())
			require.False(t, budget.beginAttempt(&model.Channel{}, retry))
		})
	}
}

func TestAutoModelRetryStopsOnBudgetSkipResponseAndCancellation(t *testing.T) {
	oldAuto := operation_setting.AutoModelEnabled
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = oldAuto })
	c := newAutoModelFeedbackTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{}
	info.MarkUpstreamDispatch()
	upstream := types.NewErrorWithStatusCode(errors.New("model unsupported"), types.ErrorCodeGetChannelFailed, http.StatusNotFound)
	require.True(t, prepareAutoModelRetry(c, info, upstream, 1))
	require.False(t, prepareAutoModelRetry(c, info, upstream, 0), "404 must not acquire two uncounted hard switches")
	require.False(t, prepareAutoModelRetry(c, info, types.NewErrorWithStatusCode(errors.New("local validation"),
		types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry()), 10))
	channelErr := types.NewError(errors.New("channel error"), types.ErrorCodeChannelNoAvailableKey)
	require.False(t, prepareAutoModelRetry(c, info, channelErr, 0), "channel errors cannot bypass the total budget")
	_, err := c.Writer.Write([]byte("partial response"))
	require.NoError(t, err)
	require.False(t, prepareAutoModelRetry(c, info, upstream, 10))
	active := newAutoModelFeedbackTestContext()
	ctx, cancel := context.WithCancel(context.Background())
	active.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	cancel()
	require.False(t, prepareAutoModelRetry(active, info, upstream, 10))
	require.Same(t, upstream, autoModelCanceledRequestError(active, upstream))
}
