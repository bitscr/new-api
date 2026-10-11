package service

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAutoModelScorePackageFixtureRegistersTable(t *testing.T) {
	require.True(t, model.DB.Migrator().HasTable(&model.AutoModelScore{}), "the service package DB fixture must migrate the new score table")
}

func setupAutoModelScorePersistenceTestDB(t *testing.T, paths ...string) *gorm.DB {
	t.Helper()
	dsn := ":memory:"
	if len(paths) > 0 {
		dsn = paths[0]
	}
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.AutoModelScore{}, &model.AutoModelCooldown{}))
	oldDB := model.DB
	oldModels, oldChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
	autoModelHealth.Clear()
	autoModelChannelHealth.Clear()
	model.DB = db
	t.Cleanup(func() {
		model.DB = oldDB
		autoModelHealth.Clear()
		autoModelHealth.AddAll(oldModels)
		autoModelChannelHealth.Clear()
		autoModelChannelHealth.AddAll(oldChannels)
		require.NoError(t, sqlDB.Close())
	})
	return db
}

func assertAutoModelScoreRow(t *testing.T, db *gorm.DB, scope, group, name string, channelID int, want autoModelOutcome) {
	t.Helper()
	var row model.AutoModelScore
	require.NoError(t, db.Where(map[string]any{"scope": scope, "group": group, "model": name, "channel_id": channelID}).Take(&row).Error)
	require.Equal(t, model.AutoModelScore{Scope: scope, Group: group, Model: name, ChannelID: channelID, Score: want.Score, LatencyMS: want.LatencyMS, Observations: want.Observations, ObservedAt: want.UpdatedAt.UnixNano(), Version: 1}, row)
}

func assertAutoModelScoreOutcomes(t *testing.T, want, got map[string]autoModelOutcome) {
	t.Helper()
	require.Len(t, got, len(want))
	for key, expected := range want {
		actual, ok := got[key]
		require.True(t, ok, "missing score key %q", key)
		require.Equal(t, expected.UpdatedAt.UnixNano(), actual.UpdatedAt.UnixNano(), "observation time must not refresh on reload")
		actual.UpdatedAt = expected.UpdatedAt
		require.Equal(t, expected, actual, "raw score evidence must not be decayed twice or replaced by effective ranking scores")
	}
}

func TestAutoModelScoreStartRestoresFreshConnectionBeforeReturning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scores.db")
	db := setupAutoModelScorePersistenceTestDB(t, path)
	stop := StartAutoModelScorePersistence()
	RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
	RecordAutoModelOutcome("g", "beta", 1, true, 150, 0)
	RecordAutoModelOutcome("g", "alpha", 2, true, 50, 0)
	stop()
	wantModels, wantChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
	snapshot := autoModelRouteSnapshot{
		Targets:     map[string][]model.AutoModelRoutingTarget{"alpha": {{ChannelID: 1, Weight: 1}, {ChannelID: 2, Weight: 1}}, "beta": {{ChannelID: 1, Weight: 1}}},
		ModelHealth: wantModels, ChannelHealth: wantChannels, Now: time.Now(),
	}
	wantRoutes := buildAutoModelRoutesFromSnapshot("g", []string{"alpha", "beta"}, snapshot, func() float64 { return 0 })
	require.Equal(t, []AutoModelRoute{{Group: "g", ModelName: "alpha", ChannelID: 1}, {Group: "g", ModelName: "beta", ChannelID: 1}, {Group: "g", ModelName: "alpha", ChannelID: 2}}, wantRoutes)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	autoModelHealth.Clear()
	autoModelChannelHealth.Clear()
	reopened, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	reopenedSQL, err := reopened.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopenedSQL.Close()) })
	model.DB = reopened
	stopReloaded := StartAutoModelScorePersistence()
	t.Cleanup(stopReloaded)
	assertAutoModelScoreOutcomes(t, wantModels, autoModelHealth.ReadAll())
	assertAutoModelScoreOutcomes(t, wantChannels, autoModelChannelHealth.ReadAll())
	snapshot.ModelHealth, snapshot.ChannelHealth = autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
	require.Equal(t, wantRoutes, buildAutoModelRoutesFromSnapshot("g", []string{"alpha", "beta"}, snapshot, func() float64 { return 0 }))
}

func TestAutoModelScoreFlushWritesOnlyChangedRawObservations(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	p := &autoModelScorePersistence{db: db}
	require.NoError(t, p.restore(context.Background()))
	var writes, writtenRows int
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:score_idle_writes", func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_scores" {
			writes++
			writtenRows += tx.Statement.ReflectValue.Len()
		}
	}))
	RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
	require.NoError(t, p.flush(context.Background()))
	require.Equal(t, 1, writes)
	require.Equal(t, 2, writtenRows)
	original, _ := autoModelHealth.Get(autoModelHealthKey("g", "alpha", 1))
	for i := 0; i < 4; i++ {
		_ = effectiveScore(original, original.UpdatedAt.Add(time.Duration(i)*time.Minute))
		require.NoError(t, p.flush(context.Background()))
	}
	require.Equal(t, 1, writes, "idle aging/flushes must not rewrite unchanged raw observations")
	RecordAutoModelOutcome("g", "beta", 1, true, 200, 0)
	require.NoError(t, p.flush(context.Background()))
	require.Equal(t, 2, writes)
	require.Equal(t, 4, writtenRows, "only the new model and changed shared channel should be saved")
	autoModelHealth.Clear()
	autoModelChannelHealth.Clear()
	reloaded := &autoModelScorePersistence{db: db}
	require.NoError(t, reloaded.restore(context.Background()))
	require.NoError(t, reloaded.flush(context.Background()))
	require.Equal(t, 2, writes, "restored rows are already durable and must not become dirty merely on load")
}

