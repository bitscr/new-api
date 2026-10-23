package service

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAutoModelScorePersistenceV2RoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	oldDB, oldSQLite := model.DB, common.UsingSQLite
	model.DB, common.UsingSQLite = db, true
	oldHealth := autoModelHealth.ReadAll()
	autoModelHealth.Clear()
	t.Cleanup(func() {
		model.DB, common.UsingSQLite = oldDB, oldSQLite
		autoModelHealth.Clear()
		autoModelHealth.AddAll(oldHealth)
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})

	now := time.Now()
	key := autoModelHealthKey("g", "m", 7)
	autoModelHealth.Set(key, autoModelOutcome{
		SuccessCount: 6, FailureCount: 1, LastObservedAt: now,
		StreamLatencyMS: 321, StreamLatencySamples: 4, StreamLatencyUpdatedAt: now,
		NonStreamLatencyMS: 654, NonStreamLatencySamples: 2, NonStreamLatencyUpdatedAt: now,
		PendingRecovery: true,
		ObservationSeq:  1, WriterID: "writer",
	})
	rows := snapshotAutoModelScores()
	require.Len(t, rows, 1)
	outcomes, err := model.SaveAutoModelScores(context.Background(), db, rows)
	require.NoError(t, err)
	require.Equal(t, []model.AutoModelScoreSaveOutcome{model.AutoModelScoreStored}, outcomes)

	autoModelHealth.Clear()
	persistence := &autoModelScorePersistence{db: db}
	require.NoError(t, persistence.restore(context.Background()))
	restored, ok := autoModelHealth.Get(key)
	require.True(t, ok)
	require.InDelta(t, 6, restored.SuccessCount, 1e-9)
	require.InDelta(t, 1, restored.FailureCount, 1e-9)
	require.InDelta(t, 321, restored.StreamLatencyMS, 1e-9)
	require.InDelta(t, 654, restored.NonStreamLatencyMS, 1e-9)
	require.True(t, restored.PendingRecovery)
}
