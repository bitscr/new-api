package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// AutoModelPermanentFailure 的判据完全来自数据库里管理员配置的关键词:
// 源码不内置任何状态码/错误码/文案判据,未配置关键词时没有任何失败会被判成确定性。
// 本测试把"过去会被内置判据命中的失败"逐一钉住:现在必须全部按抖动处理。
func TestAutoModelPermanentFailureWithoutKeywordsIsAlwaysFalse(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("")

	cases := []struct {
		name string
		err  *types.NewAPIError
	}{
		{"auth401", types.NewErrorWithStatusCode(errors.New("Invalid API key"),
			types.ErrorCodeInvalidRequest, http.StatusUnauthorized)},
		{"auth403", types.NewErrorWithStatusCode(errors.New("Credential access forbidden"),
			types.ErrorCodeInvalidRequest, http.StatusForbidden)},
		{"model-not-found-code", types.NewErrorWithStatusCode(errors.New("Requested model not found"),
			types.ErrorCodeModelNotFound, http.StatusNotFound)},
		{"model-not-supported", types.NewErrorWithStatusCode(errors.New("model_not_supported"),
			types.ErrorCode("model_not_supported"), http.StatusBadRequest)},
		{"unsupported-model-feature", types.NewErrorWithStatusCode(
			errors.New("This model does not support the requested feature"),
			types.ErrorCode("unsupported_model_feature"), http.StatusUnprocessableEntity)},
		{"capability-wording", types.NewOpenAIError(
			errors.New("tools / function calling is not supported by the upstream backend"),
			types.ErrorCode("invalid_request_error"), http.StatusBadRequest)},
		{"generic-404", types.NewErrorWithStatusCode(errors.New("Endpoint not found"),
			types.ErrorCode("route_not_found"), http.StatusNotFound)},
		{"server-error", types.NewErrorWithStatusCode(errors.New("Upstream unavailable"),
			types.ErrorCode("server_error"), http.StatusServiceUnavailable)},
		{"gateway-timeout", types.NewErrorWithStatusCode(errors.New("Upstream timed out"),
			types.ErrorCode("bad_response_status_code"), http.StatusGatewayTimeout)},
		{"cloudflare-524", types.NewErrorWithStatusCode(errors.New("bad response status code 524"),
			types.ErrorCode("bad_response_status_code"), 524)},
		{"rate-limit", types.NewErrorWithStatusCode(errors.New("Account rate limit exceeded"),
			types.ErrorCode("rate_limit_exceeded"), http.StatusTooManyRequests)},
		{"context-overflow", types.NewErrorWithStatusCode(errors.New("Maximum context length exceeded"),
			types.ErrorCode("context_length_exceeded"), http.StatusBadRequest)},
		{"nil", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, AutoModelPermanentFailure(tc.err),
				"没有配置关键词时,任何失败都不能被判成确定性")
		})
	}
}

// 管理员关键词命中(错误正文或错误码,大小写不敏感)即判确定性;这是唯一的正向判据。
func TestAutoModelPermanentFailureFollowsConfiguredKeywords(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })

	err := types.NewErrorWithStatusCode(errors.New("Requested model not found"),
		types.ErrorCodeModelNotFound, http.StatusNotFound)

	operation_setting.SetAutoModelPermanentKeywords("model not found")
	require.True(t, AutoModelPermanentFailure(err))

	operation_setting.SetAutoModelPermanentKeywords("MODEL_NOT_FOUND")
	require.True(t, AutoModelPermanentFailure(err), "错误码上的大小写不敏感匹配也要生效")

	operation_setting.SetAutoModelPermanentKeywords("some other upstream string")
	require.False(t, AutoModelPermanentFailure(err), "关键词不命中就不能扩大打击面")
}