func TestAutoModelScoreSnapshotPairsStayConsistentDuringFeedback(t *testing.T) {
	setupAutoModelScorePersistenceTestDB(t)
	RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
	checkPair := func() {
		rows := snapshotAutoModelScores()
		require.Len(t, rows, 2)
		a, b := rows[0], rows[1]
		require.Equal(t, a.ObservedAt, b.ObservedAt, "one actual observation must publish both scoring layers with one observation time")
		require.Equal(t, a.Score, b.Score, "snapshot must not split a feedback update between its two layers")
		require.Equal(t, a.LatencyMS, b.LatencyMS)
		require.Equal(t, a.Observations, b.Observations)
	}
	checkPair()
	var wg sync.WaitGroup
	defer wg.Wait()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 500; n++ {
				RecordAutoModelOutcome("g", "alpha", 1, true, int64(100+n), 0)
			}
		}()
	}
	for i := 0; i < 500; i++ {
		checkPair()
	}
}

func TestAutoModelScoreLoadRetryPreservesFeedbackAcceptedDuringSQL(t *testing.T) {
	for _, flushBeforeRetry := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsaved", true: "already_saved"}[flushBeforeRetry], func(t *testing.T) {
			db := setupAutoModelScorePersistenceTestDB(t)
			old := model.AutoModelScore{Scope: "model", Group: "g", Model: "alpha", ChannelID: 1, Score: 0.1, LatencyMS: 999, Observations: 8, ObservedAt: time.Now().Add(-time.Minute).UnixNano(), Version: 1}
			channel := old
			channel.Scope, channel.Model = "channel", ""
			untouched := old
			untouched.Model, untouched.ChannelID = "untouched", 2
			require.NoError(t, db.Create(&[]model.AutoModelScore{old, channel, untouched}).Error)
			var fail atomic.Bool
			fail.Store(true)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce, blockOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			require.NoError(t, db.Callback().Query().After("gorm:query").Register("test:score_load_retry", func(tx *gorm.DB) {
				if tx.Statement.Table != "auto_model_scores" {
					return
				}
				if fail.Load() {
					tx.AddError(errors.New("initial score read failed"))
					return
				}
				blockOnce.Do(func() { close(entered); <-release })
			}))
			p := &autoModelScorePersistence{db: db}
			require.ErrorContains(t, p.restore(context.Background()), "initial score read failed")
			RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
			if flushBeforeRetry {
				require.NoError(t, p.flush(context.Background()))
			}
			fail.Store(false)
			result, finished := make(chan error, 1), make(chan struct{})
			go func() { defer close(finished); result <- p.restore(context.Background()) }()
			defer func() { unblock(); <-finished }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("retry must issue an actual SQL load")
			}
			feedbackDone := make(chan struct{})
			go func() { RecordAutoModelOutcome("g", "alpha", 1, true, 200, 0); close(feedbackDone) }()
			defer func() { unblock(); <-feedbackDone }()
			select {
			case <-feedbackDone:
			case <-time.After(time.Second):
				t.Fatal("SQL load must not hold the score-update lock")
			}
			wantModels, wantChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
			unblock()
			require.NoError(t, <-result)
			wantModels[autoModelHealthKey("g", "untouched", 2)] = autoModelOutcome{Score: untouched.Score, LatencyMS: untouched.LatencyMS, Observations: untouched.Observations, UpdatedAt: time.Unix(0, untouched.ObservedAt)}
			assertAutoModelScoreOutcomes(t, wantModels, autoModelHealth.ReadAll())
			assertAutoModelScoreOutcomes(t, wantChannels, autoModelChannelHealth.ReadAll())
			require.NoError(t, p.flush(context.Background()))
			assertAutoModelScoreRow(t, db, "model", "g", "alpha", 1, wantModels[autoModelHealthKey("g", "alpha", 1)])
			assertAutoModelScoreRow(t, db, "channel", "g", "", 1, wantChannels[autoModelChannelHealthKey("g", 1)])
		})
	}
}

func TestAutoModelScoreExplicitWorkerFlushesAndStopsIdempotently(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	var writes atomic.Int64
	written := make(chan struct{}, 1)
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:score_worker", func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_scores" {
			writes.Add(1)
			select {
			case written <- struct{}{}:
			default:
			}
		}
	}))
	stop := StartAutoModelScorePersistence()
	t.Cleanup(stop)
	RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
	select {
	case <-written:
	case <-time.After(7 * time.Second):
		t.Fatal("the explicitly started default five-second worker must persist observed feedback without waiting for shutdown")
	}
	stop()
	rows, err := model.LoadAutoModelScores(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.EqualValues(t, 1, writes.Load(), "shutdown must not rewrite a just-persisted idle snapshot")
	RecordAutoModelOutcome("g", "alpha", 1, true, 200, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); stop() }()
	}
	wg.Wait()
	after, err := model.LoadAutoModelScores(context.Background(), db)
	require.NoError(t, err)
	require.Equal(t, rows, after, "repeated stop calls are no-ops even if later feedback exists")
	require.EqualValues(t, 1, writes.Load())
}

