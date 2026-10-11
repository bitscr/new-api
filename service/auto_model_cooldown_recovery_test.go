package service

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAutoModelCooldownInitialLoadPreservesUnpersistedFailure(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	autoModelCooldowns.Clear()
	autoModelCooldownLoadOnce = sync.Once{}
	db := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = db })
	now := time.Now()
	tripAutoModelCooldown("default", "local-model", 1, "offline failure", now)
	key := autoModelHealthKey("default", "local-model", 1)
	state, ok := autoModelCooldowns.Get(key)
	require.True(t, ok)
	require.NotNil(t, state.pending)
	require.Equal(t, map[int]bool{1: true}, GetAutoModelCoolingChannelIDs("default", "local-model"))

	// The first successful load sees an empty database, but must retain and
	// persist the failure accepted before the database became available.
	model.DB = db
	require.Equal(t, map[int]bool{1: true}, GetAutoModelCoolingChannelIDs("default", "local-model"))
	rows := cooldownRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, 1, rows[0].Level)
	require.Equal(t, now.Add(autoModelCooldownBase).Unix(), rows[0].Until)
	state, ok = autoModelCooldowns.Get(key)
	require.True(t, ok)
	require.Nil(t, state.pending)
	LoadAutoModelCooldowns()
	require.Equal(t, map[int]bool{1: true}, GetAutoModelCoolingChannelIDs("default", "local-model"))
}

func TestAutoModelCooldownInitialLoadPreservesUnpersistedRecovery(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()
	tripAutoModelCooldown("default", "recovered-model", 1, "old failure", now)
	tripAutoModelCooldown("default", "other-model", 1, "other failure", now)
	autoModelCooldowns.Clear()
	autoModelCooldownLoadOnce = sync.Once{}
	db := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = db })

	// Initial loading fails and the local cache has no key. The success still
	// needs a tombstone so a subsequently loaded old row cannot revive it.
	clearAutoModelCooldown("default", "recovered-model", 1, now)
	key := autoModelHealthKey("default", "recovered-model", 1)
	state, ok := autoModelCooldowns.Get(key)
	require.True(t, ok)
	require.NotNil(t, state.pending)
	require.True(t, state.pending.delete)
	require.True(t, state.Until.IsZero())
	model.DB = db
	LoadAutoModelCooldowns()
	require.Empty(t, GetAutoModelCoolingChannelIDs("default", "recovered-model"))
	require.Equal(t, map[int]bool{1: true}, GetAutoModelCoolingChannelIDs("default", "other-model"))
	rows := cooldownRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, "other-model", rows[0].Model)
	_, ok = autoModelCooldowns.Get(key)
	require.False(t, ok)
}

func TestAutoModelCooldownFailedDeleteRetriesWithoutResurrection(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()
	tripAutoModelCooldown("default", "model-a", 1, "failure", now)
	db := model.DB
	var fail atomic.Bool
	fail.Store(true)
	const callbackName = "test:auto_model_cooldown_delete_recovery"
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_cooldowns" && fail.Load() {
			tx.AddError(errors.New("temporary cooldown delete failure"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Delete().Remove(callbackName)) })

	clearAutoModelCooldown("default", "model-a", 1, now)
	key := autoModelHealthKey("default", "model-a", 1)
	state, ok := autoModelCooldowns.Get(key)
	require.True(t, ok)
	require.NotNil(t, state.pending)
	require.True(t, state.pending.delete)
	require.Empty(t, GetAutoModelCoolingChannelIDs("default", "model-a"), "uncommitted recovery is not an active cooldown")
	require.Len(t, cooldownRows(t), 1, "injected deletion failure leaves the stored row")
	LoadAutoModelCooldowns()
	require.Empty(t, GetAutoModelCoolingChannelIDs("default", "model-a"), "reloading must not resurrect a pending deletion")

	fail.Store(false)
	clearAutoModelCooldown("default", "model-a", 1, now)
	require.Empty(t, cooldownRows(t), "a later success retries the failed database deletion")
	_, ok = autoModelCooldowns.Get(key)
	require.False(t, ok)
	LoadAutoModelCooldowns()
	require.Empty(t, GetAutoModelCoolingChannelIDs("default", "model-a"))
}

func TestAutoModelCooldownReloadPreservesNewerFailedUpsert(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()
	tripAutoModelCooldown("default", "model-a", 1, "first failure", now)
	db := model.DB
	var fail atomic.Bool
	fail.Store(true)
	const callbackName = "test:auto_model_cooldown_upsert_recovery"
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_cooldowns" && fail.Load() {
			tx.AddError(errors.New("temporary cooldown upsert failure"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, db.Callback().Create().Remove(callbackName)) })

	tripAutoModelCooldown("default", "model-a", 1, "second failure", now)
	rows := cooldownRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, 1, rows[0].Level)
	key := autoModelHealthKey("default", "model-a", 1)
	state, ok := autoModelCooldowns.Get(key)
	require.True(t, ok)
	require.Equal(t, 2, state.Level)
	require.NotNil(t, state.pending)
	LoadAutoModelCooldowns()
	state, ok = autoModelCooldowns.Get(key)
	require.True(t, ok)
	require.Equal(t, 2, state.Level, "older stored state must not overwrite a failed local upsert")

	fail.Store(false)
	require.Equal(t, map[int]bool{1: true}, GetAutoModelCoolingChannelIDs("default", "model-a"),
		"ordinary reads retry pending writes even after initial loading succeeded")
	rows = cooldownRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, 2, rows[0].Level)
	require.Equal(t, "second failure", rows[0].Reason)
	state, ok = autoModelCooldowns.Get(key)
	require.True(t, ok)
	require.Nil(t, state.pending)
	autoModelCooldowns.Clear()
	LoadAutoModelCooldowns()
	state, ok = autoModelCooldowns.Get(key)
	require.True(t, ok)
	require.Equal(t, 2, state.Level, "reconciled feedback survives a fresh load")
}

func TestAutoModelCooldownFailureAfterPendingRecoveryStartsFresh(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()
	tripAutoModelCooldown("default", "model-a", 1, "first failure", now)
	tripAutoModelCooldown("default", "model-a", 1, "second failure", now)
	db := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = db })
	clearAutoModelCooldown("default", "model-a", 1, now)
	tripAutoModelCooldown("default", "model-a", 1, "new failure", now)
	state, ok := autoModelCooldowns.Get(autoModelHealthKey("default", "model-a", 1))
	require.True(t, ok)
	require.Equal(t, 1, state.Level)
	require.NotNil(t, state.pending)
	require.False(t, state.pending.delete)
	model.DB = db
	LoadAutoModelCooldowns()
	rows := cooldownRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, 1, rows[0].Level)
	require.Equal(t, "new failure", rows[0].Reason)
}
