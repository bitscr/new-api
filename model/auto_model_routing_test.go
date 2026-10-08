package model

import (
	"errors"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func setupAutoModelRoutingSnapshotTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	oldDB, oldEnabled := DB, common.MemoryCacheEnabled
	DB = db
	t.Cleanup(func() {
		DB, common.MemoryCacheEnabled = oldDB, oldEnabled
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
	return db
}

func TestAutoModelRoutingTargetsSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		memory    bool
		smoothing float64
	}{
		{name: "memory", memory: true},
		{name: "database", smoothing: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupAutoModelRoutingSnapshotTestDB(t)
			common.MemoryCacheEnabled = tc.memory
			priority, weight := int64(7), uint(3)
			abilityPriority, otherAbilityPriority := int64(999), int64(-999)
			channels := []Channel{
				{Id: 1, Key: "enabled", Status: common.ChannelStatusEnabled, Group: "g", Models: "m,other,disabled-ability,metadata-only", Priority: &priority, Weight: &weight},
				{Id: 2, Key: "disabled-channel", Status: common.ChannelStatusManuallyDisabled, Group: "g", Models: "m"},
				{Id: 3, Key: "disabled-ability", Status: common.ChannelStatusEnabled, Group: "g", Models: "m"},
				{Id: 4, Key: "null-metadata", Status: common.ChannelStatusEnabled, Group: "g", Models: "defaults"},
				{Id: 5, Key: "other-group", Status: common.ChannelStatusEnabled, Group: "vip", Models: "m"},
				{Id: 6, Key: "deleted", Status: common.ChannelStatusEnabled, Group: "g", Models: "m"},
			}
			require.NoError(t, db.Create(&channels).Error)
			require.NoError(t, db.Create(&[]Ability{
				{Group: "g", Model: "m", ChannelId: 1, Enabled: true, Priority: &abilityPriority, Weight: 900},
				{Group: "g", Model: "other", ChannelId: 1, Enabled: true, Priority: &otherAbilityPriority, Weight: 1},
				{Group: "g", Model: "disabled-ability", ChannelId: 1, Enabled: false},
				{Group: "g", Model: "m", ChannelId: 2, Enabled: true},
				{Group: "g", Model: "m", ChannelId: 3, Enabled: false},
				{Group: "g", Model: "defaults", ChannelId: 4, Enabled: true, Priority: &abilityPriority, Weight: 800},
				{Group: "vip", Model: "m", ChannelId: 5, Enabled: true},
				{Group: "g", Model: "m", ChannelId: 6, Enabled: true},
			}).Error)
			require.NoError(t, db.Delete(&Channel{}, 6).Error)
			require.NoError(t, db.Model(&Channel{}).Where("id = ?", 4).
				Updates(map[string]any{"priority": nil, "weight": nil}).Error)

			// Deliberately stale cache metadata must not revive disabled abilities,
			// deleted channels or routes advertised only by Channel.Models.
			channelSyncLock.Lock()
			oldGroups, oldChannels := group2model2channels, channelsIDM
			group2model2channels = map[string]map[string][]int{
				"g": {"m": {1, 2, 3, 6}, "other": {1}, "disabled-ability": {1}, "metadata-only": {1}},
			}
			channelsIDM = make(map[int]*Channel, len(channels))
			for i := range channels {
				channelsIDM[channels[i].Id] = &channels[i]
			}
			channelSyncLock.Unlock()
			t.Cleanup(func() {
				channelSyncLock.Lock()
				group2model2channels, channelsIDM = oldGroups, oldChannels
				channelSyncLock.Unlock()
			})

			queries := 0
			const callbackName = "test:auto_model_routing_query_count"
			require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				queries++
			}))
			t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callbackName)) })
			want := map[string][]AutoModelRoutingTarget{
				"m":        {{ChannelID: 1, Priority: 7, Weight: 3 + tc.smoothing}},
				"other":    {{ChannelID: 1, Priority: 7, Weight: 3 + tc.smoothing}},
				"defaults": {{ChannelID: 4, Weight: tc.smoothing}},
			}
			targets, err := GetAutoModelRoutingTargets("g")
			require.NoError(t, err)
			require.Equal(t, want, targets, "ability metadata must never override channel metadata")
			require.Equal(t, 1, queries, "a snapshot must use one batch join, not N+1 channel lookups")
			targets["m"][0].Weight = 100
			delete(targets, "other")
			next, err := GetAutoModelRoutingTargets("g")
			require.NoError(t, err)
			require.Equal(t, want, next, "mutating a returned snapshot must not modify channel state")
			require.Equal(t, 2, queries)
			missing, err := GetAutoModelRoutingTargets("missing")
			require.NoError(t, err)
			require.Empty(t, missing)
			require.Equal(t, 3, queries)
		})
	}
}