func TestAutoModelScoreWorkerRetriesInitialLoadWithoutReplacingFeedback(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	old := model.AutoModelScore{Scope: "model", Group: "g", Model: "alpha", ChannelID: 1, Score: 0.1, Observations: 7, ObservedAt: time.Now().Add(-time.Minute).UnixNano(), Version: 1}
	untouched := old
	untouched.Model, untouched.ChannelID = "untouched", 2
	require.NoError(t, db.Create(&[]model.AutoModelScore{old, untouched}).Error)
	var fail atomic.Bool
	var attempts atomic.Int64
	fail.Store(true)
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:score_worker_retry", func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_scores" {
			attempts.Add(1)
			if fail.Load() {
				tx.AddError(errors.New("startup load unavailable"))
			}
		}
	}))
	stop := startAutoModelScorePersistence(db, 20*time.Millisecond, 100*time.Millisecond)
	t.Cleanup(stop)
	require.EqualValues(t, 1, attempts.Load(), "initial restore runs before start returns")
	RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
	wantModels, wantChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
	fail.Store(false)
	require.Eventually(t, func() bool { _, ok := autoModelHealth.Get(autoModelHealthKey("g", "untouched", 2)); return ok }, time.Second, 5*time.Millisecond, "the worker must retry an initially failed load, not just save new observations")
	stop()
	require.EqualValues(t, 2, attempts.Load(), "a successfully restored worker must not reload on every tick/stop")
	wantModels[autoModelHealthKey("g", "untouched", 2)] = autoModelOutcome{Score: untouched.Score, Observations: untouched.Observations, UpdatedAt: time.Unix(0, untouched.ObservedAt)}
	assertAutoModelScoreOutcomes(t, wantModels, autoModelHealth.ReadAll())
	assertAutoModelScoreOutcomes(t, wantChannels, autoModelChannelHealth.ReadAll())
	assertAutoModelScoreRow(t, db, "model", "g", "alpha", 1, wantModels[autoModelHealthKey("g", "alpha", 1)])
}

func TestAutoModelScoreFlushRetainsFeedbackDuringSQLAndAfterFailure(t *testing.T) {
	for _, failFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "slow_commit", true: "failed_commit"}[failFirst], func(t *testing.T) {
			db := setupAutoModelScorePersistenceTestDB(t)
			p := &autoModelScorePersistence{db: db}
			require.NoError(t, p.restore(context.Background()))
			RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
			original, _ := autoModelHealth.Get(autoModelHealthKey("g", "alpha", 1))
			if failFirst {
				require.NoError(t, db.Exec(`CREATE TRIGGER fail_score_batch BEFORE INSERT ON auto_model_scores WHEN NEW.scope = 'channel' BEGIN SELECT RAISE(ABORT, 'transient score failure'); END`).Error)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var blockOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			var writes atomic.Int64
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:score_write_barrier", func(tx *gorm.DB) {
				if tx.Statement.Table != "auto_model_scores" {
					return
				}
				writes.Add(1)
				blockOnce.Do(func() { close(entered); <-release })
			}))
			result, finished := make(chan error, 1), make(chan struct{})
			go func() { defer close(finished); result <- p.flush(context.Background()) }()
			defer func() { unblock(); <-finished }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("flush must reach real SQL")
			}
			feedbackDone := make(chan struct{})
			go func() {
				RecordAutoModelOutcome("g", "alpha", 1, true, 200, 0)
				RecordAutoModelOutcome("g", "beta", 1, true, 400, 0)
				close(feedbackDone)
			}()
			defer func() { unblock(); <-feedbackDone }()
			select {
			case <-feedbackDone:
			case <-time.After(time.Second):
				t.Fatal("feedback must not wait for a score SQL write")
			}
			wantModels, wantChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
			unblock()
			err := <-result
			if failFirst {
				require.ErrorContains(t, err, "transient score failure")
				rows, err := model.LoadAutoModelScores(context.Background(), db)
				require.NoError(t, err)
				require.Empty(t, rows, "failed captured batches cannot publish a partial scoring layer")
				require.NoError(t, db.Exec("DROP TRIGGER fail_score_batch").Error)
			} else {
				require.NoError(t, err)
				assertAutoModelScoreRow(t, db, "model", "g", "alpha", 1, original)
			}
			require.NoError(t, p.flush(context.Background()))
			for name, key := range map[string]string{"alpha": autoModelHealthKey("g", "alpha", 1), "beta": autoModelHealthKey("g", "beta", 1)} {
				assertAutoModelScoreRow(t, db, "model", "g", name, 1, wantModels[key])
			}
			assertAutoModelScoreRow(t, db, "channel", "g", "", 1, wantChannels[autoModelChannelHealthKey("g", 1)])
			require.NoError(t, p.flush(context.Background()))
			require.EqualValues(t, 2, writes.Load(), "ack only the captured successful batch, then remain idle")
		})
	}
}

func TestAutoModelScoreFailedWriteRetriesWithoutAnotherObservation(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	p := &autoModelScorePersistence{db: db}
	require.NoError(t, p.restore(context.Background()))
	RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
	wantModels, wantChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
	require.NoError(t, db.Exec(`CREATE TRIGGER fail_idle_score BEFORE INSERT ON auto_model_scores BEGIN SELECT RAISE(ABORT, 'retry me'); END`).Error)
	require.ErrorContains(t, p.flush(context.Background()), "retry me")
	require.NoError(t, db.Exec("DROP TRIGGER fail_idle_score").Error)
	require.NoError(t, p.flush(context.Background()))
	assertAutoModelScoreRow(t, db, "model", "g", "alpha", 1, wantModels[autoModelHealthKey("g", "alpha", 1)])
	assertAutoModelScoreRow(t, db, "channel", "g", "", 1, wantChannels[autoModelChannelHealthKey("g", 1)])
}

