package controller

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

func TestAutoModelFailedChannelExclusionIsNotLocalLimit(t *testing.T) {
	db := openAutoModelRouteControllerTestDB(t)
	first := createAutoModelRouteTestChannel(t, db, "default", "a", "b")
	other := createAutoModelRouteTestChannel(t, db, "default", "a")
	routes := []service.AutoModelRoute{
		{Group: "default", ModelName: "a", ChannelID: first.Id},
		{Group: "default", ModelName: "b", ChannelID: first.Id},
		{Group: "default", ModelName: "a", ChannelID: other.Id},
	}
	c, info, retry := newAutoModelRouteTestContext(t, "default", routes)
	bindInitialAutoModelRouteForTest(t, c, info)
	service.MarkAutoModelChannelFailed(c, first.Id)
	selected, apiErr := getChannel(c, info, retry)
	require.Nil(t, selected)
	require.NotNil(t, apiErr)
	require.Equal(t, types.ErrorCodeGetChannelFailed, apiErr.GetErrorCode(), "upstream exclusion must not fabricate a local daily limit")
	require.Empty(t, service.GetChannelDailySuccessLimitSkippedIDs(c))
	require.Empty(t, service.GetChannelRPMLimitSkippedIDs(c))
	require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
	require.Equal(t, other.Id, info.ChannelId, "all remaining models on the failed channel are skipped in this request")

	next, nextInfo, nextRetry := newAutoModelRouteTestContext(t, "default", routes)
	selected, apiErr = getChannel(next, nextInfo, nextRetry)
	require.Nil(t, apiErr)
	require.Equal(t, first.Id, selected.Id, "a request-only exclusion must not leak to a new request")
	cooldowns, err := model.GetAutoModelCooldowns()
	require.NoError(t, err)
	require.Empty(t, cooldowns, "excluding the channel must not create persistent channel-wide cooldowns")
}

