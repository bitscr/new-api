package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGetAutoModelHardExcludedChannelIDs(t *testing.T) {
	require.Empty(t, GetAutoModelHardExcludedChannelIDs(nil))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(channelDailySuccessLimitSkippedIDsKey, map[int]bool{11: true, 13: true, 14: false})
	c.Set(channelRPMLimitSkippedIDsKey, map[int]bool{12: true, 13: true, 15: false})
	c.Set("use_channel", []string{"21", "21", "22", "invalid"})

	want := map[int]bool{11: true, 12: true, 13: true}
	hard := GetAutoModelHardExcludedChannelIDs(c)
	require.Equal(t, want, hard, "only channel-wide daily-success and RPM limits belong in auto hard exclusions")
	require.False(t, hard[21], "two attempts must not block another model on the same channel")
	legacy := GetChannelSelectionExcludedIDs(c)
	require.Equal(t, map[int]bool{11: true, 12: true, 13: true, 21: true}, legacy,
		"non-auto and legacy selectors retain the existing channel-wide max2 rule")

	delete(hard, 11)
	hard[99] = true
	require.Equal(t, want, GetAutoModelHardExcludedChannelIDs(c), "the caller owns the returned map")
	require.True(t, IsChannelDailySuccessLimitSkipped(c, 11))
	require.False(t, IsChannelRPMLimitSkipped(c, 99))
	require.False(t, GetChannelSelectionExcludedIDs(c)[99])
}

func TestGetAutoModelHardExcludedChannelIDsIgnoresMalformedContextValues(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(channelDailySuccessLimitSkippedIDsKey, "invalid")
	c.Set(channelRPMLimitSkippedIDsKey, []int{1})
	c.Set("use_channel", []string{"1", "1"})
	require.Empty(t, GetAutoModelHardExcludedChannelIDs(c))
	require.True(t, GetChannelSelectionExcludedIDs(c)[1])
}
