package service

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 同一个渠道在一次请求里试满上限后就不再参与选择：
// 排队型上游重试同一个渠道没有任何收益（实测有单请求重试 51 次、拖满两分钟的情况）。
func TestChannelSelectionExcludesOverAttemptedChannels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	c.Set("use_channel", []string{"1", "1", "10"})
	excluded := GetChannelSelectionExcludedIDs(c)
	require.True(t, excluded[1], "试满 %d 次的渠道应被排除", maxChannelAttemptsPerRequest)
	require.False(t, excluded[10], "只试过一次的渠道仍应参与选择")

	// 试满上限后仍然可以换到别的渠道：新渠道不在排除集合里
	require.False(t, excluded[12])

	// 没有历史尝试时不做任何排除
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	require.Empty(t, GetChannelSelectionExcludedIDs(c2))
}

func TestCountChannelAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("use_channel", []string{"1", "1", "2", "12", "bad"})
	require.Equal(t, map[int]int{1: 2, 2: 1, 12: 1}, countChannelAttempts(c))
	require.Empty(t, countChannelAttempts(nil))
}