func TestAutoModelRetryPolicyAdvancesByErrorScope(t *testing.T) {
	oldCodes := operation_setting.AutomaticRetryStatusCodesToString()
	t.Cleanup(func() { require.NoError(t, operation_setting.AutomaticRetryStatusCodesFromString(oldCodes)) })
	for _, tc := range []struct {
		name        string
		status      int
		code        string
		message     string
		param       string
		codes       string
		wantRetry   bool
		wantChannel bool
		local       bool
	}{
		{name: "account401", status: 401, code: "invalid_api_key", message: "Invalid API key", wantRetry: true, wantChannel: true},
		{name: "account429", status: 429, code: "rate_limit_exceeded", message: "Account rate limit exceeded", wantRetry: true, wantChannel: true},
		{name: "account429_model_hint", status: 429, code: "rate_limit_exceeded", message: "Account-wide rate limit exceeded for model 'gpt-4o'.", wantRetry: true, wantChannel: true},
		{name: "generic503", status: 503, code: "server_error", message: "Upstream unavailable", wantRetry: true, wantChannel: true},
		{name: "model403", status: 403, code: "model_access_denied", message: "Access to this model is denied", wantRetry: true},
		{name: "model429", status: 429, code: "model_rate_limit_exceeded", message: "Rate limit exceeded for this model", wantRetry: true},
		{name: "model503", status: 503, code: "model_overloaded", message: "This model is overloaded", wantRetry: true},
		{name: "model_only_account_qualifier", status: 429, code: "model_rate_limit_exceeded", param: "model", message: "Your account rate limit is exceeded only for model 'gpt-4o'; other models remain available.", codes: "429", wantRetry: true},
		{name: "negated_compatibility", status: 400, code: "invalid_request", message: "No model is unsupported. The request JSON is malformed.", codes: "429"},
		{name: "negated_overload", status: 503, code: "upstream_error", message: "No model is overloaded; upstream maintenance is in progress.", codes: "503", wantRetry: true, wantChannel: true},
		{name: "model404", status: 404, code: "model_not_found", message: "Requested model not found", wantRetry: true},
		{name: "context400", status: 400, code: "context_length_exceeded", message: "Maximum context length exceeded", wantRetry: true},
		{name: "context_capacity_only", status: 400, code: "invalid_request", message: "This model's maximum context length is 8192 tokens; the request JSON is malformed.", codes: "429"},
		{name: "account_model_only_colon", status: 429, code: "model_rate_limit_exceeded", param: "model", message: "Your account rate limit is exceeded: only for model 'gpt-4o'; other models remain available.", codes: "429", wantRetry: true},
		{name: "unrelated_semicolon_excluded", status: 400, code: "invalid_request", message: "This endpoint does not support streaming; a validation error occurred with this model.", codes: "429"},
		{name: "unrelated_semicolon_enabled", status: 400, code: "invalid_request", message: "This endpoint does not support streaming; a validation error occurred with this model.", codes: "400", wantRetry: true, wantChannel: true},
		{name: "unrelated_colon_excluded", status: 400, code: "invalid_request", message: "This endpoint does not support streaming: a validation error occurred with this model.", codes: "429"},
		{name: "unrelated_colon_enabled", status: 400, code: "invalid_request", message: "This endpoint does not support streaming: a validation error occurred with this model.", codes: "400", wantRetry: true, wantChannel: true},
		{name: "account_owner_conditional", status: 429, code: "rate_limit_exceeded", message: "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct.", codes: "429", wantRetry: true, wantChannel: true},
		{name: "account_owner_conditional_with_is", status: 429, code: "rate_limit_exceeded", message: "Your account rate limit is exceeded for model 'gpt-4o'; only if the estimate is correct.", codes: "429", wantRetry: true, wantChannel: true},
		{name: "account_owner_affirmative", status: 429, code: "rate_limit_exceeded", message: "Your account rate limit exceeded for model 'gpt-4o'.", codes: "429", wantRetry: true},
		{name: "account_owner_model_first", status: 429, code: "rate_limit_exceeded", message: "Your account rate limit for model 'gpt-4o' exceeded.", codes: "429", wantRetry: true},
		{name: "account_owner_structured_code", status: 429, code: "model_rate_limit_exceeded", message: "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct.", codes: "429", wantRetry: true},
		{name: "account_owner_excluded_status", status: 429, code: "rate_limit_exceeded", message: "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct.", codes: "503"},
		{name: "account_owner_later_model_clause", status: 429, code: "rate_limit_exceeded", message: "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct. This model is overloaded.", codes: "429", wantRetry: true},
		{name: "generic400", status: 400, code: "invalid_request", message: "Invalid request field"},
		{name: "generic404", status: 404, code: "route_not_found", message: "Endpoint not found"},
		{name: "generic422", status: 422, code: "invalid_request", message: "Invalid request structure"},
		{name: "generic400_enabled", status: 400, code: "invalid_request", message: "Invalid request field", codes: "400", wantRetry: true, wantChannel: true},
		{name: "excluded429", status: 429, code: "rate_limit_exceeded", message: "Account rate limit", codes: "500-599"},
		{name: "enabled408", status: 408, code: "request_timeout", message: "Upstream request timeout", codes: "408", wantRetry: true, wantChannel: true},
		{name: "local400", status: 400, code: "invalid_request", message: "Local conversion failed", local: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codes := tc.codes
			if codes == "" {
				codes = "401-403,413,429,500-599"
			}
			require.NoError(t, operation_setting.AutomaticRetryStatusCodesFromString(codes))
			db := openAutoModelRouteControllerTestDB(t)
			group := t.Name()
			first := createAutoModelRouteTestChannel(t, db, group, "a", "b")
			other := createAutoModelRouteTestChannel(t, db, group, "a")
			c, info, retry := newAutoModelRouteTestContext(t, group, []service.AutoModelRoute{
				{Group: group, ModelName: "a", ChannelID: first.Id},
				{Group: group, ModelName: "b", ChannelID: first.Id},
				{Group: group, ModelName: "a", ChannelID: other.Id},
			})
			bindInitialAutoModelRouteForTest(t, c, info)
			info.BeginAttempt()
			if !tc.local {
				info.MarkUpstreamDispatch()
			}
			markCurrentAutoModelRouteAttempted(c)
			upstream := types.WithOpenAIError(types.OpenAIError{Code: tc.code, Type: "invalid_request_error", Message: tc.message, Param: tc.param}, tc.status)
			if tc.local {
				upstream = types.NewErrorWithStatusCode(errors.New(tc.message), types.ErrorCodeInvalidRequest, http.StatusBadRequest)
			}
			info.LastError = upstream
			allowed := prepareAutoModelRetry(c, info, upstream, 2)
			require.Equal(t, tc.wantRetry, allowed, "retry eligibility must distinguish compatibility from generic or local errors")
			require.Equal(t, tc.wantChannel, service.GetAutoModelHardExcludedChannelIDs(c)[first.Id], "retry preparation must exclude only channel-scoped failures")
			require.Empty(t, service.GetChannelDailySuccessLimitSkippedIDs(c))
			require.Empty(t, service.GetChannelRPMLimitSkippedIDs(c))
			if allowed {
				require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
				want := first.Id
				if tc.wantChannel {
					want = other.Id
				}
				require.Equal(t, want, info.ChannelId, "the actual route switch must honor the classified scope")
			}
			require.Same(t, upstream, info.LastError, "scope handling must retain the real failure")
		})
	}
}
