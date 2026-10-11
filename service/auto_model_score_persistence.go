package service

import (
	"context"
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
	db        *gorm.DB
	saved     map[autoModelScoreKey]model.AutoModelScore
	nextPrune time.Time
	loaded    bool
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
	var changed []model.AutoModelScore
	rows := snapshotAutoModelScores()
	now := time.Now()
	for _, row := range rows {
		if !validAutoModelScore(row, now) {
			continue
		}
		if previous, ok := p.saved[autoModelScoreKeyFor(row)]; !ok || previous != row {
			changed = append(changed, row)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	// Keep failed observations dirty; later feedback stays in the online maps
	// while SQL runs, and comparing against this captured batch retains it.
	if err := model.SaveAutoModelScores(ctx, p.db, changed); err != nil {
		return err
	}
	if p.saved == nil {
		p.saved = make(map[autoModelScoreKey]model.AutoModelScore)
	}
	for _, row := range changed {
		p.saved[autoModelScoreKeyFor(row)] = row
		expires := time.Unix(0, row.ObservedAt).Add(autoModelScoreRetention())
		if expires.Before(p.nextPrune) {
			p.nextPrune = expires
		}
	}
	return nil
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
	autoModelScoreMutex.Lock()
	for _, row := range rows {
		if !validAutoModelScore(row, now) {
			continue
		}
		p.saved[autoModelScoreKeyFor(row)] = row
		outcome := autoModelOutcome{Score: row.Score, LatencyMS: row.LatencyMS, Observations: row.Observations, UpdatedAt: time.Unix(0, row.ObservedAt)}
		// Restore only missing keys. Any accepted in-process feedback wins,
		// including feedback saved during an earlier failed-load interval or
		// accepted while this query ran. Never merge two instances' EWMAs.
		if row.Scope == model.AutoModelScoreScopeModel {
			key := autoModelHealthKey(row.Group, row.Model, row.ChannelID)
			if _, exists := autoModelHealth.Get(key); !exists {
				autoModelHealth.Set(key, outcome)
			}
		} else if row.Scope == model.AutoModelScoreScopeChannel {
			key := autoModelChannelHealthKey(row.Group, row.ChannelID)
			if _, exists := autoModelChannelHealth.Get(key); !exists {
				autoModelChannelHealth.Set(key, outcome)
			}
		}
	}
	autoModelScoreMutex.Unlock()
	p.loaded = true // prune failures retry independently without reloading scores
	return p.prune(ctx, now)
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
	return nil
}

func validAutoModelScore(row model.AutoModelScore, now time.Time) bool {
	if row.Version != model.AutoModelScoreVersion || row.ChannelID <= 0 ||
		row.Group == "" || strings.TrimSpace(row.Group) != row.Group || strings.ContainsRune(row.Group, '\x00') {
		return false
	}
	switch row.Scope {
	case model.AutoModelScoreScopeModel:
		if row.Model == "" || strings.TrimSpace(row.Model) != row.Model || strings.ContainsRune(row.Model, '\x00') || operation_setting.IsAutoModelName(row.Model) {
			return false
		}
	case model.AutoModelScoreScopeChannel:
		if row.Model != "" {
			return false
		}
	default:
		return false
	}
	for _, value := range []float64{row.Score, row.LatencyMS, row.Observations} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	// Reject future clocks conservatively rather than granting extra freshness.
	// Validation never rewrites timestamps or applies an extra decay fold.
	return row.Score >= 0 && row.Score <= 1 && row.LatencyMS >= 0 &&
		row.Observations > 0 && row.Observations <= autoModelMaxObservations &&
		row.ObservedAt > 0 && row.ObservedAt <= now.UnixNano() &&
		row.ObservedAt > now.Add(-autoModelScoreRetention()).UnixNano()
}

// Snapshot both layers together, but release the publication lock before key
// conversion, validation or any SQL. Score reads remain entirely in memory.
func snapshotAutoModelScores() []model.AutoModelScore {
	autoModelScoreMutex.Lock()
	models, channels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
	autoModelScoreMutex.Unlock()
	var rows []model.AutoModelScore
	for _, layer := range []struct {
		scope    string
		outcomes map[string]autoModelOutcome
	}{
		{model.AutoModelScoreScopeModel, models},
		{model.AutoModelScoreScopeChannel, channels},
	} {
		for key, outcome := range layer.outcomes {
			parts := strings.Split(key, "\x00")
			wantParts := 3
			if layer.scope == model.AutoModelScoreScopeChannel {
				wantParts = 2
			}
			if len(parts) != wantParts {
				continue
			}
			id, err := strconv.Atoi(parts[len(parts)-1])
			if err != nil {
				continue
			}
			row := model.AutoModelScore{Scope: layer.scope, Group: parts[0], ChannelID: id,
				Score: outcome.Score, LatencyMS: outcome.LatencyMS, Observations: outcome.Observations,
				ObservedAt: outcome.UpdatedAt.UnixNano(), Version: model.AutoModelScoreVersion}
			if layer.scope == model.AutoModelScoreScopeModel {
				row.Model = parts[1]
			}
			rows = append(rows, row)
		}
	}
	return rows
}
