package service

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"gorm.io/gorm"
)

const autoModelScoreIOTimeout = 2 * time.Second
const autoModelScoreFlushPeriod = 5 * time.Second

// autoModelScoreRetention 是持久化行的保留窗口:超过衰减窗口的观测已经归零,
// 留着也不会影响选路,直接清掉。跟随面板上的 AutoModelScoreDecayMinutes,
// 所以管理员调长衰减,保留期自动跟着变长。
func autoModelScoreRetention() time.Duration {
	return time.Duration(autoModelHalfLifeMs()) * time.Millisecond
}

type autoModelScorePersistence struct {
	// Only the lifecycle's worker (or its joined stop callback) owns this state.
	// The saved map acknowledges exact raw batches, never a newer live map copy.
	db    *gorm.DB
	saved map[autoModelScoreKey]model.AutoModelScore
	// superseded 记录"数据库里已经有更新快照持有这个键"的本地内容。它既不是
	// 已保存(不能假装确认),也不该被无止境重写:只有本地内容再次变化时才重新
	// 参与竞争。多实例共享库时,这让落后的实例停止覆盖领先实例的快照。
	superseded map[autoModelScoreKey]model.AutoModelScore
	nextPrune  time.Time
	loaded     bool
}

type autoModelScoreKey struct {
	scope, group, name string
	channelID          int
}

func autoModelScoreKeyFor(row model.AutoModelScore) autoModelScoreKey {
	return autoModelScoreKey{row.Scope, row.Group, row.Model, row.ChannelID}
}

// StartAutoModelScorePersistence is called once after DB/options initialization
// and before accepting HTTP requests. It restores synchronously, then performs
// SQL-only maintenance; it never dispatches health checks or upstream requests.
// Stop it before closing the database. Abrupt termination can lose the latest
// unsaved interval; snapshots are not an observation/event log.
func StartAutoModelScorePersistence() func() {
	return startAutoModelScorePersistence(model.DB, autoModelScoreFlushPeriod, autoModelScoreIOTimeout)
}

func startAutoModelScorePersistence(db *gorm.DB, period, ioTimeout time.Duration) func() {
	if db == nil {
		return func() {}
	}
	p := &autoModelScorePersistence{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
	if err := p.restore(ctx); err != nil {
		common.SysError("failed to load auto model scores: " + err.Error())
	}
	cancel()
	flush := func() {
		ctx, cancel := context.WithTimeout(context.Background(), ioTimeout)
		defer cancel()
		if !p.loaded {
			if err := p.restore(ctx); err != nil {
				common.SysError("failed to load auto model scores: " + err.Error())
			}
		}
		if err := p.flush(ctx); err != nil {
			common.SysError("failed to save auto model scores: " + err.Error())
		}
		if err := p.prune(ctx, time.Now()); err != nil {
			common.SysError("failed to prune auto model scores: " + err.Error())
		}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	ticker := time.NewTicker(period)
	go func() {
		defer close(done)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				select {
				case <-quit:
					return
				default:
				}
				flush()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(quit)
			<-done // no overlapping writes or worker access after stop returns
			flush()
		})
	}
}