func TestAutoModelScoreLifecycleUsesCapturedDBAndBoundsSQLWaits(t *testing.T) {
	t.Run("stable_handle", func(t *testing.T) {
		original := setupAutoModelScorePersistenceTestDB(t)
		stop := StartAutoModelScorePersistence()
		t.Cleanup(stop)
		replacement := setupAutoModelScorePersistenceTestDB(t)
		RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
		stop()
		rows, err := model.LoadAutoModelScores(context.Background(), original)
		require.NoError(t, err)
		require.Len(t, rows, 2, "all worker I/O must use the DB captured at startup")
		rows, err = model.LoadAutoModelScores(context.Background(), replacement)
		require.NoError(t, err)
		require.Empty(t, rows)
	})
	t.Run("nil", func(t *testing.T) {
		setupAutoModelScorePersistenceTestDB(t)
		model.DB = nil
		require.NotPanics(t, func() { stop := StartAutoModelScorePersistence(); stop(); stop() })
	})
	t.Run("pool_deadlines", func(t *testing.T) {
		db := setupAutoModelScorePersistenceTestDB(t)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		conn, err := sqlDB.Conn(context.Background())
		require.NoError(t, err)
		defer conn.Close()
		started := time.Now()
		stop := startAutoModelScorePersistence(db, time.Hour, 30*time.Millisecond)
		t.Cleanup(stop)
		require.Less(t, time.Since(started), time.Second, "initial restore cannot hang behind an exhausted SQL pool")
		RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
		started = time.Now()
		stop()
		require.Less(t, time.Since(started), time.Second, "shutdown flush must obey the SQL deadline")
		require.NoError(t, conn.Close())
		rows, err := model.LoadAutoModelScores(context.Background(), db)
		require.NoError(t, err)
		require.Empty(t, rows, "timed-out writes cannot be reported as committed")
		// A later explicit lifecycle can still persist the unchanged in-memory
		// observations; the timed-out stop did not erase their pending state.
		restarted := startAutoModelScorePersistence(db, time.Hour, time.Second)
		restarted()
		rows, err = model.LoadAutoModelScores(context.Background(), db)
		require.NoError(t, err)
		require.Len(t, rows, 2)
	})
}

func TestAutoModelScorePruneFailureIsRetried(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	old := model.AutoModelScore{Scope: "model", Group: "g", Model: "old", ChannelID: 1, Score: 0.7, Observations: 3, ObservedAt: time.Now().Add(-time.Hour).UnixNano(), Version: 1}
	require.NoError(t, db.Create(&old).Error)
	require.NoError(t, db.Exec(`CREATE TRIGGER fail_score_prune BEFORE DELETE ON auto_model_scores BEGIN SELECT RAISE(ABORT, 'prune unavailable'); END`).Error)
	p := &autoModelScorePersistence{db: db}
	require.ErrorContains(t, p.restore(context.Background()), "prune unavailable")
	require.True(t, p.loaded, "a prune failure must not invalidate a successful score read")
	require.NoError(t, db.Exec("DROP TRIGGER fail_score_prune").Error)
	require.NoError(t, p.prune(context.Background(), time.Now()))
	rows, err := model.LoadAutoModelScores(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestAutoModelScoreIdleWorkerDoesNotReloadOrRewrite(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	var reads, writes, deletes atomic.Int64
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:score_idle_read", func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_scores" {
			reads.Add(1)
		}
	}))
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:score_idle_write", func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_scores" {
			writes.Add(1)
		}
	}))
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("test:score_idle_delete", func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_scores" {
			deletes.Add(1)
		}
	}))
	stop := startAutoModelScorePersistence(db, 10*time.Millisecond, time.Second)
	defer stop()
	RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
	require.Eventually(t, func() bool { return writes.Load() == 1 }, time.Second, time.Millisecond)
	for i := 0; i < 100; i++ {
		outcome, _ := autoModelHealth.Get(autoModelHealthKey("g", "alpha", 1))
		_ = effectiveScore(outcome, time.Now())
	}
	<-time.After(100 * time.Millisecond)
	stop()
	require.EqualValues(t, 1, reads.Load(), "score reads and idle ticks never reload SQL")
	require.EqualValues(t, 1, writes.Load(), "idle worker ticks must not rewrite a durable raw batch")
	require.EqualValues(t, 1, deletes.Load(), "only the initial expiration prune is due")
}

func TestAutoModelScoreRestoredHintsCannotCreateRoutingTargets(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	ghost := model.AutoModelScore{Scope: "model", Group: "g", Model: "deleted", ChannelID: 99, Score: 1, Observations: 20, ObservedAt: time.Now().UnixNano(), Version: 1}
	channel := ghost
	channel.Scope, channel.Model = "channel", ""
	require.NoError(t, db.Create(&[]model.AutoModelScore{ghost, channel}).Error)
	p := &autoModelScorePersistence{db: db}
	require.NoError(t, p.restore(context.Background()))
	snapshot := autoModelRouteSnapshot{
		Targets:     map[string][]model.AutoModelRoutingTarget{"live": {{ChannelID: 1, Weight: 1}}},
		ModelHealth: autoModelHealth.ReadAll(), ChannelHealth: autoModelChannelHealth.ReadAll(), Now: time.Now(),
	}
	require.Equal(t, []AutoModelRoute{{Group: "g", ModelName: "live", ChannelID: 1}}, buildAutoModelRoutesFromSnapshot("g", []string{"deleted", "live"}, snapshot, func() float64 { return 0 }), "historical scores cannot re-enable a deleted/absent routing target")
}

