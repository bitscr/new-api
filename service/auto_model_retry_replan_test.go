package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// 快照里的 ExcludedRoutes 只丢掉那一个三元组:同渠道的兄弟模型、同模型的其他渠道
// 都不受影响。这是"本次请求已经打过这条路"的正确粒度。
func TestAutoModelRouteSnapshotDropsOnlyExcludedTriples(t *testing.T) {
	snapshot := autoModelRouteSnapshot{
		Now: time.Unix(1700000000, 0),
		Targets: map[string][]model.AutoModelRoutingTarget{
			"a": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 1}},
			"b": {{ChannelID: 1, Weight: 1}},
		},
		ExcludedRoutes: map[AutoModelRoute]bool{
			{Group: "g", ModelName: "a", ChannelID: 1}: true,
		},
	}
	require.Equal(t, []AutoModelRoute{
		{Group: "g", ModelName: "b", ChannelID: 1},
		{Group: "g", ModelName: "a", ChannelID: 2},
	}, buildAutoModelRoutesFromSnapshot("g", []string{"a", "b"}, snapshot, func() float64 { return 0 }),
		"an excluded triple must not take its sibling model or its other channels with it")

	// 排除是"组 + 模型 + 渠道"三元组:换一个分组就不该命中。
	other := buildAutoModelRoutesFromSnapshot("other", []string{"a", "b"}, snapshot, func() float64 { return 0 })
	require.Len(t, other, 3)
}

// 重排的入口守卫:没有已试路由、或拿不到请求分组时返回 nil,让调用方退回原计划。
//
// 空 attempted 返回 nil 不只是省一次查询:重排后调用方要从队首重走,而"渠道在
// 派发前就不可用"这条路径不会标记 attempted;空集也重排会让那个循环失去
// "索引单调递增"这个原有的终止性上界。
func TestRebuildAutoModelRoutePlanForRetryFallsBackWithoutAttemptsOrGroup(t *testing.T) {
	routes, status := RebuildAutoModelRoutePlanForRetry(nil, "g", map[AutoModelRoute]bool{
		{Group: "g", ModelName: "a", ChannelID: 1}: true,
	})
	require.Nil(t, routes)
	require.Equal(t, AutoModelRebuildSkipped, status, "没有 gin 上下文时不能重排")
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	routes, status = RebuildAutoModelRoutePlanForRetry(c, "g", nil)
	require.Nil(t, routes)
	require.Equal(t, AutoModelRebuildSkipped, status, "空 attempted 必须退回原计划")
	routes, status = RebuildAutoModelRoutePlanForRetry(c, "", map[AutoModelRoute]bool{
		{Group: "g", ModelName: "a", ChannelID: 1}: true,
	})
	require.Nil(t, routes)
	require.Equal(t, AutoModelRebuildEmpty, status, "没有请求分组时没有候选可重排")
}

// 重排必须用当前模型分重新排序,并丢掉已经打过的路:冻结计划里"还没试过的最优路"
// 可能排在预算之外,重排把它提到队首。模型分跨渠道比较,渠道健康分不再参与。
func TestRebuildAutoModelRoutePlanForRetryReranksAndDropsAttempted(t *testing.T) {
	db := setupAutoModelRouteRequestTest(t)
	seedAutoModelRouteRequestChannels(t, db)

	// 两条渠道 priority 相同;route-b@2 被证明最好,其余路线全中性 0.5。
	now := time.Now()
	autoModelHealth.Set(autoModelHealthKey("g", "route-b", 2),
		autoModelOutcome{Score: 0.9, Observations: 20, UpdatedAt: now})

	attempted := map[AutoModelRoute]bool{
		{Group: "g", ModelName: "route-a", ChannelID: 1}: true,
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	routes, status := RebuildAutoModelRoutePlanForRetry(c, "g", attempted)
	require.Equal(t, AutoModelRebuildReplaced, status)
	require.NotEmpty(t, routes)
	for _, route := range routes {
		require.NotEqual(t, AutoModelRoute{Group: "g", ModelName: "route-a", ChannelID: 1}, route,
			"已经打过的路不能回到重排后的计划里")
	}
	require.Equal(t, AutoModelRoute{Group: "g", ModelName: "route-b", ChannelID: 2}, routes[0],
		"重排后队首是当前模型分最高的路线,同优先级内跨渠道比较")
	require.Len(t, routes, 3, "两条渠道共 4 条路,减去已试过的 1 条")

	// 对照:同一分组没有任何模型观测时,四条路线全部中性同档,队首由两级加权
	// 随机决定(渠道权重相同,固定随机源下按稳定枚举顺序可复现:draw 0 先取
	// 渠道 1,再取其中按名排序的第一个模型 route-a)。
	fresh := autoModelRouteSnapshot{
		Now:     now,
		Targets: map[string][]model.AutoModelRoutingTarget{
			"route-a": {{ChannelID: 1, Weight: 10}, {ChannelID: 2, Weight: 10}},
			"route-b": {{ChannelID: 1, Weight: 10}, {ChannelID: 2, Weight: 10}},
		},
	}
	require.Equal(t, AutoModelRoute{Group: "g", ModelName: "route-a", ChannelID: 1},
		buildAutoModelRoutesFromSnapshot("g", []string{"route-a", "route-b"}, fresh,
			func() float64 { return 0 })[0])
}

// 换计划不能顺手换分组:只有真正 bind 成功的那次提交才允许把请求挪到别的分组。
func TestReplaceAutoModelRoutePlanKeepsGroupBookkeeping(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTokenGroup, "auto")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "auto")
	SetAutoModelRoutePlan(c, []AutoModelRoute{{Group: "g", ModelName: "a", ChannelID: 1}})
	require.Equal(t, 0, GetAutoModelRouteIndex(c))
	require.Equal(t, "g", common.GetContextKeyString(c, constant.ContextKeyUsingGroup))

	ReplaceAutoModelRoutePlan(c, []AutoModelRoute{
		{Group: "vip", ModelName: "b", ChannelID: 2},
		{Group: "vip", ModelName: "c", ChannelID: 3},
	})
	require.Equal(t, -1, GetAutoModelRouteIndex(c),
		"换过的计划在 bind 提交之前没有活跃路由")
	_, ok := GetCurrentAutoModelRoute(c)
	require.False(t, ok)
	require.Equal(t, "g", common.GetContextKeyString(c, constant.ContextKeyUsingGroup),
		"重排不得在 bind 成功之前把请求挪到另一个分组")
	require.Equal(t, []string{"b", "c"},
		common.GetContextKeyStringSlice(c, constant.ContextKeyAutoModelCandidates))
	require.Len(t, GetAutoModelRoutePlan(c), 2)

	// 入参是拷贝:调用方改自己的切片不能改动已经生效的计划。
	input := []AutoModelRoute{{Group: "g", ModelName: "d", ChannelID: 4}}
	ReplaceAutoModelRoutePlan(c, input)
	input[0].ChannelID = 900
	require.Equal(t, 4, GetAutoModelRoutePlan(c)[0].ChannelID)
}
