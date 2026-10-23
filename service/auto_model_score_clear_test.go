package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAutoModelClearTest(t *testing.T) *gorm.DB {
	t.Helper()
	oldDB := model.DB
	oldHealth := autoModelHealth.ReadAll()
	autoModelHealth.Clear()
	autoModelCooldowns.Clear()
	autoModelCooldownLoadOnce = sync.Once{}
	autoModelScoreMutex.Lock()
	oldTombstones := autoModelClearTombstones
	autoModelClearTombstones = make(map[string]autoModelClearTombstone)
	autoModelScoreMutex.Unlock()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.AutoModelCooldown{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = oldDB
		autoModelHealth.Clear()
		autoModelHealth.AddAll(oldHealth)
		autoModelScoreMutex.Lock()
		autoModelClearTombstones = oldTombstones
		autoModelScoreMutex.Unlock()
		sqlDB, closeErr := db.DB()
		if closeErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func resetAutoModelClearMemory(t *testing.T) {
	t.Helper()
	autoModelHealth.Clear()
	autoModelScoreMutex.Lock()
	autoModelClearTombstones = make(map[string]autoModelClearTombstone)
	autoModelScoreMutex.Unlock()
}

func TestClearTombstoneOutranksSnapshotCapturedBeforeClear(t *testing.T) {
	db := setupAutoModelClearTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	group, name, channelID := "clear-race", "m", 41
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, true, false, true, 120, 120, "", time.Now())

	stale := snapshotAutoModelScores()
	require.Len(t, stale, 1)

	cleared, err := ClearAutoModelNegativeEffects(group, name, channelID)
	require.True(t, cleared)
	require.NoError(t, err)
	_, exists := autoModelHealth.Get(autoModelHealthKey(group, name, channelID))
	require.False(t, exists, "clearing must remove online evidence immediately")

	outcomes, err := model.SaveAutoModelScores(context.Background(), db, stale)
	require.NoError(t, err)
	require.Equal(t, []model.AutoModelScoreSaveOutcome{model.AutoModelScoreSuperseded}, outcomes,
		"a snapshot captured before the clear must not be reinserted")

	var rows []model.AutoModelScore
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, model.AutoModelScoreTombstoneVersion, rows[0].Version)
}

func TestRestoreHonorsTombstoneUntilNewerEvidence(t *testing.T) {
	db := setupAutoModelClearTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	now := time.Now()
	tombstoneAt := now.Add(-time.Minute)
	tombstone := model.AutoModelScore{
		Scope: model.AutoModelScoreScopeModel, Group: "g", Model: "m", ChannelID: 7,
		Score: 0.5, LatencyMS: 0, Observations: 1,
		ObservedAt: tombstoneAt.UnixNano(), ObservationSeq: 5, WriterID: "w",
		Version: model.AutoModelScoreTombstoneVersion,
	}
	require.NoError(t, db.Create(&tombstone).Error)

	p := &autoModelScorePersistence{db: db}
	require.NoError(t, p.restore(context.Background()))
	_, exists := autoModelHealth.Get(autoModelHealthKey("g", "m", 7))
	require.False(t, exists, "a tombstone must not restore as evidence")

	resetAutoModelClearMemory(t)
	newer := model.AutoModelScore{
		Scope: model.AutoModelScoreScopeModel, Group: "g", Model: "m", ChannelID: 7,
		Score: 0.5, LatencyMS: 0, Observations: 1,
		SuccessCount: 3, FailureCount: 1,
		ObservedAt: now.Add(-time.Second).UnixNano(), ObservationSeq: 6, WriterID: "w",
		Version: model.AutoModelScoreVersion,
	}
	require.NoError(t, db.Save(&newer).Error)
	require.NoError(t, (&autoModelScorePersistence{db: db}).restore(context.Background()))
	restored, exists := autoModelHealth.Get(autoModelHealthKey("g", "m", 7))
	require.True(t, exists, "a real post-clear observation must survive restore")
	require.InDelta(t, 3, restored.SuccessCount, 1e-9)
}

func TestClearReportsCooldownDeleteFailureAndRetries(t *testing.T) {
	db := setupAutoModelClearTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	group, name, channelID := "clear-cooldown-failure", "m", 45
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, false, false, false, 0, 500, "fast failure", time.Now())
	require.NotEmpty(t, ListAutoModelCooldowns(time.Now()), "control: the pair must be cooling")

	// Fail only the cooldown-row deletion while the score tombstone still works.
	require.NoError(t, db.Migrator().DropTable(&model.AutoModelCooldown{}))
	cleared, err := ClearAutoModelNegativeEffects(group, name, channelID)
	require.True(t, cleared, "online evidence is cleared even when the cooldown row cannot be deleted")
	require.Error(t, err, "the clear API must report the failed cooldown deletion")
	key := autoModelHealthKey(group, name, channelID)
	autoModelCooldownMutex.Lock()
	state, exists := autoModelCooldowns.Get(key)
	autoModelCooldownMutex.Unlock()
	require.True(t, exists)
	require.NotNil(t, state.pending)
	require.True(t, state.pending.delete)

	require.NoError(t, db.AutoMigrate(&model.AutoModelCooldown{}))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, RetryPendingAutoModelCooldownWrites(canceled, db),
		"a canceled worker budget must stop the retry")
	autoModelCooldownMutex.Lock()
	_, exists = autoModelCooldowns.Get(key)
	autoModelCooldownMutex.Unlock()
	require.True(t, exists, "an unfinished retry must remain pending")
	autoModelCooldownMutex.Lock()
	require.NoError(t, RetryPendingAutoModelCooldownWrites(context.Background(), db),
		"a busy cooldown lock skips this background round")
	autoModelCooldownMutex.Unlock()
	require.NoError(t, RetryPendingAutoModelCooldownWrites(context.Background(), db))
	autoModelCooldownMutex.Lock()
	_, exists = autoModelCooldowns.Get(key)
	autoModelCooldownMutex.Unlock()
	require.False(t, exists, "the background retry must finish the pending deletion")
	require.Empty(t, cooldownRows(t))
}

