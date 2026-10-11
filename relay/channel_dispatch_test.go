package relay

import (
	"errors"
	"net/http"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type channelDispatchSeam struct {
	name string
	run  func(*gin.Context, *relaycommon.RelayInfo, func() (*http.Response, error)) (*http.Response, error)
}

func channelDispatchSeams() []channelDispatchSeam {
	return []channelDispatchSeam{
		{name: "normal", run: func(c *gin.Context, info *relaycommon.RelayInfo, request func() (*http.Response, error)) (*http.Response, error) {
			response, err := doChannelRPMGuardedRequest(c, info, func() (any, error) { return request() })
			if response == nil {
				return nil, err
			}
			return response.(*http.Response), err
		}},
		{name: "task", run: doChannelRPMGuardedTaskRequest},
	}
}

func TestChannelRPMGuardedSeamsMarkUpstreamDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, seam := range channelDispatchSeams() {
		for _, fails := range []bool{false, true} {
			name := seam.name + "/success"
			if fails {
				name = seam.name + "/request_error"
			}
			t.Run(name, func(t *testing.T) {
				const channelID = 930001
				withMemoryChannelRPM(t, channelID)
				c, info := channelRPMTestContext(), channelRPMTestInfo(channelID, 2)
				info.BeginAttempt()
				require.False(t, info.HasUpstreamDispatch())
				var wantErr error
				wantResponse := &http.Response{StatusCode: http.StatusOK}
				if fails {
					wantErr = errors.New("upstream connection failed")
					wantResponse = nil
				}
				called, markedInCallback := false, false
				response, err := seam.run(c, info, func() (*http.Response, error) {
					called, markedInCallback = true, info.HasUpstreamDispatch()
					return wantResponse, wantErr
				})
				require.True(t, called)
				require.Same(t, wantResponse, response)
				require.Equal(t, wantErr, err)
				require.True(t, markedInCallback, "dispatch must be marked before invoking the upstream callback")
				info.EndAttempt()
				require.True(t, info.HasUpstreamDispatch(), "a failed connection still entered dispatch")
			})
		}
	}
}

func TestChannelRPMGuardedSeamsDeniedAttemptDoesNotDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, seam := range channelDispatchSeams() {
		t.Run(seam.name, func(t *testing.T) {
			const channelID = 930002
			withMemoryChannelRPM(t, channelID)
			c, info := channelRPMTestContext(), channelRPMTestInfo(channelID, 1)
			require.True(t, service.TryAcquireChannelRPM(c.Request.Context(), channelID, info.ChannelSetting.RPMProtection).Allowed)
			info.BeginAttempt()
			called := false
			response, err := seam.run(c, info, func() (*http.Response, error) {
				called = true
				return &http.Response{StatusCode: http.StatusOK}, nil
			})
			require.Nil(t, response)
			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			require.True(t, service.IsChannelRPMLimitError(apiErr))
			require.False(t, called, "RPM denial must not invoke the upstream callback")
			require.False(t, info.HasUpstreamDispatch(), "local RPM admission is not evidence against upstream health")
		})
	}
}

func TestChannelRPMGuardedSeamsRetryDoesNotInheritDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, seam := range channelDispatchSeams() {
		t.Run(seam.name, func(t *testing.T) {
			const channelID = 930003
			withMemoryChannelRPM(t, channelID)
			c, info := channelRPMTestContext(), channelRPMTestInfo(channelID, 1)
			calls := 0
			request := func() (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: http.StatusOK}, nil
			}
			info.BeginAttempt()
			_, err := seam.run(c, info, request)
			require.NoError(t, err)
			info.EndAttempt()
			require.True(t, info.HasUpstreamDispatch())

			info.BeginAttempt()
			_, err = seam.run(c, info, request)
			info.EndAttempt()
			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			require.True(t, service.IsChannelRPMLimitError(apiErr))
			require.Equal(t, 1, calls, "the rejected retry must not execute the callback")
			require.False(t, info.HasUpstreamDispatch(), "the previous dispatch must not leak into an RPM-denied attempt")
		})
	}
}
