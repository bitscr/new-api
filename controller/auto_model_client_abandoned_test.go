package controller

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// autoModelClientAbandoned 用来判断"客户端在拿到本次交付物之前就离开"。
// 关键回归（实测）：只看 ctx.Err() 会把"客户端收完正文后正常关闭连接"也判成离开，
// 于是每个正常完成的请求的反馈都被丢掉，分数不再更新、慢成功的冷却也不触发。
func TestAutoModelClientAbandonedOnlyOnInterruptedDelivery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
		return c
	}

	info := &relaycommon.RelayInfo{}

	c := newContext()
	require.False(t, autoModelClientAbandoned(c, info), "客户端还在等着，不算离开")

	request := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	c.Request = request.WithContext(ctx)
	require.True(t, autoModelClientAbandoned(c, info),
		"ctx 取消且一个字节都没写出去：客户端在等到任何内容前就走了")

	c.Writer.WriteHeader(200)
	_, err := c.Writer.Write([]byte(`{"choices":[]}`))
	require.NoError(t, err)
	require.False(t, autoModelClientAbandoned(c, info),
		"ctx 取消但正文已经写完：客户端是收完才关的，必须照常记分")
}