func TestAutoModelRoutingTargetsDatabaseFailure(t *testing.T) {
	for _, memory := range []bool{true, false} {
		t.Run(map[bool]string{true: "memory", false: "database"}[memory], func(t *testing.T) {
			db := setupAutoModelRoutingSnapshotTestDB(t)
			common.MemoryCacheEnabled = memory
			DB = nil
			targets, err := GetAutoModelRoutingTargets("g")
			require.Error(t, err)
			require.Nil(t, targets)
			DB = db
			failure := errors.New("routing query failed")
			const callbackName = "test:auto_model_routing_query_failure"
			require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				tx.AddError(failure)
			}))
			t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callbackName)) })
			targets, err = GetAutoModelRoutingTargets("g")
			require.ErrorIs(t, err, failure)
			require.Nil(t, targets, "failed eligibility queries must not fall back to an unchecked cache")
		})
	}
}

func TestAutoModelRoutingTargetsDatabaseDialects(t *testing.T) {
	base := setupAutoModelRoutingSnapshotTestDB(t)
	sqlDB, err := base.DB()
	require.NoError(t, err)
	for _, tc := range []struct {
		name       string
		dialect    gorm.Dialector
		groupQuote string
	}{
		{name: "sqlite", groupQuote: "`abilities`.`group`"},
		{name: "mysql", dialect: mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}), groupQuote: "`abilities`.`group`"},
		{name: "postgres", dialect: postgres.New(postgres.Config{Conn: sqlDB}), groupQuote: `"abilities"."group"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := base.Session(&gorm.Session{DryRun: true})
			if tc.dialect != nil {
				var err error
				db, err = gorm.Open(tc.dialect, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
				require.NoError(t, err)
			}
			oldDB, oldEnabled := DB, common.MemoryCacheEnabled
			DB = db
			t.Cleanup(func() { DB, common.MemoryCacheEnabled = oldDB, oldEnabled })
			var statements []string
			var variables [][]any
			const callbackName = "test:auto_model_routing_dialect"
			require.NoError(t, db.Callback().Query().After("gorm:query").Register(callbackName, func(tx *gorm.DB) {
				statements = append(statements, tx.Statement.SQL.String())
				variables = append(variables, append([]any(nil), tx.Statement.Vars...))
			}))
			t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callbackName)) })
			const group = "g' OR 1=1 --"
			for _, memory := range []bool{true, false} {
				common.MemoryCacheEnabled = memory
				_, err := GetAutoModelRoutingTargets(group)
				require.NoError(t, err)
			}
			require.Len(t, statements, 2, "both modes issue exactly one portable query per snapshot")
			for i, statement := range statements {
				require.Contains(t, statement, "INNER JOIN channels ON channels.id = abilities.channel_id")
				require.Contains(t, statement, tc.groupQuote)
				require.Contains(t, statement, "channels.priority, channels.weight")
				require.Contains(t, statement, "abilities.enabled = ")
				require.Contains(t, statement, "channels.status = ")
				require.NotContains(t, statement, "abilities.priority")
				require.NotContains(t, statement, "abilities.weight")
				require.NotContains(t, statement, group)
				require.Equal(t, []any{group, true, common.ChannelStatusEnabled}, variables[i])
			}
		})
	}
}