func TestAutoModelScoreExpirationPrunesWithoutRevivingIdleEvidence(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	now := time.Now()
	// 过期/新鲜的边界跟随可配置的衰减窗口,不写死 11m/5m。
	window := autoModelScoreRetention()
	old := model.AutoModelScore{Scope: "model", Group: "g", Model: "old", ChannelID: 1, Score: 0.9, LatencyMS: 300, Observations: 4, ObservedAt: now.Add(-window - time.Minute).UnixNano(), Version: 1}
	oldChannel := old
	oldChannel.Scope, oldChannel.Model = "channel", ""
	fresh := old
	fresh.Model, fresh.ChannelID, fresh.ObservedAt = "fresh", 2, now.Add(-window/2).UnixNano()
	require.NoError(t, db.Create(&[]model.AutoModelScore{old, oldChannel, fresh}).Error)
	p := &autoModelScorePersistence{db: db}
	require.NoError(t, p.restore(context.Background()))
	rows, err := model.LoadAutoModelScores(context.Background(), db)
	require.NoError(t, err)
	require.Equal(t, []model.AutoModelScore{fresh}, rows, "initial restore must prune expired snapshots from both scopes")
	// An inactive entry can remain in the existing online maps; it must not
	// be reinserted by a later flush, nor may invalid floating-point state.
	outcome := autoModelOutcome{Score: old.Score, LatencyMS: old.LatencyMS, Observations: old.Observations, UpdatedAt: time.Unix(0, old.ObservedAt)}
	autoModelHealth.Set(autoModelHealthKey("g", "old", 1), outcome)
	autoModelChannelHealth.Set(autoModelChannelHealthKey("g", 1), outcome)
	outcome.Score, outcome.UpdatedAt = math.NaN(), now
	autoModelHealth.Set(autoModelHealthKey("g", "invalid", 3), outcome)
	var writes, deletes int
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:expired_writes", func(tx *gorm.DB) { writes++ }))
	require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("test:expired_prunes", func(tx *gorm.DB) { deletes++ }))
	require.NoError(t, p.flush(context.Background()))
	require.NoError(t, p.prune(context.Background(), now.Add(time.Minute)))
	require.Zero(t, writes, "idle/expired/malformed memory must not create repeated score writes")
	require.Zero(t, deletes, "an idle tick must not issue another prune before any known expiry")
	// fresh 在窗口一半处观测,所以它恰好在 now+window/2 过期。
	require.NoError(t, p.prune(context.Background(), now.Add(window/2+time.Minute)))
	rows, err = model.LoadAutoModelScores(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, rows, "runtime aging must eventually prune snapshots even without another observation")
	require.Equal(t, 1, deletes)
}