func (p *autoModelScorePersistence) flush(ctx context.Context) error {
	// Clear markers go first: every snapshot captured before a clear must lose
	// against them, whether it arrives from this instance or another one.
	tombstoneErr := errors.Join(savePendingAutoModelClearTombstones(ctx, p.db), RetryPendingAutoModelCooldownWrites(ctx, p.db))
	var changed []model.AutoModelScore
	rows := staleAutoModelScoreRows(snapshotAutoModelScores())
	now := time.Now()
	for _, row := range rows {
		if !validAutoModelScore(row, now) {
			continue
		}
		key := autoModelScoreKeyFor(row)
		if previous, ok := p.saved[key]; ok && previous == row {
			continue // 数据库已经接纳这一行(或库内完全相同)
		}
		if rejected, ok := p.superseded[key]; ok && rejected == row {
			// 该键被更新的快照持有,而本地内容没有再变化:不重写,等下一次
			// 真实反馈产生新快照时再竞争。
			continue
		}
		changed = append(changed, row)
	}
	if len(changed) == 0 {
		return tombstoneErr
	}
	// Keep failed observations dirty; later feedback stays in the online maps
	// while SQL runs, and comparing against this captured batch retains it.
	outcomes, err := model.SaveAutoModelScores(ctx, p.db, changed)
	if err != nil {
		return errors.Join(tombstoneErr, err)
	}
	if p.saved == nil {
		p.saved = make(map[autoModelScoreKey]model.AutoModelScore)
	}
	for i, row := range changed {
		key := autoModelScoreKeyFor(row)
		if i < len(outcomes) && outcomes[i] == model.AutoModelScoreSuperseded {
			// 单独标记,不伪装成已保存:saved 里绝不能出现数据库没有接纳的行。
			delete(p.saved, key)
			if p.superseded == nil {
				p.superseded = make(map[autoModelScoreKey]model.AutoModelScore)
			}
			p.superseded[key] = row
			continue
		}
		delete(p.superseded, key)
		p.saved[key] = row
		expires := time.Unix(0, row.ObservedAt).Add(autoModelScoreRetention())
		if expires.Before(p.nextPrune) {
			p.nextPrune = expires
		}
	}
	return tombstoneErr
}

func (p *autoModelScorePersistence) restore(ctx context.Context) error {
	rows, err := model.LoadAutoModelScores(ctx, p.db)
	if err != nil {
		return err
	}
	now := time.Now()
	// Rejected future evidence must not block valid feedback at the store's
	// strict timestamp guard. Retire only the observation/version read here,
	// outside the score lock, so a concurrent replacement remains untouched.
	for _, row := range rows {
		if row.ObservedAt > now.UnixNano() {
			if err := model.DeleteAutoModelScoreSnapshot(ctx, p.db, row); err != nil {
				return err
			}
		}
	}
	// Reconcile acknowledgements only after the load and cleanup succeed.
	// Missing/invalid rows cannot acknowledge local feedback, including a
	// timestamp-rejected batch whose blocking row another instance removed.
	p.saved = make(map[autoModelScoreKey]model.AutoModelScore, len(rows))
	p.superseded = make(map[autoModelScoreKey]model.AutoModelScore)
	loadedTombstones := make(map[string]model.AutoModelScore)
	for _, row := range rows {
		if row.Version != model.AutoModelScoreTombstoneVersion || !validAutoModelTombstone(row, now) {
			continue
		}
		key := autoModelHealthKey(row.Group, row.Model, row.ChannelID)
		if previous, ok := loadedTombstones[key]; !ok || model.AutoModelScoreIsNewer(row, previous) {
			loadedTombstones[key] = row
		}
	}
	autoModelScoreMutex.Lock()
	for key, row := range loadedTombstones {
		// A clear accepted in this process after the query started must not be
		// replaced by the older stored marker.
		if current, ok := autoModelClearTombstones[key]; ok {
			if !time.Unix(0, row.ObservedAt).After(current.ClearedAt) {
				continue
			}
		}
		autoModelClearTombstones[key] = autoModelClearTombstone{
			ClearedAt:      time.Unix(0, row.ObservedAt),
			ObservationSeq: row.ObservationSeq,
			WriterID:       row.WriterID,
		}
	}
	for _, row := range rows {
		if !validAutoModelScore(row, now) {
			continue
		}
		key := autoModelHealthKey(row.Group, row.Model, row.ChannelID)
		// A tombstone older than this evidence is legitimate post-clear learning;
		// an older evidence row must not resurrect what the administrator cleared.
		if tombstone, cleared := autoModelClearTombstones[key]; cleared {
			tombstoneRow, ok := autoModelClearTombstoneRow(key, tombstone)
			if ok && !model.AutoModelScoreIsNewer(row, tombstoneRow) {
				continue
			}
		}
		p.saved[autoModelScoreKeyFor(row)] = row
		// 旧行可能没有排序元数据(NULL/零值),恢复时按零值兼容:下一次本地
		// 观测会从它继续递增,而不是回退。
		outcome := autoModelOutcome{
			SuccessCount:              row.SuccessCount,
			FailureCount:              row.FailureCount,
			LastObservedAt:            time.Unix(0, row.ObservedAt),
			StreamLatencyMS:           row.StreamLatencyMS,
			StreamLatencySamples:      row.StreamLatencySamples,
			StreamLatencyUpdatedAt:    unixOrZero(row.StreamLatencyAt),
			NonStreamLatencyMS:        row.NonStreamLatencyMS,
			NonStreamLatencySamples:   row.NonStreamLatencySamples,
			NonStreamLatencyUpdatedAt: unixOrZero(row.NonStreamLatencyAt),
			PendingRecovery:           row.PendingRecovery,
			ObservationSeq:            row.ObservationSeq,
			WriterID:                  row.WriterID,
		}
		// Restore only missing keys. Any accepted in-process feedback wins,
		// including feedback saved during an earlier failed-load interval or
		// accepted while this query ran. Never merge two instances' EWMAs.
		if _, exists := autoModelHealth.Get(key); !exists {
			autoModelHealth.Set(key, outcome)
		}
	}
	autoModelScoreMutex.Unlock()
	p.loaded = true // prune failures retry independently without reloading scores
	return p.prune(ctx, now)
}

