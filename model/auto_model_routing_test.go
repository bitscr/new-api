package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

func TestAutoModelRoutingTargetsMemorySnapshot(t *testing.T) {
	oldEnabled := common.MemoryCacheEnabled
	channelSyncLock.Lock()
	oldGroups, oldChannels := group2model2channels, channelsIDM
	priority := int64(7)
	weight := uint(3)
	group2model2channels = map[string]map[string][]int{"g": {"m": {1, 2, 99}}}
	channelsIDM = map[int]*Channel{
		1: {Id: 1, Status: common.ChannelStatusEnabled, Priority: &priority, Weight: &weight},
		2: {Id: 2, Status: common.ChannelStatusManuallyDisabled},
	}
	channelSyncLock.Unlock()
	common.MemoryCacheEnabled = true
	t.Cleanup(func() {
		common.MemoryCacheEnabled = oldEnabled
		channelSyncLock.Lock()
		group2model2channels, channelsIDM = oldGroups, oldChannels
		channelSyncLock.Unlock()
	})
	targets, err := GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Equal(t, []AutoModelRoutingTarget{{ChannelID: 1, Priority: 7, Weight: 3}}, targets["m"])
	targets["m"][0].Weight = 100
	next, err := GetAutoModelRoutingTargets("g")
	require.NoError(t, err)
	require.Equal(t, 3.0, next["m"][0].Weight)
}
