package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestInvalidAutoAnswerSwitchesBeforeCommit(t *testing.T) {
	oldSelfUse := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = true
	t.Cleanup(func() { operation_setting.SelfUseModeEnabled = oldSelfUse })

	db := openAutoModelRouteControllerTestDB(t)
	group := "invalid-switch"
	first := createAutoModelRouteTestChannel(t, db, group, "a")
	other := createAutoModelRouteTestChannel(t, db, group, "a")
	routes := []service.AutoModelRoute{
		{Group: group, ModelName: "a", ChannelID: first.Id},
		{Group: group, ModelName: "a", ChannelID: other.Id},
	}
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"auto","messages":[{"role":"user","content":"hello"}],"stream":false}`))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	common.SetContextKey(c, constant.ContextKeyAutoModelRequestGroup, group)
	common.SetContextKey(c, constant.ContextKeyTokenGroup, group)
	common.SetContextKey(c, constant.ContextKeyUserGroup, group)
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	service.SetAutoModelRoutePlan(c, routes)
	request := &dto.GeneralOpenAIRequest{Model: "auto", Stream: common.GetPointer(false)}
	info := relaycommon.GenRelayInfoOpenAI(c, request)
	info.TokenGroup, info.UserGroup, info.UsingGroup = group, group, group
	info.UserSetting = dto.UserSetting{AcceptUnsetRatioModel: true}
	retry := &service.RetryParam{Ctx: c, TokenGroup: group, ModelName: "a", Retry: common.GetPointer(0)}
	_ = bindInitialAutoModelRouteForTest(t, c, info)
	relaycommon.InstallRelayResponseWriter(c)
	info.BeginAttempt()
	info.MarkUpstreamDispatch()

	staged := relaycommon.InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, staged)
	_, err := staged.Write([]byte(`{"choices":[{"index":0,"message":{"content":"[Gateway Warning: busy]"},"finish_reason":"stop"}]}`))
	require.NoError(t, err)
	require.False(t, staged.Committed())

	unusable, reason := info.ClientAnswerUnusable()
	require.True(t, unusable, reason)
	recordAutoModelUnusableAnswer(c, info, reason)
	budget := autoModelAttemptBudget{attempts: 1, dispatched: 1, started: true}
	require.Empty(t, autoModelInvalidSwitchBlockedReason(c, info, budget, staged),
		"an uncommitted invalid answer with budget left must be switchable")
	exhausted := autoModelAttemptBudget{
		attempts: operation_setting.AutoModelMaxAttempts(), dispatched: operation_setting.AutoModelMaxAttempts(), started: true,
	}
	require.Equal(t, "budget_exhausted", autoModelInvalidSwitchBlockedReason(c, info, exhausted, staged),
		"no real dispatch budget means no further switch")

	staged.Discard()
	staged.Restore()
	flushAutoModelFeedback(c)
	require.Empty(t, takeAutoModelFeedback(c),
		"an immediately published failure must not remain queued for the final flush")
	require.True(t, trySwitchAutoModel(c, info, retry, 0, &types.TokenCountMeta{}))
	require.Equal(t, other.Id, info.ChannelId, "the failed pair is skipped, not the whole model/channel family")

	second := relaycommon.InstallAutoAttemptResponseWriter(c)
	require.NotNil(t, second)
	_, err = second.Write([]byte(`{"choices":[{"index":0,"message":{"content":"real answer"},"finish_reason":"stop"}]}`))
	require.NoError(t, err)
	require.True(t, second.Committed())
	require.Equal(t, "response_committed", autoModelInvalidSwitchBlockedReason(c, info, budget, second),
		"once any candidate output is committed, another candidate must not be spliced in")
	require.Contains(t, recorder.Body.String(), "real answer")
	require.NotContains(t, recorder.Body.String(), "Gateway Warning")

	cooldown, active := service.GetAutoModelCooldown(group, "a", first.Id, time.Now())
	require.True(t, active, "the invalid pair must be cooling before the switch")
	require.GreaterOrEqual(t, cooldown.Level, 1)
	_, active = service.GetAutoModelCooldown(group, "a", other.Id, time.Now())
	require.False(t, active, "the sibling route must remain eligible")
}