func validAutoModelTombstone(row model.AutoModelScore, now time.Time) bool {
	if row.Scope != model.AutoModelScoreScopeModel || row.ChannelID <= 0 ||
		row.Group == "" || strings.TrimSpace(row.Group) != row.Group || strings.ContainsRune(row.Group, '\x00') {
		return false
	}
	if row.Model == "" || strings.TrimSpace(row.Model) != row.Model ||
		strings.ContainsRune(row.Model, '\x00') || operation_setting.IsAutoModelName(row.Model) {
		return false
	}
	if row.ObservationSeq < 0 {
		return false
	}
	return row.ObservedAt > 0 && row.ObservedAt <= now.UnixNano()
}

func (p *autoModelScorePersistence) prune(ctx context.Context, now time.Time) error {
	if now.Before(p.nextPrune) {
		return nil
	}
	cutoff := now.Add(-autoModelScoreRetention()).UnixNano()
	if err := model.PruneAutoModelScores(ctx, p.db, cutoff); err != nil {
		return err
	}
	// Prune at the earliest known expiration, or once per retention window for
	// snapshots written by other instances. Ordinary idle ticks do no SQL writes.
	p.nextPrune = now.Add(autoModelScoreRetention())
	for key, row := range p.saved {
		if row.ObservedAt <= cutoff {
			delete(p.saved, key)
			continue
		}
		expires := time.Unix(0, row.ObservedAt).Add(autoModelScoreRetention())
		if expires.Before(p.nextPrune) {
			p.nextPrune = expires
		}
	}
	// 被取代的标记同样会过期:过期后本地内容若仍存在,它会按新快照重新竞争。
	for key, row := range p.superseded {
		if row.ObservedAt <= cutoff {
			delete(p.superseded, key)
		}
	}
	autoModelScoreMutex.Lock()
	for key, tombstone := range autoModelClearTombstones {
		// A pending marker must survive until the database confirms it, even if
		// the wall clock has passed the retention boundary.
		if !tombstone.PersistPending && tombstone.ClearedAt.UnixNano() <= cutoff {
			delete(autoModelClearTombstones, key)
		}
	}
	autoModelScoreMutex.Unlock()
	return nil
}

func unixNanoOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func unixOrZero(unixNano int64) time.Time {
	if unixNano <= 0 {
		return time.Time{}
	}
	return time.Unix(0, unixNano)
}

func validAutoModelScore(row model.AutoModelScore, now time.Time) bool {
	if row.Version != model.AutoModelScoreVersion || row.ChannelID <= 0 ||
		row.Group == "" || strings.TrimSpace(row.Group) != row.Group || strings.ContainsRune(row.Group, '\x00') {
		return false
	}
	// 只恢复模型层快照;历史 scope=channel 行不再加载、不再更新,由通用过期清理淘汰。
	if row.Scope != model.AutoModelScoreScopeModel {
		return false
	}
	if row.Model == "" || strings.TrimSpace(row.Model) != row.Model || strings.ContainsRune(row.Model, '\x00') || operation_setting.IsAutoModelName(row.Model) {
		return false
	}
	if row.SuccessCount < 0 || row.FailureCount < 0 ||
		row.SuccessCount+row.FailureCount <= 0 ||
		row.SuccessCount+row.FailureCount > autoModelMaxEvidenceCount+1e-9 {
		return false
	}
	for _, value := range []float64{
		row.SuccessCount, row.FailureCount,
		row.StreamLatencyMS, row.StreamLatencySamples,
		row.NonStreamLatencyMS, row.NonStreamLatencySamples,
	} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return false
		}
	}
	if row.StreamLatencySamples > 0 && (row.StreamLatencyAt <= 0 || row.StreamLatencyAt > now.UnixNano()) {
		return false
	}
	if row.NonStreamLatencySamples > 0 && (row.NonStreamLatencyAt <= 0 || row.NonStreamLatencyAt > now.UnixNano()) {
		return false
	}
	if row.ObservationSeq < 0 {
		return false
	}
	// Reject future clocks conservatively rather than granting extra freshness.
	// Validation never rewrites timestamps or applies an extra decay fold.
	return row.ObservedAt > 0 && row.ObservedAt <= now.UnixNano() &&
		row.ObservedAt > now.Add(-autoModelScoreRetention()).UnixNano()
}

// Snapshot the model layer under the publication lock, then release it before
// key conversion, validation or any SQL. Score reads remain entirely in memory.
func snapshotAutoModelScores() []model.AutoModelScore {
	autoModelScoreMutex.Lock()
	models := autoModelHealth.ReadAll()
	autoModelScoreMutex.Unlock()
	var rows []model.AutoModelScore
	for key, outcome := range models {
		parts := strings.Split(key, "\x00")
		if len(parts) != 3 {
			continue
		}
		id, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}
		if outcome.SuccessCount+outcome.FailureCount <= 0 || outcome.LastObservedAt.IsZero() {
			continue
		}
		rows = append(rows, model.AutoModelScore{
			Scope: model.AutoModelScoreScopeModel, Group: parts[0], Model: parts[1], ChannelID: id,
			// Legacy columns stay neutral; version 2 readers never use them.
			Score: 0.5, LatencyMS: 0, Observations: 1,
			SuccessCount: outcome.SuccessCount, FailureCount: outcome.FailureCount,
			StreamLatencyMS: outcome.StreamLatencyMS, StreamLatencySamples: outcome.StreamLatencySamples,
			StreamLatencyAt:    unixNanoOrZero(outcome.StreamLatencyUpdatedAt),
			NonStreamLatencyMS: outcome.NonStreamLatencyMS, NonStreamLatencySamples: outcome.NonStreamLatencySamples,
			NonStreamLatencyAt: unixNanoOrZero(outcome.NonStreamLatencyUpdatedAt),
			PendingRecovery:    outcome.PendingRecovery,
			ObservedAt:         outcome.LastObservedAt.UnixNano(), ObservationSeq: outcome.ObservationSeq,
			WriterID: outcome.WriterID, Version: model.AutoModelScoreVersion,
		})
	}
	return rows
}
