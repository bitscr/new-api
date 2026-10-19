package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// 手动渠道测试结果并入打分:多分组渠道每个分组各记一条;失败走抖动阶梯;
// 命中确定性关键词的失败直接摘除组合(不再有冷却);冷却中的组合被一次快速
// 成功的测试解冻;auto 关闭或模型名为 auto 时不记。
func TestRecordAutoModelTestOutcome(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() { operation_setting.AutoModelEnabled = true })

	channel := &model.Channel{Id: 7, Group: "g1,g2", Models: "m1"}

	// a) 成功:两个分组都提分
	RecordAutoModelTestOutcome(channel, "m1", true, 800, nil)
	for _, group := range []string{"g1", "g2"} {
		out, ok := autoModelHealth.Get(autoModelHealthKey(group, "m1", 7))
		require.True(t, ok, "分组 %s 应记录成功观测", group)
		require.Greater(t, out.Score, 0.5)
	}
	require.Empty(t, ListAutoModelCooldowns(time.Now()), "快速成功不应产生冷却")

	// b) 失败:抖动阶梯冷却,两个分组一致
	RecordAutoModelTestOutcome(channel, "m1", false, 500, nil)
	for _, group := range []string{"g1", "g2"} {
		require.True(t, GetAutoModelCoolingChannelIDs(group, "m1")[7],
			"分组 %s 的组合应进入冷却", group)
	}
	rows := ListAutoModelCooldowns(time.Now())
	require.Len(t, rows, 2)

	// c) 冷却中的组合被一次快速成功的测试解冻(管理员手动恢复通道)
	RecordAutoModelTestOutcome(channel, "m1", true, 600, nil)
	require.Empty(t, GetAutoModelCoolingChannelIDs("g1", "m1"), "成功测试应清除冷却")
	require.Empty(t, GetAutoModelCoolingChannelIDs("g2", "m1"), "成功测试应清除冷却")
	require.Empty(t, ListAutoModelCooldowns(time.Now()))

	// d) 模型名为 auto 时不记(它是虚拟模型,不是被测对象)
	RecordAutoModelTestOutcome(channel, "auto", false, 100, nil)
	require.Empty(t, ListAutoModelCooldowns(time.Now()))

	// e) auto 关闭时不记
	operation_setting.AutoModelEnabled = false
	RecordAutoModelTestOutcome(channel, "m1", false, 100, nil)
	require.Empty(t, ListAutoModelCooldowns(time.Now()))
	operation_setting.AutoModelEnabled = true

	// f) 失败两次:阶梯升级(第二次窗口 = 2×第一次);每个分组各一条记录
	RecordAutoModelTestOutcome(channel, "m1", false, 500, nil)
	first := ListAutoModelCooldowns(time.Now())
	require.Len(t, first, 2, "两个分组各一条冷却记录")
	firstWindow := first[0].Until - first[0].UpdatedAt
	RecordAutoModelTestOutcome(channel, "m1", false, 500, nil)
	second := ListAutoModelCooldowns(time.Now())
	require.Len(t, second, 2)
	secondWindow := second[0].Until - second[0].UpdatedAt
	require.Greater(t, secondWindow, firstWindow, "重复失败阶梯必须升级")

	// g) 失败命中确定性关键词:直接摘除组合,不再有冷却档。
	//    本测试的内存库只有 auto_model_cooldowns 表,摘除必然失败(没有 channels
	//    表);此时不得退回冷却——宁可让组合保持可选、下次失败再试摘除,也不能
	//    退回"每 15 分钟再撞一次"的抖动档装作无事发生。
	RecordAutoModelTestOutcome(channel, "m1", true, 600, nil)
	require.Empty(t, ListAutoModelCooldowns(time.Now()))
	t.Cleanup(func() { operation_setting.SetAutoModelPermanentKeywords("") })
	operation_setting.SetAutoModelPermanentKeywords("status code 524")
	permErr := types.NewErrorWithStatusCode(
		errors.New("bad response status code 524"),
		types.ErrorCode("bad_response_status_code"), 524)
	RecordAutoModelTestOutcome(channel, "m1", false, 500, permErr)
	require.Empty(t, ListAutoModelCooldowns(time.Now()),
		"确定性失败不再产生冷却;摘除失败也不得退回抖动冷却")

	// h) 普通瞬态错误(非确定性)仍走抖动档:清掉关键词后失败一次。
	operation_setting.SetAutoModelPermanentKeywords("")
	transientErr := types.NewErrorWithStatusCode(
		errors.New("account rate limit exceeded"),
		types.ErrorCode("rate_limit_exceeded"), http.StatusTooManyRequests)
	RecordAutoModelTestOutcome(channel, "m1", false, 500, transientErr)
	tr := ListAutoModelCooldowns(time.Now())
	require.Len(t, tr, 2, "两个分组各一条抖动冷却")
	for _, row := range tr {
		require.Equal(t, 1, row.Level, "清状态后第一档从起步时长开始")
	}
}
