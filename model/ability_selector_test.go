package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetChannelWithoutColumnInitialization(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retry    int
		excluded map[int]bool
		wantID   int
	}{
		{name: "first_attempt", wantID: 1},
		{name: "retry_next_tier", retry: 1, wantID: 2},
		{name: "retry_last_tier", retry: 2, wantID: 3},
		{name: "retry_past_last_tier", retry: 10, wantID: 3},
		{name: "empty_exclusions", excluded: map[int]bool{}, wantID: 1},
		{name: "exclude_highest_tier", excluded: map[int]bool{1: true}, wantID: 2},
		{name: "retry_after_exclusion", retry: 1, excluded: map[int]bool{1: true}, wantID: 3},
		{name: "retry_past_remaining_tiers", retry: 10, excluded: map[int]bool{1: true}, wantID: 3},
		{name: "exclude_middle_tier", retry: 1, excluded: map[int]bool{2: true}, wantID: 3},
		{name: "false_exclusion_keeps_tier", excluded: map[int]bool{1: false}, wantID: 1},
		{name: "all_channels_excluded", excluded: map[int]bool{1: true, 2: true, 3: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)

			oldDB, oldGroupCol := DB, commonGroupCol
			// TestMain calls initCol; reproduce a caller that only supplies a DB.
			DB, commonGroupCol = db, ""
			t.Cleanup(func() {
				DB, commonGroupCol = oldDB, oldGroupCol
				require.NoError(t, sqlDB.Close())
			})
			require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))

			high, middle, low, outside := int64(100), int64(50), int64(0), int64(1000)
			channels := []Channel{
				{Id: 1, Key: "high", Status: common.ChannelStatusEnabled, Group: "g", Models: "m", Priority: &high},
				{Id: 2, Key: "middle", Status: common.ChannelStatusEnabled, Group: "g", Models: "m", Priority: &middle},
				{Id: 3, Key: "low", Status: common.ChannelStatusEnabled, Group: "g", Models: "m", Priority: &low},
				{Id: 4, Key: "other-group", Status: common.ChannelStatusEnabled, Group: "other", Models: "m", Priority: &outside},
				{Id: 5, Key: "other-model", Status: common.ChannelStatusEnabled, Group: "g", Models: "other", Priority: &outside},
				{Id: 6, Key: "disabled", Status: common.ChannelStatusManuallyDisabled, Group: "g", Models: "m", Priority: &outside},
			}
			require.NoError(t, db.Create(&channels).Error)
			require.NoError(t, db.Create(&[]Ability{
				{Group: "g", Model: "m", ChannelId: 1, Enabled: true, Priority: &high},
				{Group: "g", Model: "m", ChannelId: 2, Enabled: true, Priority: &middle, Weight: 100},
				{Group: "g", Model: "m", ChannelId: 3, Enabled: true, Priority: &low, Weight: 1000},
				{Group: "other", Model: "m", ChannelId: 4, Enabled: true, Priority: &outside},
				{Group: "g", Model: "other", ChannelId: 5, Enabled: true, Priority: &outside},
				{Group: "g", Model: "m", ChannelId: 6, Enabled: false, Priority: &outside},
			}).Error)

			var selected *Channel
			if tc.excluded == nil {
				selected, err = GetChannel("g", "m", tc.retry)
			} else {
				selected, err = GetChannelWithExclusions("g", "m", tc.retry, tc.excluded)
			}
			require.NoError(t, err)
			require.Empty(t, commonGroupCol, "selection must not initialize global column names")
			if tc.wantID == 0 {
				require.Nil(t, selected)
				return
			}
			require.NotNil(t, selected)
			require.Equal(t, tc.wantID, selected.Id)
		})
	}
}
