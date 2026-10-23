package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAutoModelOptionPairTest(t *testing.T) *gorm.DB {
	t.Helper()
	oldDB := DB
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Option{}))
	DB = db
	oldMap := common.OptionMap
	common.OptionMapRWMutex.Lock()
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()
	previous := operation_setting.AutoModelRoutingSettingsSnapshot()
	t.Cleanup(func() {
		DB = oldDB
		common.OptionMapRWMutex.Lock()
		common.OptionMap = oldMap
		common.OptionMapRWMutex.Unlock()
		_ = operation_setting.SetAutoModelExploreRatios(previous.ExploreRatio, previous.ExploreMaxRatio)
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestUpdateAutoModelExploreRatiosIsAtomic(t *testing.T) {
	db := setupAutoModelOptionPairTest(t)
	require.NoError(t, UpdateAutoModelExploreRatios("0.02", "0.03"))
	snapshot := operation_setting.AutoModelRoutingSettingsSnapshot()
	require.InDelta(t, 0.02, snapshot.ExploreRatio, 1e-9)
	require.InDelta(t, 0.03, snapshot.ExploreMaxRatio, 1e-9)

	var rows []Option
	require.NoError(t, db.Order("key").Find(&rows).Error)
	require.Len(t, rows, 2)
	require.Equal(t, "0.02", rows[1].Value)
	require.Equal(t, "0.03", rows[0].Value)

	require.Error(t, UpdateAutoModelExploreRatios("0.04", "0.01"))
	snapshot = operation_setting.AutoModelRoutingSettingsSnapshot()
	require.InDelta(t, 0.02, snapshot.ExploreRatio, 1e-9, "an invalid pair must not change runtime")
	require.InDelta(t, 0.03, snapshot.ExploreMaxRatio, 1e-9)
	rows = nil
	require.NoError(t, db.Order("key").Find(&rows).Error)
	require.Equal(t, "0.02", rows[1].Value, "an invalid pair must not change the database")
}

func TestStaleOptionSyncCannotOverwritePairedSave(t *testing.T) {
	db := setupAutoModelOptionPairTest(t)
	require.NoError(t, UpdateAutoModelExploreRatios("0.02", "0.03"))
	// Simulate a periodic sync that read these older DB values before the save
	// completed. Its snapshot must be rejected by the version check.
	stale := []*Option{
		{Key: "AutoModelExploreRatio", Value: "0.08"},
		{Key: "AutoModelExploreMaxRatio", Value: "0.09"},
	}
	handled := loadAutoModelExploreRatiosFromDatabase(stale, 0)
	require.True(t, handled["AutoModelExploreRatio"])
	require.True(t, handled["AutoModelExploreMaxRatio"])
	snapshot := operation_setting.AutoModelRoutingSettingsSnapshot()
	require.InDelta(t, 0.02, snapshot.ExploreRatio, 1e-9)
	require.InDelta(t, 0.03, snapshot.ExploreMaxRatio, 1e-9)

	var rows []Option
	require.NoError(t, db.Order("key").Find(&rows).Error)
	require.Equal(t, "0.02", rows[1].Value)
	require.Equal(t, "0.03", rows[0].Value)
}

func TestLoadExploreRatiosCollectsBothKeysBeforeApplying(t *testing.T) {
	db := setupAutoModelOptionPairTest(t)
	// Stored in reverse order: the maximum would be rejected against the old base
	// if loadOptionsFromDatabase applied rows one by one.
	require.NoError(t, db.Create(&Option{Key: "AutoModelExploreMaxRatio", Value: "0.03"}).Error)
	require.NoError(t, db.Create(&Option{Key: "AutoModelExploreRatio", Value: "0.02"}).Error)

	loadOptionsFromDatabase()

	snapshot := operation_setting.AutoModelRoutingSettingsSnapshot()
	require.InDelta(t, 0.02, snapshot.ExploreRatio, 1e-9)
	require.InDelta(t, 0.03, snapshot.ExploreMaxRatio, 1e-9)
	common.OptionMapRWMutex.RLock()
	require.Equal(t, "0.02", common.OptionMap["AutoModelExploreRatio"])
	require.Equal(t, "0.03", common.OptionMap["AutoModelExploreMaxRatio"])
	common.OptionMapRWMutex.RUnlock()
}
