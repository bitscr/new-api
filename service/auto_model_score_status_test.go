package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestStatusPageShowsEffectiveLatencySamples(t *testing.T) {
	oldDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = db
	oldHealth := autoModelHealth.ReadAll()
	autoModelHealth.Clear()
	oldAuto := operation_setting.AutoModelEnabled
	operation_setting.AutoModelEnabled = true
	operation_setting.SetAutoModelMinObservations(5)
	t.Cleanup(func() {
		model.DB = oldDB
		autoModelHealth.Clear()
		autoModelHealth.AddAll(oldHealth)
		operation_setting.AutoModelEnabled = oldAuto
		operation_setting.SetAutoModelMinObservations(5)
		model.InvalidateAutoModelRoutingTargetsCache()
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})
	model.InvalidateAutoModelRoutingTargetsCache()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"status-page-model":1}`))
	channel := &model.Channel{
		Type: 1, Key: "k", Status: common.ChannelStatusEnabled,
		Name: "status-page-channel", Group: "status-page", Models: "status-page-model",
	}
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group: "status-page", Model: "status-page-model", ChannelId: channel.Id, Enabled: true,
	}).Error)
	model.InvalidateAutoModelRoutingTargetsCache()

	now := time.Now()
	key := autoModelHealthKey("status-page", "status-page-model", channel.Id)
	autoModelHealth.Set(key, autoModelOutcome{
		SuccessCount: 20, LastObservedAt: now,
		StreamLatencyMS: 500, StreamLatencySamples: 10, StreamLatencyUpdatedAt: now.Add(-time.Hour),
	})

	rows, err := listAutoModelRouteStatus(now, true)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	require.InDelta(t, 10*(1-60.0/float64(operation_setting.AutoModelScoreDecayMinutes())), row.StreamLatencySamples, 0.001,
		"the page must show the decayed sample count used by routing")
	require.True(t, row.StreamLatencyKnown)
	require.True(t, row.StreamLatencyFresh)
	require.InDelta(t, 500, row.StreamLatencyMS, 1e-9)
	require.Equal(t, 0.0, row.NonStreamLatencySamples)
	require.Equal(t, "main", row.Category)
	require.True(t, row.ReliabilityKnown)
}

func TestStatusPageReadFailureIsExplicit(t *testing.T) {
	oldDB := model.DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	model.DB = db
	t.Cleanup(func() {
		model.DB = oldDB
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})
	_, err = ListAutoModelScores(time.Now(), false)
	require.Error(t, err, "a broken target query must not look like an empty table")
}
