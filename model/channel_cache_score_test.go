package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func weightPtr(v uint) *uint { return &v }

// TestChooseChannelByScore 锁定 auto 挑渠道的语义：
// 分数最高的一档优先；档内（分数差在容差以内）按渠道权重加权随机；
// 落后一档的渠道不参与；score 为 nil 时退化为原来的纯权重随机。
func TestChooseChannelByScore(t *testing.T) {
	ch1 := &Channel{Id: 1, Weight: weightPtr(10)}
	ch2 := &Channel{Id: 2, Weight: weightPtr(10)}
	ch3 := &Channel{Id: 3, Weight: weightPtr(10)}
	targets := []*Channel{ch1, ch2, ch3}

	scores := map[int]float64{1: 0.9, 2: 0.6, 3: 0.5}
	pick := func(id int) float64 { return scores[id] }

	// 已知 0.9 / 0.6 / 未观测(0.5)：只有最高档该被选中
	for i := 0; i < 20; i++ {
		chosen, err := ChooseChannelByScore(targets, pick, 0.02)
		require.NoError(t, err)
		require.Equal(t, 1, chosen.Id, "只有分数最高的渠道该被选中")
	}

	// 并列（差 0.01，在容差内）：档内加权随机，两个都该出现，绝不该选落后的 3
	scores = map[int]float64{1: 0.90, 2: 0.89, 3: 0.50}
	seen := map[int]bool{}
	for i := 0; i < 120; i++ {
		chosen, err := ChooseChannelByScore(targets, pick, 0.02)
		require.NoError(t, err)
		require.NotEqual(t, 3, chosen.Id, "落后一档的渠道不该被选中")
		seen[chosen.Id] = true
	}
	require.Len(t, seen, 2, "并列档内的渠道都该有机会")

	// 权重生效：并列时权重高的拿更多流量
	scores = map[int]float64{1: 0.9, 2: 0.9, 3: 0.9}
	heavy := &Channel{Id: 4, Weight: weightPtr(90)}
	light := &Channel{Id: 5, Weight: weightPtr(10)}
	counts := map[int]int{}
	for i := 0; i < 400; i++ {
		chosen, err := ChooseChannelByScore([]*Channel{heavy, light}, pick2, 0.02)
		require.NoError(t, err)
		counts[chosen.Id]++
	}
	require.Greater(t, counts[4], counts[5], "并列时权重高的渠道该拿到更多流量")

	// score 为 nil：退化为纯权重随机，三个都可能出现
	seen = map[int]bool{}
	for i := 0; i < 120; i++ {
		chosen, err := ChooseChannelByScore(targets, nil, 0.02)
		require.NoError(t, err)
		seen[chosen.Id] = true
	}
	require.Len(t, seen, 3)

	chosen, err := ChooseChannelByScore(nil, pick, 0.02)
	require.NoError(t, err)
	require.Nil(t, chosen)
}

// pick2 是上面权重占比用例用的打分函数：两个渠道分数完全相同。
func pick2(int) float64 { return 0.9 }
