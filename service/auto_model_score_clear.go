package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

// autoModelClearTombstone 是管理员清除操作的内存标记。
//
// 它不是业务观测:ClearedAt 只用于压过清除前捕获的快照,绝不会成为
// autoModelOutcome.LastObservedAt 或页面上的"最后观测"。
// PersistPending 表示数据库尚未确认这次清除,后台 flush 会持续重试。
type autoModelClearTombstone struct {
	ClearedAt      time.Time
	ObservationSeq int64
	WriterID       string
	PersistPending bool
}

// guarded by autoModelScoreMutex.
var autoModelClearTombstones = make(map[string]autoModelClearTombstone)

func autoModelClearTombstoneRow(key string, tombstone autoModelClearTombstone) (model.AutoModelScore, bool) {
	group, name, channelID, ok := parseAutoModelHealthKey(key)
	if !ok {
		return model.AutoModelScore{}, false
	}
	return model.AutoModelScore{
		Scope: model.AutoModelScoreScopeModel, Group: group, Model: name, ChannelID: channelID,
		// Legacy columns stay neutral; a tombstone is not evidence.
		Score: 0.5, LatencyMS: 0, Observations: 1,
		ObservedAt:     tombstone.ClearedAt.UnixNano(),
		ObservationSeq: tombstone.ObservationSeq,
		WriterID:       tombstone.WriterID,
		Version:        model.AutoModelScoreTombstoneVersion,
	}, true
}

func pendingAutoModelClearTombstones() map[string]autoModelClearTombstone {
	autoModelScoreMutex.Lock()
	defer autoModelScoreMutex.Unlock()
	pending := make(map[string]autoModelClearTombstone)
	for key, tombstone := range autoModelClearTombstones {
		if tombstone.PersistPending {
			pending[key] = tombstone
		}
	}
	return pending
}

func markAutoModelClearTombstonePersisted(key string, tombstone autoModelClearTombstone) {
	autoModelScoreMutex.Lock()
	defer autoModelScoreMutex.Unlock()
	current, ok := autoModelClearTombstones[key]
	if !ok || !current.ClearedAt.Equal(tombstone.ClearedAt) ||
		current.ObservationSeq != tombstone.ObservationSeq || current.WriterID != tombstone.WriterID {
		return
	}
	current.PersistPending = false
	autoModelClearTombstones[key] = current
}