func TestAutoModelScoreRestoreValidatesRawEvidence(t *testing.T) {
	now := time.Now()
	// "expired" 必须真的超过当前保留窗口;窗口可配置,所以按窗口算。
	window := autoModelScoreRetention()
	fresh := model.AutoModelScore{Scope: "model", Group: "g", Model: "alpha", ChannelID: 1, Score: 0.8, LatencyMS: 300, Observations: 9.5, ObservedAt: now.Add(-5 * time.Minute).UnixNano(), Version: 1}
	for _, tc := range []struct {
		name   string
		mutate func(*model.AutoModelScore)
	}{
		{"expired", func(r *model.AutoModelScore) { r.ObservedAt = now.Add(-window - time.Minute).UnixNano() }},
		{"future", func(r *model.AutoModelScore) { r.ObservedAt = now.Add(time.Hour).UnixNano() }},
		{"missing_time", func(r *model.AutoModelScore) { r.ObservedAt = 0 }},
		{"version", func(r *model.AutoModelScore) { r.Version = 2 }},
		{"scope", func(r *model.AutoModelScore) { r.Scope = "global" }},
		{"empty_group", func(r *model.AutoModelScore) { r.Group = "" }},
		{"group_separator", func(r *model.AutoModelScore) { r.Group = "g\x00x" }},
		{"padded_group", func(r *model.AutoModelScore) { r.Group = " g" }},
		{"empty_model", func(r *model.AutoModelScore) { r.Model = "" }},
		{"virtual_model", func(r *model.AutoModelScore) { r.Model = "auto" }},
		{"model_separator", func(r *model.AutoModelScore) { r.Model = "a\x00x" }},
		{"padded_model", func(r *model.AutoModelScore) { r.Model = "alpha " }},
		{"channel_model", func(r *model.AutoModelScore) { r.Scope = "channel" }},
		{"nonpositive_channel", func(r *model.AutoModelScore) { r.ChannelID = 0 }},
		{"negative_score", func(r *model.AutoModelScore) { r.Score = -0.1 }},
		{"high_score", func(r *model.AutoModelScore) { r.Score = 1.1 }},
		{"infinite_score", func(r *model.AutoModelScore) { r.Score = math.Inf(1) }},
		{"negative_latency", func(r *model.AutoModelScore) { r.LatencyMS = -1 }},
		{"infinite_latency", func(r *model.AutoModelScore) { r.LatencyMS = math.Inf(1) }},
		{"negative_observations", func(r *model.AutoModelScore) { r.Observations = -1 }},
		{"no_observations", func(r *model.AutoModelScore) { r.Observations = 0 }},
		{"excess_observations", func(r *model.AutoModelScore) { r.Observations = 21 }},
		{"infinite_observations", func(r *model.AutoModelScore) { r.Observations = math.Inf(1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupAutoModelScorePersistenceTestDB(t)
			row := fresh
			tc.mutate(&row)
			require.NoError(t, db.Create(&row).Error)
			p := &autoModelScorePersistence{db: db}
			require.NoError(t, p.restore(context.Background()))
			require.Zero(t, autoModelHealth.Len(), "invalid stored model evidence must not enter online scores")
			require.Zero(t, autoModelChannelHealth.Len(), "invalid stored channel evidence must not enter online scores")
		})
	}
	t.Run("valid_keeps_raw_age", func(t *testing.T) {
		db := setupAutoModelScorePersistenceTestDB(t)
		channel := fresh
		channel.Scope, channel.Model = "channel", ""
		require.NoError(t, db.Create(&[]model.AutoModelScore{fresh, channel}).Error)
		p := &autoModelScorePersistence{db: db}
		require.NoError(t, p.restore(context.Background()))
		got, ok := autoModelHealth.Get(autoModelHealthKey("g", "alpha", 1))
		require.True(t, ok)
		expected := autoModelOutcome{Score: fresh.Score, LatencyMS: fresh.LatencyMS, Observations: fresh.Observations, UpdatedAt: time.Unix(0, fresh.ObservedAt)}
		require.Equal(t, expected, got)
		require.Equal(t, effectiveScore(expected, now), effectiveScore(got, now), "restored raw evidence must undergo the original single aging fold")
		require.Equal(t, 0.5, effectiveScore(got, now.Add(window)), "窗口到期后评分恰好回到中立")
	})
	t.Run("nonfinite_and_exact_boundaries", func(t *testing.T) {
		require.True(t, validAutoModelScore(fresh, now))
		for _, mutate := range []func(*model.AutoModelScore){
			func(r *model.AutoModelScore) { r.Score = math.NaN() },
			func(r *model.AutoModelScore) { r.LatencyMS = math.NaN() },
			func(r *model.AutoModelScore) { r.Observations = math.NaN() },
			func(r *model.AutoModelScore) { r.ObservedAt = now.Add(-window - time.Minute).UnixNano() },
			func(r *model.AutoModelScore) { r.ObservedAt = now.Add(time.Nanosecond).UnixNano() },
		} {
			row := fresh
			mutate(&row)
			require.False(t, validAutoModelScore(row, now))
		}
		fresh.ObservedAt = now.Add(-window + time.Nanosecond).UnixNano()
		require.True(t, validAutoModelScore(fresh, now))
	})
}

func TestAutoModelScoreRejectedFutureSnapshotCannotSuppressDurableFeedback(t *testing.T) {
	for _, scope := range []string{model.AutoModelScoreScopeModel, model.AutoModelScoreScopeChannel} {
		t.Run(scope, func(t *testing.T) {
			db := setupAutoModelScorePersistenceTestDB(t)
			future := model.AutoModelScore{Scope: scope, Group: "g", ChannelID: 1, Score: 0.1, LatencyMS: 999, Observations: 8, ObservedAt: time.Now().Add(time.Hour).UnixNano(), Version: 1}
			if scope == model.AutoModelScoreScopeModel {
				future.Model = "alpha"
			}
			require.NoError(t, db.Create(&future).Error)
			p := &autoModelScorePersistence{db: db}
			require.NoError(t, p.restore(context.Background()))
			require.Empty(t, autoModelHealth.ReadAll(), "future model evidence must be rejected")
			require.Empty(t, autoModelChannelHealth.ReadAll(), "future channel evidence must be rejected")
			RecordAutoModelOutcome("g", "alpha", 1, true, 123, 0)
			wantModels, wantChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
			require.NoError(t, p.flush(context.Background()))
			rows, err := model.LoadAutoModelScores(context.Background(), db)
			require.NoError(t, err)
			t.Logf("rejected_snapshot=%+v; after_feedback_flush=%+v", future, rows)
			autoModelHealth.Clear()
			autoModelChannelHealth.Clear()
			restored := &autoModelScorePersistence{db: db}
			require.NoError(t, restored.restore(context.Background()))
			if scope == model.AutoModelScoreScopeModel {
				require.Contains(t, autoModelHealth.ReadAll(), autoModelHealthKey("g", "alpha", 1), "rejected future model snapshot must not suppress durable real feedback")
			} else {
				require.Contains(t, autoModelChannelHealth.ReadAll(), autoModelChannelHealthKey("g", 1), "rejected future channel snapshot must not suppress durable real feedback")
			}
			assertAutoModelScoreOutcomes(t, wantModels, autoModelHealth.ReadAll())
			assertAutoModelScoreOutcomes(t, wantChannels, autoModelChannelHealth.ReadAll())
		})
	}
}

func TestAutoModelScoreWorkerRetriesFailedFutureCleanupWithoutNewFeedback(t *testing.T) {
	for _, scope := range []string{model.AutoModelScoreScopeModel, model.AutoModelScoreScopeChannel} {
		t.Run(scope, func(t *testing.T) {
			db := setupAutoModelScorePersistenceTestDB(t)
			future := model.AutoModelScore{Scope: scope, Group: "g", ChannelID: 1, Score: 0.1, LatencyMS: 999, Observations: 8, ObservedAt: time.Now().Add(time.Hour).UnixNano(), Version: 1}
			if scope == model.AutoModelScoreScopeModel {
				future.Model = "alpha"
			}
			require.NoError(t, db.Create(&future).Error)
			require.NoError(t, db.Exec(`CREATE TRIGGER fail_future_cleanup BEFORE DELETE ON auto_model_scores
				BEGIN SELECT RAISE(ABORT, 'future cleanup unavailable'); END`).Error)
			p := &autoModelScorePersistence{db: db}
			require.ErrorContains(t, p.restore(context.Background()), "future cleanup unavailable")
			require.False(t, p.loaded, "failed cleanup must keep restore retryable")
			var writes atomic.Int64
			require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:failed_future_cleanup_write", func(tx *gorm.DB) {
				if tx.Statement.Table == "auto_model_scores" && tx.Error == nil {
					writes.Add(1)
				}
			}))
			stop := startAutoModelScorePersistence(db, 20*time.Millisecond, 100*time.Millisecond)
			t.Cleanup(stop)
			RecordAutoModelOutcome("g", "alpha", 1, true, 123, 0)
			wantModels, wantChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
			require.Eventually(t, func() bool { return writes.Load() > 0 }, time.Second, time.Millisecond,
				"positive control must observe a real save attempt while cleanup is unavailable")
			rows, err := model.LoadAutoModelScores(context.Background(), db)
			require.NoError(t, err)
			require.Contains(t, rows, future, "the rejected future snapshot must still block this key before cleanup recovers")
			require.NoError(t, db.Exec("DROP TRIGGER fail_future_cleanup").Error)
			wantObservedAt := wantModels[autoModelHealthKey("g", "alpha", 1)].UpdatedAt.UnixNano()
			require.Eventually(t, func() bool {
				stored, loadErr := model.LoadAutoModelScores(context.Background(), db)
				if loadErr != nil || len(stored) != 2 {
					return false
				}
				for _, row := range stored {
					if row.ObservedAt != wantObservedAt {
						return false
					}
				}
				return true
			}, time.Second, 5*time.Millisecond,
				"cleanup retry must persist unchanged feedback instead of leaving an acknowledged observation absent")
			stop()
			assertAutoModelScoreRow(t, db, "model", "g", "alpha", 1, wantModels[autoModelHealthKey("g", "alpha", 1)])
			assertAutoModelScoreRow(t, db, "channel", "g", "", 1, wantChannels[autoModelChannelHealthKey("g", 1)])
			autoModelHealth.Clear()
			autoModelChannelHealth.Clear()
			reloaded := &autoModelScorePersistence{db: db}
			require.NoError(t, reloaded.restore(context.Background()))
			assertAutoModelScoreOutcomes(t, wantModels, autoModelHealth.ReadAll())
			assertAutoModelScoreOutcomes(t, wantChannels, autoModelChannelHealth.ReadAll())
		})
	}
}