func TestClearBlocksDelayedPreClearFeedbackInMemory(t *testing.T) {
	db := setupAutoModelClearTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	group, name, channelID := "clear-delayed-feedback", "m", 44
	observedAt := time.Now().Add(-time.Minute)
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, true, false, true, 100, 100, "", observedAt)
	cleared, err := ClearAutoModelNegativeEffects(group, name, channelID)
	require.True(t, cleared)
	require.NoError(t, err)

	// The request observed these outcomes before the clear and publishes them late.
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, true, false, true, 100, 100, "", observedAt)
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, false, false, false, 0, 500, "late failure", observedAt)
	_, exists := autoModelHealth.Get(autoModelHealthKey(group, name, channelID))
	require.False(t, exists, "a pre-clear observation must not recreate memory evidence")
	_, cooling := GetAutoModelCooldown(group, name, channelID, time.Now())
	require.False(t, cooling, "a pre-clear failure must not recreate a cooldown")

	time.Sleep(time.Millisecond)
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, true, false, true, 100, 100, "", time.Now())
	restored, exists := autoModelHealth.Get(autoModelHealthKey(group, name, channelID))
	require.True(t, exists, "a genuinely later observation starts learning again")
	require.InDelta(t, 1, restored.SuccessCount, 1e-9)
}

func TestPreClearDeterministicFailureCannotRetireAfterClear(t *testing.T) {
	db := setupAutoModelClearTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}, &model.Channel{}, &model.Ability{}))
	channel := &model.Channel{
		Type: 1, Key: "k", Status: common.ChannelStatusEnabled,
		Name: "retire-race", Group: "retire-race-group", Models: "m",
	}
	require.NoError(t, db.Create(channel).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group: "retire-race-group", Model: "m", ChannelId: channel.Id, Enabled: true,
	}).Error)
	observedAt := time.Now()
	RecordAutoModelOutcomeWithModeAt("retire-race-group", "m", channel.Id, true, false, true, 100, 100, "", observedAt)
	cleared, err := ClearAutoModelNegativeEffects("retire-race-group", "m", channel.Id)
	require.True(t, cleared)
	require.NoError(t, err)

	RecordAutoModelPermanentFailureAt("retire-race-group", "m", channel.Id, "model not found", observedAt)
	var reloaded model.Channel
	require.NoError(t, db.First(&reloaded, channel.Id).Error)
	require.Equal(t, "m", reloaded.Models, "a pre-clear deterministic failure must not retire the model")
	var abilities []model.Ability
	require.NoError(t, db.Where("channel_id = ? AND model = ?", channel.Id, "m").Find(&abilities).Error)
	require.Len(t, abilities, 1, "the ability must remain after a stale deterministic failure")
}

func TestClearPersistenceFailureStaysPendingAndRetries(t *testing.T) {
	db := setupAutoModelClearTest(t) // Deliberately no score table yet.
	group, name, channelID := "clear-pending", "m", 42
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, true, false, true, 80, 80, "", time.Now())

	cleared, err := ClearAutoModelNegativeEffects(group, name, channelID)
	require.True(t, cleared, "online evidence is cleared even when SQL fails")
	require.Error(t, err, "the API must not report a full success when persistence failed")
	pending := pendingAutoModelClearTombstones()
	require.Len(t, pending, 1)

	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, savePendingAutoModelClearTombstones(canceled, db),
		"the batch retry must honor the caller's deadline instead of renewing it per row")
	require.Len(t, pendingAutoModelClearTombstones(), 1)

	require.NoError(t, savePendingAutoModelClearTombstones(context.Background(), db))
	require.Empty(t, pendingAutoModelClearTombstones(), "a successful retry clears the pending flag")
	var rows []model.AutoModelScore
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, model.AutoModelScoreTombstoneVersion, rows[0].Version)
}

func TestClearDoesNotUseClearTimeAsObservationTime(t *testing.T) {
	db := setupAutoModelClearTest(t)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}))
	group, name, channelID := "clear-time", "m", 43
	observedAt := time.Now().Add(-time.Hour)
	RecordAutoModelOutcomeWithModeAt(group, name, channelID, true, false, true, 100, 100, "", observedAt)

	cleared, err := ClearAutoModelNegativeEffects(group, name, channelID)
	require.True(t, cleared)
	require.NoError(t, err)
	_, exists := autoModelHealth.Get(autoModelHealthKey(group, name, channelID))
	require.False(t, exists)

	var rows []model.AutoModelScore
	require.NoError(t, model.DB.Find(&rows).Error)
	require.Len(t, rows, 1)
	require.Equal(t, model.AutoModelScoreTombstoneVersion, rows[0].Version)
	require.False(t, time.Unix(0, rows[0].ObservedAt).Equal(observedAt),
		"the tombstone ordering time is not a business observation")
}