// persistAutoModelClearTombstoneContext writes the clear marker through the
// same per-key ordering guard as evidence. A snapshot captured before the clear
// can therefore never be inserted after it, including by another instance.
// The administrator clear supplies one deadline for this and the cooldown DELETE.
func persistAutoModelClearTombstoneContext(ctx context.Context, db *gorm.DB, key string, tombstone autoModelClearTombstone) error {
	if db == nil {
		return nil // Memory-only mode (tests without a database).
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	row, ok := autoModelClearTombstoneRow(key, tombstone)
	if !ok {
		return fmt.Errorf("invalid auto model score tombstone key %q", key)
	}
	outcomes, err := model.SaveAutoModelScores(ctx, db, []model.AutoModelScore{row})
	if err != nil {
		return err
	}
	if len(outcomes) != 1 {
		return errors.New("auto model score tombstone save returned no outcome")
	}
	// Superseded means a real post-clear observation already owns this key. The
	// marker has still done its job: every older snapshot loses to that evidence.
	switch outcomes[0] {
	case model.AutoModelScoreStored, model.AutoModelScoreUnchanged, model.AutoModelScoreSuperseded:
		return nil
	default:
		return fmt.Errorf("auto model score tombstone for %s was not accepted", key)
	}
}

// savePendingAutoModelClearTombstones is called by the persistence worker before
// it writes evidence snapshots. All pending markers share the caller's single
// I/O budget; giving each row its own timeout could block the worker and normal
// score saves for minutes when there is a backlog.
func savePendingAutoModelClearTombstones(ctx context.Context, db *gorm.DB) error {
	pending := pendingAutoModelClearTombstones()
	if len(pending) == 0 || db == nil {
		return nil
	}
	keys := make([]string, 0, len(pending))
	rows := make([]model.AutoModelScore, 0, len(pending))
	for key, tombstone := range pending {
		row, ok := autoModelClearTombstoneRow(key, tombstone)
		if !ok {
			continue
		}
		keys = append(keys, key)
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	outcomes, err := model.SaveAutoModelScores(ctx, db, rows)
	if err != nil {
		return err
	}
	if len(outcomes) != len(keys) {
		return errors.New("auto model tombstone batch returned incomplete outcomes")
	}
	for i, key := range keys {
		switch outcomes[i] {
		case model.AutoModelScoreStored, model.AutoModelScoreUnchanged, model.AutoModelScoreSuperseded:
			markAutoModelClearTombstonePersisted(key, pending[key])
		default:
			return fmt.Errorf("auto model tombstone for %s was not accepted", key)
		}
	}
	return nil
}

// staleAutoModelScoreRows drops snapshots captured before a clear. The database
// guard is authoritative; this filter prevents the same process from even
// attempting to reinsert a known-stale row.
func staleAutoModelScoreRows(rows []model.AutoModelScore) []model.AutoModelScore {
	autoModelScoreMutex.Lock()
	defer autoModelScoreMutex.Unlock()
	filtered := rows[:0]
	for _, row := range rows {
		key := autoModelHealthKey(row.Group, row.Model, row.ChannelID)
		tombstone, cleared := autoModelClearTombstones[key]
		if cleared {
			tombstoneRow, ok := autoModelClearTombstoneRow(key, tombstone)
			if !ok || !model.AutoModelScoreIsNewer(row, tombstoneRow) {
				continue
			}
		}
		filtered = append(filtered, row)
	}
	return filtered
}

func clearAutoModelNegativeEffectsWithDB(db *gorm.DB, group, name string, channelID int) (bool, error) {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || name == "" || channelID <= 0 {
		return false, nil
	}
	key := autoModelHealthKey(group, name, channelID)
	// A clear is one route publication. Serialize it with normal feedback and
	// deterministic retirement; the clear must finish before any later feedback
	// can observe "not cleared yet" and proceed to mutate the route.
	unlock := lockAutoModelRoute(key)
	defer unlock()
	// One budget covers the cooldown snapshot, cooldown deletion and score
	// tombstone. The tombstone write must not start a fresh timeout after a
	// blocked cooldown DELETE has exhausted the budget.
	ctx, cancel := context.WithTimeout(context.Background(), autoModelScoreIOTimeout)
	defer cancel()
	_, hadCooldown := autoModelCooldownSnapshotContext(ctx, db)[key]

	autoModelScoreMutex.Lock()
	current, hadEvidence := autoModelHealth.Get(key)
	previous, hadTombstone := autoModelClearTombstones[key]
	if !hadEvidence && !hadTombstone && !hadCooldown {
		autoModelScoreMutex.Unlock()
		return false, nil
	}
	clearedAt := time.Now()
	sequenceBase := current
	if previous.ObservationSeq > sequenceBase.ObservationSeq {
		sequenceBase.ObservationSeq = previous.ObservationSeq
	}
	sequence, writerID := nextAutoModelObservationSeq(sequenceBase)
	tombstone := autoModelClearTombstone{
		ClearedAt: clearedAt, ObservationSeq: sequence, WriterID: writerID, PersistPending: true,
	}
	autoModelClearTombstones[key] = tombstone
	if hadEvidence {
		autoModelHealth.Delete(key)
	}
	autoModelScoreMutex.Unlock()

	cooldownErr := clearAutoModelCooldownWithResultContext(ctx, db, group, name, channelID, clearedAt)
	err := errors.Join(persistAutoModelClearTombstoneContext(ctx, db, key, tombstone), cooldownErr)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to persist auto model score clear %s/%s#%d: %s", group, name, channelID, err.Error()))
		return true, err
	}
	markAutoModelClearTombstonePersisted(key, tombstone)
	common.SysLog(fmt.Sprintf("auto model: %s(渠道 %d)的负面效果已清除，恢复为待观察", name, channelID))
	return true, nil
}