func TestAutoModelScoreWorkerRecoversAfterExternalFutureCleanupWithoutNewFeedback(t *testing.T) {
	for _, scope := range []string{model.AutoModelScoreScopeModel, model.AutoModelScoreScopeChannel} {
		for _, recovery := range []string{"worker_tick", "shutdown"} {
			t.Run(scope+"/"+recovery, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "external-cleanup.db")
				db := setupAutoModelScorePersistenceTestDB(t, path)
				external, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
				require.NoError(t, err)
				externalSQL, err := external.DB()
				require.NoError(t, err)
				externalSQL.SetMaxOpenConns(1)
				t.Cleanup(func() { require.NoError(t, externalSQL.Close()) })
				ctx := context.Background()
				future := model.AutoModelScore{Scope: scope, Group: "g", ChannelID: 1, Score: 0.1, LatencyMS: 999, Observations: 8, ObservedAt: time.Now().Add(time.Hour).UnixNano(), Version: 1}
				if scope == model.AutoModelScoreScopeModel {
					future.Model = "alpha"
				}
				require.NoError(t, db.Create(&future).Error)
				require.NoError(t, db.Exec(`CREATE TRIGGER fail_external_future_cleanup BEFORE DELETE ON auto_model_scores
					BEGIN SELECT RAISE(ABORT, 'future cleanup unavailable'); END`).Error)

				var reads, writes, writtenRows, cleanupFailures atomic.Int64
				entered, release := make(chan struct{}), make(chan struct{})
				var blockOnce, releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				defer unblock()
				require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:external_cleanup_write", func(tx *gorm.DB) {
					if tx.Statement.Table == "auto_model_scores" && tx.Error == nil {
						writes.Add(1)
						writtenRows.Add(int64(tx.Statement.ReflectValue.Len()))
					}
				}))
				require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("test:external_cleanup_failure", func(tx *gorm.DB) {
					if tx.Statement.Table == "auto_model_scores" && tx.Error != nil {
						cleanupFailures.Add(1)
					}
				}))
				require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:external_cleanup_load_barrier", func(tx *gorm.DB) {
					if tx.Statement.Table == "auto_model_scores" {
						reads.Add(1)
						if writes.Load() > 0 {
							// The previous worker flush has returned and acknowledged
							// its batch. Hold the next restore before reading SQL.
							blockOnce.Do(func() { close(entered); <-release })
						}
					}
				}))
				stop := startAutoModelScorePersistence(db, 20*time.Millisecond, time.Second)
				t.Cleanup(stop)
				require.EqualValues(t, 1, cleanupFailures.Load(), "startup must actually fail to retire the future snapshot")
				require.Empty(t, autoModelHealth.ReadAll())
				require.Empty(t, autoModelChannelHealth.ReadAll())
				RecordAutoModelOutcome("g", "alpha", 1, true, 123, 0)
				wantModels, wantChannels := autoModelHealth.ReadAll(), autoModelChannelHealth.ReadAll()
				wantRows := snapshotAutoModelScores()
				require.Len(t, wantRows, 2)
				var other model.AutoModelScore
				for _, row := range wantRows {
					if row.Scope != scope {
						other = row
					}
				}
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("worker must acknowledge a real flush before the next restore is blocked")
				}
				readsAtRecovery := reads.Load()
				if !t.Run("timestamp_guard_and_other_scope_positive_control", func(t *testing.T) {
					require.EqualValues(t, 1, writes.Load())
					require.GreaterOrEqual(t, cleanupFailures.Load(), int64(2))
					rows, err := model.LoadAutoModelScores(ctx, external)
					require.NoError(t, err)
					require.ElementsMatch(t, []model.AutoModelScore{future, other}, rows,
						"the acknowledged flush must leave the future key unchanged but durably save the other scope")
				}) {
					return
				}
				// Another instance cleans up BEFORE the retry reads SQL. The
				// worker cannot invalidate this key via its own cleanup branch.
				require.NoError(t, external.Exec("DROP TRIGGER fail_external_future_cleanup").Error)
				require.NoError(t, model.DeleteAutoModelScoreSnapshot(ctx, external, future))
				rows, err := model.LoadAutoModelScores(ctx, external)
				require.NoError(t, err)
				require.Equal(t, []model.AutoModelScore{other}, rows, "independent cleanup must preserve the durable other scope")
				t.Logf("external_cleanup_before_retry=%+v; unchanged_feedback=%+v", rows, wantRows)
				unblock()
				if recovery == "worker_tick" {
					require.Eventually(t, func() bool { return writes.Load() == 2 }, time.Second, time.Millisecond,
						"L1: a successful restore after independent future cleanup must retry unchanged feedback before shutdown")
					<-time.After(60 * time.Millisecond)
				}
				stop()
				rows, err = model.LoadAutoModelScores(ctx, external)
				require.NoError(t, err)
				t.Logf("after_recovery_and_stop=%+v", rows)
				require.ElementsMatch(t, wantRows, rows,
					"L1: successful restore and shutdown after independent future cleanup must persist unchanged feedback with its exact raw timestamp")
				require.EqualValues(t, 2, writes.Load(), "recovery and idle shutdown must not rewrite already durable observations")
				require.EqualValues(t, 3, writtenRows.Load(), "only the missing scope is dirty after recovery")
				require.Equal(t, readsAtRecovery, reads.Load(), "successful recovery must not reload on idle ticks or shutdown")
				assertAutoModelScoreOutcomes(t, wantModels, autoModelHealth.ReadAll())
				assertAutoModelScoreOutcomes(t, wantChannels, autoModelChannelHealth.ReadAll())
				autoModelHealth.Clear()
				autoModelChannelHealth.Clear()
				reloaded := &autoModelScorePersistence{db: external}
				require.NoError(t, reloaded.restore(ctx))
				assertAutoModelScoreOutcomes(t, wantModels, autoModelHealth.ReadAll())
				assertAutoModelScoreOutcomes(t, wantChannels, autoModelChannelHealth.ReadAll())
			})
		}
	}
}

