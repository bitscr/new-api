package service

import (
	"context"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/require"
)

func TestAutoModelReloadReconcilesPersistedSuccessWithOldCooldown(t *testing.T) {
	db := setupAutoModelPersistenceLifecycleTest(t)
	now := time.Now()
	failedAt := now.Add(-time.Minute).Truncate(time.Second)
	key := autoModelHealthKey("restart-recovery", "slow", 7)
	require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
		Group: "restart-recovery", Model: "slow", ChannelId: 7,
		Until: now.Add(time.Hour).Unix(), Level: 3, Reason: "old failure", UpdatedAt: failedAt.Unix(),
	}))
	// Simulate persisted recovery followed by failed cooldown-row deletion and restart.
	_, err := model.SaveAutoModelScores(context.Background(), db, []model.AutoModelScore{{
		Scope: model.AutoModelScoreScopeModel, Group: "restart-recovery", Model: "slow", ChannelID: 7,
		SuccessCount: 1, ObservedAt: now.UnixNano(), Version: model.AutoModelScoreVersion,
	}})
	require.NoError(t, err)
	p := &autoModelScorePersistence{db: db}
	require.NoError(t, p.restore(context.Background()))
	LoadAutoModelCooldowns()
	require.False(t, autoModelCooldownActive("restart-recovery", "slow", []int{7}))
	require.Empty(t, cooldownRows(t), "newer persisted recovery must finish the failed cooldown deletion")
	require.Equal(t, now.UnixNano(), autoModelCooldownRecoveries[key].UnixNano())
}

func TestAutoModelReloadDoesNotClearSameSecondOrNewerFailure(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	for _, seconds := range []int64{0, 1} {
		key := autoModelHealthKey("reload-order", "slow", 8)
		observedAt := time.Now().Truncate(time.Second).Add(100 * time.Millisecond)
		autoModelHealth.Set(key, autoModelOutcome{SuccessCount: 1, LastObservedAt: observedAt})
		t.Cleanup(func() { autoModelHealth.Delete(key) })
		require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
			Group: "reload-order", Model: "slow", ChannelId: 8,
			Until: observedAt.Add(time.Hour).Unix(), Level: 1, UpdatedAt: observedAt.Unix() + seconds,
		}))
		LoadAutoModelCooldowns()
		require.True(t, autoModelCooldownActive("reload-order", "slow", []int{8}), "ambiguous or newer failure must still block")
	}
}
