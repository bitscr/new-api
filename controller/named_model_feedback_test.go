package controller

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"github.com/stretchr/testify/require"
)

// newNamedModelFeedbackTestContext 构造"具名请求"上下文:不设置
// ContextKeyAutoModelClientName(auto 专用标记),模拟客户端直接请求固定模型。
func newNamedModelFeedbackTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	return c
}

// 具名请求的成败也并入 auto 健康体系:失败按与真实流量相同的判据分档
// (命中管理员确定性关键词走摘除,其余按抖动),成功提分;但不改变任何路由行为
// (这里只测记账,路由行为由既有测试覆盖)。
func TestNamedModelFeedbackEntersHealthSystem(t *testing.T) {
	original := operation_setting.AutoModelPermanentKeywordsToString()
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords(original) })
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = true })

	start := time.Now()
	// 具名请求:没有 ContextKeyAutoModelClientName。
	info := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "glm-5.3-flash",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 32},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(21 * time.Second),
	}
	info.MarkUpstreamDispatch()

	// a) 确定性失败(502 命中管理员关键词):按摘除档记录,与 auto 请求同判据
	operation_setting.SetAutoModelPermanentKeywords("bad response status code 502")
	namedErr := types.NewErrorWithStatusCode(
		errors.New("bad response status code 502"),
		types.ErrorCode("bad_response_status_code"), http.StatusBadGateway)
	c1 := newNamedModelFeedbackTestContext()
	recordAutoModelUpstreamFailure(c1, info, namedErr)
	pending := takeAutoModelFeedback(c1)
	require.Len(t, pending, 1, "具名请求的确定性失败必须入队")
	for key, outcome := range pending {
		require.Equal(t, "default", key.group)
		require.Equal(t, "glm-5.3-flash", key.modelName)
		require.Equal(t, 32, key.channelID)
		require.True(t, outcome.permanent, "502 命中管理员关键词,是确定性失败")
	}

	// b) 普通瞬态失败(限流):抖动档
	c2 := newNamedModelFeedbackTestContext()
	info2 := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "glm-5.3-flash",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 32},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(time.Second),
	}
	info2.MarkUpstreamDispatch()
	transientErr := types.NewErrorWithStatusCode(
		errors.New("account rate limit exceeded"),
		types.ErrorCode("rate_limit_exceeded"), http.StatusTooManyRequests)
	recordAutoModelUpstreamFailure(c2, info2, transientErr)
	pending2 := takeAutoModelFeedback(c2)
	for _, outcome := range pending2 {
		require.False(t, outcome.permanent, "限流是抖动失败")
	}

	// c) 成功反馈:正常入队
	c3 := newNamedModelFeedbackTestContext()
	info3 := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "glm-5.3-flash",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 32},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(time.Second),
	}
	info3.MarkUpstreamDispatch()
	recordAutoModelFeedback(c3, info3, true)
	pending3 := takeAutoModelFeedback(c3)
	require.Len(t, pending3, 1)
	for _, outcome := range pending3 {
		require.True(t, outcome.success)
	}

	// d) 没有真正派发上游(本地拒绝)不记账
	c4 := newNamedModelFeedbackTestContext()
	info4 := &relaycommon.RelayInfo{
		UsingGroup:      "default",
		OriginModelName: "glm-5.3-flash",
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: 32},
	}
	recordAutoModelUpstreamFailure(c4, info4, namedErr)
	require.Empty(t, takeAutoModelFeedback(c4), "未进入上游派发的失败不是证据")

	// e) AutoModelEnabled 关闭时不记账
	operation_setting.AutoModelEnabled = false
	c5 := newNamedModelFeedbackTestContext()
	info5 := &relaycommon.RelayInfo{
		UsingGroup:       "default",
		OriginModelName:  "glm-5.3-flash",
		ChannelMeta:      &relaycommon.ChannelMeta{ChannelId: 32},
		AttemptStartTime: start,
		AttemptEndTime:   start.Add(time.Second),
	}
	info5.MarkUpstreamDispatch()
	recordAutoModelUpstreamFailure(c5, info5, transientErr)
	require.Empty(t, takeAutoModelFeedback(c5), "auto 关闭时具名请求不并入健康体系")
}

// resetAutoModelCooldownState 清空 auto 冷却(内存 + 表),避免跨用例串味:
// 具名请求的失败现在会建立冷却(全局内存态),同一包内后续用例若不复位,
// 就会被上一个用例的冷却挡住。
func resetAutoModelCooldownState(db *gorm.DB) {
	if db != nil {
		_ = db.Exec("DELETE FROM auto_model_cooldowns").Error
	}
	service.LoadAutoModelCooldowns()
}
