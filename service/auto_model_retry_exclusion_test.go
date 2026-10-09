package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMarkAutoModelChannelFailedPreservesOtherExclusions(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	MarkAutoModelChannelFailed(nil, 1)
	MarkAutoModelChannelFailed(c, 0)
	MarkAutoModelChannelFailed(c, -1)
	require.Empty(t, GetAutoModelHardExcludedChannelIDs(c))
	MarkChannelDailySuccessLimitSkipped(c, 2)
	MarkChannelRPMLimitSkipped(c, 3)
	MarkAutoModelChannelFailed(c, 4)
	MarkAutoModelChannelFailed(c, 5)
	MarkAutoModelChannelFailed(c, 4)
	require.Equal(t, map[int]bool{2: true, 3: true, 4: true, 5: true}, GetAutoModelHardExcludedChannelIDs(c))
	require.False(t, IsChannelRPMLimitSkipped(c, 4))
	require.False(t, IsChannelDailySuccessLimitSkipped(c, 4))
}

func TestAutoModelUpstreamFailureExclusionIsRequestLocal(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	// A retry decision stores its own reason, not a fabricated local RPM limit.
	c.Set("auto_model_failed_channel_ids", map[int]bool{41: true, 42: false, -1: true})
	MarkChannelDailySuccessLimitSkipped(c, 45)
	MarkChannelRPMLimitSkipped(c, 46)
	hard := GetAutoModelHardExcludedChannelIDs(c)
	require.True(t, hard[41], "an upstream channel failure must exclude its remaining models in this request")
	require.True(t, hard[45])
	require.True(t, hard[46])
	require.False(t, hard[42])
	require.False(t, hard[-1])
	require.False(t, IsChannelRPMLimitSkipped(c, 41), "upstream failure is not a local RPM rejection")
	require.False(t, IsChannelDailySuccessLimitSkipped(c, 41))
	delete(hard, 41)
	require.True(t, GetAutoModelHardExcludedChannelIDs(c)[41], "callers must not mutate stored exclusions")
	fresh, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.Empty(t, GetAutoModelHardExcludedChannelIDs(fresh), "channel exclusion must not leak into another request")
}