// 普通冷却关键词优先:管理员显式钉回抖动的失败,即使同时命中确定性关键词,
// 也必须按抖动处理。
func TestTransientKeywordDowngradesPermanentMatch(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	t.Cleanup(func() { operation_setting.SetAutoModelTransientKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("model not found")
	operation_setting.SetAutoModelTransientKeywords("model not found")

	err := types.NewErrorWithStatusCode(errors.New("Requested model not found"),
		types.ErrorCodeModelNotFound, http.StatusNotFound)
	require.False(t, AutoModelPermanentFailure(err))
}

// 保护项优先于关键词:上下文超长是"这次请求太长",不是"模型不存在"。
// 管理员加错关键词也不能把整个模型摘掉。
func TestKeywordCannotOverrideProtectedTransient(t *testing.T) {
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("maximum context length")

	err := types.NewErrorWithStatusCode(errors.New("Maximum context length exceeded"),
		types.ErrorCode("context_length_exceeded"), http.StatusBadRequest)

	require.False(t, AutoModelPermanentFailure(err),
		"上下文超长必须永远按抖动处理,即使关键词命中")
}

// 老行(没有 permanent 列/值为假)必须按抖动冷却载入,不能被误升为永久。
func TestLegacyCooldownRowLoadsAsTransient(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
		Group: "default", Model: "old-model", ChannelId: 1,
		Until: time.Now().Add(time.Hour).Unix(), Level: 2, Reason: "请求失败",
		UpdatedAt: time.Now().Unix(),
	}))
	LoadAutoModelCooldowns()
	state, ok := autoModelCooldowns.Get(autoModelHealthKey("default", "old-model", 1))
	require.True(t, ok)
	require.Equal(t, 2, state.Level)
}

// 确定性失败不再有冷却档:存量 Permanent=true 的行(旧版本的"永久级冷却")
// 在载入时直接清库,否则新代码没有任何路径会去清除它们。
func TestLegacyPermanentCooldownRowIsPurged(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
		Group: "default", Model: "ghost-model", ChannelId: 1,
		Until: time.Now().Add(24 * time.Hour).Unix(), Level: 1, Reason: "模型不存在",
		Permanent: true,
		UpdatedAt: time.Now().Unix(),
	}))
	LoadAutoModelCooldowns()

	require.Empty(t, cooldownRows(t), "旧版确定性冷却记录应在载入时被清掉")
	_, ok := autoModelCooldowns.Get(autoModelHealthKey("default", "ghost-model", 1))
	require.False(t, ok)
}

// 命中确定性关键词的组合在第一次失败时就被直接摘除:模型从渠道声明里删除,
// 全程没有任何冷却记录。再犯是幂等的,不会重复写库。
func TestPermanentFailureRetiresCombinationImmediately(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("model not found")

	require.NoError(t, db.Create(&model.Channel{
		Id: 11, Key: "k", Status: common.ChannelStatusEnabled, Group: "g", Models: "m1,m2",
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "g", Model: "m1", ChannelId: 11, Enabled: true},
		{Group: "g", Model: "m2", ChannelId: 11, Enabled: true},
	}).Error)

	RecordAutoModelPermanentFailure("g", "m1", 11, "模型不存在(model_not_found)")

	channel, err := model.GetChannelById(11, false)
	require.NoError(t, err)
	require.Equal(t, "m2", channel.Models, "命中确定性关键词的模型应立即从渠道里删除")
	require.Empty(t, ListAutoModelCooldowns(time.Now()), "确定性失败不再产生任何冷却")

	RecordAutoModelPermanentFailure("g", "m1", 11, "模型不存在(model_not_found)")
	channel, err = model.GetChannelById(11, false)
	require.NoError(t, err)
	require.Equal(t, "m2", channel.Models, "再犯是幂等的")
}

// 渠道被摘掉最后一个模型后自动禁用,状态里带原因。
func TestPermanentFailureDisablesEmptiedChannel(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("model not found")

	require.NoError(t, db.Create(&model.Channel{
		Id: 12, Key: "k", Status: common.ChannelStatusEnabled, Group: "g", Models: "only",
	}).Error)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "g", Model: "only", ChannelId: 12, Enabled: true},
	}).Error)

	RecordAutoModelPermanentFailure("g", "only", 12, "模型不存在(model_not_found)")

	channel, err := model.GetChannelById(12, false)
	require.NoError(t, err)
	require.Empty(t, channel.Models)
	require.Equal(t, common.ChannelStatusAutoDisabled, channel.Status,
		"渠道没有剩余模型时应被自动禁用")
}