func TestAutoModelScoreStopPersistsBothRawFeedbackLayers(t *testing.T) {
	db := setupAutoModelScorePersistenceTestDB(t)
	stop := StartAutoModelScorePersistence()
	t.Cleanup(stop)
	RecordAutoModelOutcome("g", "alpha", 1, true, 100, 0)
	RecordAutoModelOutcome("g", "beta", 1, true, 900, 0)
	RecordAutoModelOutcome("vip", "alpha", 1, true, 250, 0)
	RecordAutoModelOutcome("g", "alpha", 2, true, 50, 0)
	before, err := model.LoadAutoModelScores(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, before, "score feedback only mutates memory, not SQL")
	stop()
	rows, err := model.LoadAutoModelScores(context.Background(), db)
	require.NoError(t, err)
	require.Len(t, rows, 7, "four model keys and three independently aggregated channel keys must persist")
	for _, key := range []struct {
		group, name string
		channelID   int
	}{
		{"g", "alpha", 1}, {"g", "beta", 1}, {"vip", "alpha", 1}, {"g", "alpha", 2},
	} {
		outcome, ok := autoModelHealth.Get(autoModelHealthKey(key.group, key.name, key.channelID))
		require.True(t, ok)
		assertAutoModelScoreRow(t, db, "model", key.group, key.name, key.channelID, outcome)
	}
	for _, key := range []struct {
		group     string
		channelID int
	}{{"g", 1}, {"vip", 1}, {"g", 2}} {
		outcome, ok := autoModelChannelHealth.Get(autoModelChannelHealthKey(key.group, key.channelID))
		require.True(t, ok)
		assertAutoModelScoreRow(t, db, "channel", key.group, "", key.channelID, outcome)
	}
}
