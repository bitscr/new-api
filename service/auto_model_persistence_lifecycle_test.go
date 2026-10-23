package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAutoModelPersistenceLifecycleTest(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}, &model.AutoModelCooldown{}))
	oldDB := model.DB
	model.DB = db
	oldHealth := autoModelHealth.ReadAll()
	autoModelHealth.Clear()
	autoModelCooldowns.Clear()
	autoModelCooldownLoadOnce = sync.Once{}
	t.Cleanup(func() {
		model.DB = oldDB
		autoModelHealth.Clear()
		autoModelHealth.AddAll(oldHealth)
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func TestAutoModelScorePersistenceRetriesFailedBatch(t *testing.T) {
	db := setupAutoModelPersistenceLifecycleTest(t)
	key := autoModelHealthKey("persist-retry", "m", 71)
	autoModelHealth.Set(key, autoModelOutcome{
		SuccessCount: 2, LastObservedAt: time.Now(),
		NonStreamLatencyMS: 100, NonStreamLatencySamples: 2, NonStreamLatencyUpdatedAt: time.Now(),
	})
	p := &autoModelScorePersistence{db: db}
	require.NoError(t, db.Migrator().DropTable(&model.AutoModelScore{}))
	err := p.flush(context.Background())
	require.Error(t, err, "a failed batch must stay dirty")

	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	require.NoError(t, p.flush(context.Background()))
	var rows []model.AutoModelScore
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.InDelta(t, 2, rows[0].SuccessCount, 1e-9)
}

func TestAutoModelScorePersistenceConcurrentFeedback(t *testing.T) {
	db := setupAutoModelPersistenceLifecycleTest(t)
	const workers = 40
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordAutoModelOutcomeWithMode("persist-concurrent", "m", 73, true, false, true, 100, 100, "")
		}()
	}
	wg.Wait()
	p := &autoModelScorePersistence{db: db}
	require.NoError(t, p.flush(context.Background()))
	var rows []model.AutoModelScore
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.InDelta(t, workers, rows[0].SuccessCount, 1e-6)
	require.InDelta(t, workers, rows[0].NonStreamLatencySamples, 1e-6)
}

func TestAutoModelScorePersistenceStopFlushesLatestEvidence(t *testing.T) {
	db := setupAutoModelPersistenceLifecycleTest(t)
	stop := startAutoModelScorePersistence(db, time.Hour, time.Second)
	key := autoModelHealthKey("persist-stop", "m", 72)
	now := time.Now()
	autoModelHealth.Set(key, autoModelOutcome{
		SuccessCount: 3, LastObservedAt: now,
		StreamLatencyMS: 200, StreamLatencySamples: 3, StreamLatencyUpdatedAt: now,
	})
	stop()

	var rows []model.AutoModelScore
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.InDelta(t, 3, rows[0].SuccessCount, 1e-9)
	require.Equal(t, model.AutoModelScoreVersion, rows[0].Version)
}
