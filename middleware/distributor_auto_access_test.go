package middleware

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestInitialAutoRouteKeepsSelectedChannelAndSkipsUnauthorizedModels(t *testing.T) {
	for _, failure := range []error{nil, errors.New("selection failed")} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
		common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
		common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"first": true, "last": true})
		routes := []service.AutoModelRoute{
			{Group: "initial-route", ModelName: "first", ChannelID: 41},
			{Group: "initial-route", ModelName: "denied", ChannelID: 41},
			{Group: "initial-route", ModelName: "last", ChannelID: 41},
			{Group: "initial-route", ModelName: "last", ChannelID: 42},
		}
		service.SetAutoModelRoutePlan(c, routes)
		request := &ModelRequest{Model: "first"}
		var attempts []service.AutoModelRoute
		selected, group, err := selectInitialAutoRoute(c, request, func(route service.AutoModelRoute) (*model.Channel, error) {
			attempts = append(attempts, route)
			if route.ModelName == "first" {
				return nil, failure
			}
			return &model.Channel{Id: route.ChannelID, Status: common.ChannelStatusEnabled}, nil
		})
		require.NoError(t, err)
		require.NotNil(t, selected)
		require.Equal(t, 41, selected.Id)
		require.Equal(t, "initial-route", group)
		require.Equal(t, "last", request.Model)
		require.Equal(t, []service.AutoModelRoute{routes[0], routes[2]}, attempts)
		require.Equal(t, 2, service.GetAutoModelRouteIndex(c))
	}
}

func TestInitialAutoRouteSkipsWholeLimitedChannelAndDoesNotUseAffinity(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
	service.SetAutoModelRoutePlan(c, []service.AutoModelRoute{
		{Group: "initial-limits", ModelName: "a", ChannelID: 41},
		{Group: "initial-limits", ModelName: "b", ChannelID: 41},
		{Group: "initial-limits", ModelName: "c", ChannelID: 42},
	})
	service.MarkChannelRPMLimitSkipped(c, 41)
	c.Set("channel_affinity_skip_retry_on_failure", true)
	var ids []int
	selected, _, err := selectInitialAutoRoute(c, &ModelRequest{}, func(route service.AutoModelRoute) (*model.Channel, error) {
		ids = append(ids, route.ChannelID)
		return &model.Channel{Id: route.ChannelID, Status: common.ChannelStatusEnabled}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.Equal(t, []int{42}, ids)
}

func TestInitialAutoRouteRejectsStaleChannelsAndCancellation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	service.SetAutoModelRoutePlan(c, []service.AutoModelRoute{
		{Group: "initial-stale", ModelName: "m", ChannelID: 41},
		{Group: "initial-stale", ModelName: "m", ChannelID: 42},
	})
	selected, _, err := selectInitialAutoRoute(c, &ModelRequest{}, func(route service.AutoModelRoute) (*model.Channel, error) {
		if route.ChannelID == 41 {
			return &model.Channel{Id: 41, Status: common.ChannelStatusManuallyDisabled}, nil
		}
		return &model.Channel{Id: 99, Status: common.ChannelStatusEnabled}, nil
	})
	require.NoError(t, err)
	require.Nil(t, selected, "must not dispatch a disabled or mismatched channel")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
	selected, _, err = selectInitialAutoRoute(c, &ModelRequest{}, func(route service.AutoModelRoute) (*model.Channel, error) {
		t.Fatal("canceled request must not look up another channel")
		return nil, nil
	})
	require.Nil(t, selected)
	require.ErrorIs(t, err, context.Canceled)
}
