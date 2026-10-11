package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func newAutoModelScoreTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	return db
}

func TestAutoModelScoreMigrationRegistersTable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		migrate func() error
	}{
		{"normal", migrateDB},
		{"fast", migrateDBFast},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newAutoModelScoreTestDB(t)
			oldDB := DB
			oldSQLite, oldMySQL, oldPostgres := common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL
			DB = db
			common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = true, false, false
			t.Cleanup(func() {
				DB = oldDB
				common.UsingSQLite, common.UsingMySQL, common.UsingPostgreSQL = oldSQLite, oldMySQL, oldPostgres
			})
			require.NoError(t, tc.migrate())
			require.True(t, db.Migrator().HasTable("auto_model_scores"), "score snapshot table must be registered in the real migration path")
			columns, err := db.Migrator().ColumnTypes("auto_model_scores")
			require.NoError(t, err)
			var names []string
			for _, column := range columns {
				names = append(names, column.Name())
			}
			require.ElementsMatch(t, []string{"scope", "group", "model", "channel_id", "score", "latency_ms", "observations", "observed_at", "version"}, names)
		})
	}
}

func TestAutoModelScoreRoundTripPreservesScopesAndRawFields(t *testing.T) {
	db := newAutoModelScoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observed := time.Now().Add(-time.Minute).UnixNano()
	want := []AutoModelScore{
		{Scope: "model", Group: "g", Model: "m", ChannelID: 1, Score: 0.85, LatencyMS: 123.5, Observations: 3.75, ObservedAt: observed, Version: 1},
		{Scope: "model", Group: "vip", Model: "m", ChannelID: 1, Score: 0.15, LatencyMS: 432.125, Observations: 8.5, ObservedAt: observed + 1, Version: 1},
		{Scope: "model", Group: "g", Model: "other", ChannelID: 1, Score: 0, LatencyMS: 0, Observations: 1, ObservedAt: observed + 2, Version: 1},
		{Scope: "model", Group: "g", Model: "m", ChannelID: 2, Score: 1, LatencyMS: 2, Observations: 20, ObservedAt: observed + 3, Version: 1},
		{Scope: "channel", Group: "g", Model: "", ChannelID: 1, Score: 0.4, LatencyMS: 321.25, Observations: 12.25, ObservedAt: observed + 4, Version: 1},
		{Scope: "channel", Group: "vip", Model: "", ChannelID: 1, Score: 0.2, LatencyMS: 900, Observations: 2.75, ObservedAt: observed + 5, Version: 1},
	}
	require.NoError(t, SaveAutoModelScores(ctx, db, want))
	got, err := LoadAutoModelScores(ctx, db)
	require.NoError(t, err)
	require.ElementsMatch(t, want, got, "all raw fields and both full key scopes must survive SQL round-trip without re-timestamping")
}

func TestAutoModelScoreDatabaseFailuresAreReturned(t *testing.T) {
	row := AutoModelScore{Scope: "model", Group: "g", Model: "m", ChannelID: 1, Observations: 1, ObservedAt: time.Now().UnixNano(), Version: 1}
	operations := map[string]func(context.Context, *gorm.DB) error{
		"save": func(ctx context.Context, db *gorm.DB) error {
			return SaveAutoModelScores(ctx, db, []AutoModelScore{row})
		},
		"load":  func(ctx context.Context, db *gorm.DB) error { _, err := LoadAutoModelScores(ctx, db); return err },
		"prune": func(ctx context.Context, db *gorm.DB) error { return PruneAutoModelScores(ctx, db, row.ObservedAt) },
		"delete_snapshot": func(ctx context.Context, db *gorm.DB) error {
			return DeleteAutoModelScoreSnapshot(ctx, db, row)
		},
	}
	for name, operation := range operations {
		t.Run(name+"/nil", func(t *testing.T) {
			require.NotPanics(t, func() {
				require.Error(t, operation(context.Background(), nil), "nil DB must return an error, not silently acknowledge persistence")
			})
		})
		t.Run(name+"/missing_table", func(t *testing.T) {
			require.Error(t, operation(context.Background(), newAutoModelScoreTestDB(t)))
		})
		t.Run(name+"/bounded_pool_wait", func(t *testing.T) {
			db := newAutoModelScoreTestDB(t)
			require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
			sqlDB, err := db.DB()
			require.NoError(t, err)
			conn, err := sqlDB.Conn(context.Background())
			require.NoError(t, err)
			defer conn.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			start := time.Now()
			err = operation(ctx, db)
			require.ErrorIs(t, err, context.DeadlineExceeded, "an exhausted real SQL connection pool must obey the caller's deadline")
			require.Less(t, time.Since(start), time.Second)
		})
	}
}

func TestAutoModelScorePruneExpiresOnlyOldSnapshots(t *testing.T) {
	db := newAutoModelScoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
	ctx := context.Background()
	cutoff := time.Now().Add(-10 * time.Minute).UnixNano()
	rows := []AutoModelScore{
		{Scope: "model", Group: "g", Model: "old", ChannelID: 1, ObservedAt: cutoff - 1, Version: 1},
		{Scope: "channel", Group: "g", ChannelID: 1, ObservedAt: cutoff, Version: 1},
		{Scope: "model", Group: "other", Model: "fresh", ChannelID: 1, ObservedAt: cutoff + 1, Version: 1},
		{Scope: "channel", Group: "other", ChannelID: 1, ObservedAt: cutoff + 2, Version: 1},
	}
	require.NoError(t, SaveAutoModelScores(ctx, db, rows))
	require.NoError(t, PruneAutoModelScores(ctx, db, cutoff))
	got, err := LoadAutoModelScores(ctx, db)
	require.NoError(t, err)
	require.ElementsMatch(t, rows[2:], got, "expiration must prune both scopes without replacing the table or deleting a newer key")
}

func TestAutoModelScoreBatchFailureRollsBackBothScopes(t *testing.T) {
	db := newAutoModelScoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
	ctx := context.Background()
	original := AutoModelScore{Scope: "model", Group: "g", Model: "m", ChannelID: 1, Score: 0.8, Observations: 4, ObservedAt: time.Now().UnixNano(), Version: 1}
	require.NoError(t, SaveAutoModelScores(ctx, db, []AutoModelScore{original}))
	// The second SQL chunk genuinely fails after the first chunk has executed.
	// The store must own atomicity even when the caller disables GORM's default
	// per-statement transactions; updating an existing row must roll back too.
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_channel_score BEFORE INSERT ON auto_model_scores
		WHEN NEW.scope = 'channel' BEGIN SELECT RAISE(ABORT, 'reject captured batch'); END`).Error)
	batch := make([]AutoModelScore, 0, 102)
	for id := 1; id <= 101; id++ {
		row := original
		row.ChannelID, row.Score, row.ObservedAt = id, 0.1, original.ObservedAt+1
		batch = append(batch, row)
	}
	channel := original
	channel.Scope, channel.Model = "channel", ""
	batch = append(batch, channel)
	err := SaveAutoModelScores(ctx, db.Session(&gorm.Session{SkipDefaultTransaction: true, CreateBatchSize: 100}), batch)
	require.ErrorContains(t, err, "reject captured batch")
	got, err := LoadAutoModelScores(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []AutoModelScore{original}, got, "a captured model/channel batch must roll back every earlier chunk on failure")
}

// Server dialects construct real GORM SQL against a local SQLite connection
// only. This is deliberately not represented as live MySQL/PostgreSQL coverage.
func TestAutoModelScoreSQLDialectsGuardEveryUpdatedField(t *testing.T) {
	base := newAutoModelScoreTestDB(t)
	sqlDB, err := base.DB()
	require.NoError(t, err)
	for _, dialect := range []gorm.Dialector{
		sqlite.Dialector{Conn: sqlDB},
		mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}),
		postgres.New(postgres.Config{Conn: sqlDB}),
	} {
		t.Run(dialect.Name(), func(t *testing.T) {
			db, err := gorm.Open(dialect, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
			require.NoError(t, err)
			var statements []string
			var variables [][]any
			require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:score_dialect", func(tx *gorm.DB) {
				statements = append(statements, tx.Statement.SQL.String())
				variables = append(variables, append([]any(nil), tx.Statement.Vars...))
			}))
			row := AutoModelScore{Scope: "model", Group: "g' OR 1=1 --", Model: "模型'", ChannelID: 4, Score: 0.8, LatencyMS: 123, Observations: 7.5, ObservedAt: time.Now().UnixNano(), Version: 1}
			require.NoError(t, SaveAutoModelScores(context.Background(), db, []AutoModelScore{row}))
			require.Len(t, statements, 1)
			sql := statements[0]
			t.Logf("%s SQL: %s; parameter_count=%d", dialect.Name(), sql, len(variables[0]))
			require.NotContains(t, sql, row.Group)
			require.NotContains(t, sql, row.Model)
			require.Equal(t, []any{row.Scope, row.Group, row.Model, row.ChannelID, row.Score, row.LatencyMS, row.Observations, row.ObservedAt, row.Version}, variables[0])
			if dialect.Name() == "mysql" {
				require.Contains(t, sql, "ON DUPLICATE KEY UPDATE")
				for _, field := range []string{"score", "latency_ms", "observations", "version", "observed_at"} {
					require.Contains(t, sql, "`"+field+"`=CASE WHEN `observed_at` < VALUES(`observed_at`) THEN VALUES(`"+field+"`) ELSE `"+field+"` END", "MySQL ignores OnConflict.Where; each field must have its own stale-write guard")
				}
				require.True(t, strings.HasSuffix(sql, "`observed_at`=CASE WHEN `observed_at` < VALUES(`observed_at`) THEN VALUES(`observed_at`) ELSE `observed_at` END"), "MySQL must update observed_at last because assignments run left-to-right")
			} else {
				quote := "`"
				if dialect.Name() == "postgres" {
					quote = `"`
				}
				q := func(s string) string { return quote + s + quote }
				require.Contains(t, sql, "ON CONFLICT ("+q("scope")+","+q("group")+","+q("model")+","+q("channel_id")+") DO UPDATE SET")
				require.Contains(t, sql, "WHERE "+q("auto_model_scores")+"."+q("observed_at")+" < "+q("excluded")+"."+q("observed_at"))
			}
			statement := &gorm.Statement{DB: db}
			require.NoError(t, statement.Parse(&AutoModelScore{}))
			observed := statement.Schema.LookUpField("ObservedAt")
			require.Zero(t, observed.AutoCreateTime)
			require.Zero(t, observed.AutoUpdateTime)
			require.Contains(t, strings.ToLower(db.Migrator().FullDataTypeOf(observed).SQL), "int")
		})
	}
}

func TestAutoModelScoreDeleteSnapshotMatchesFullKey(t *testing.T) {
	for _, scope := range []string{AutoModelScoreScopeModel, AutoModelScoreScopeChannel} {
		t.Run(scope, func(t *testing.T) {
			db := newAutoModelScoreTestDB(t)
			require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
			row := AutoModelScore{Scope: scope, Group: "g", ChannelID: 1, Score: 0.1, Observations: 8, ObservedAt: time.Now().Add(time.Hour).UnixNano(), Version: 1}
			if scope == AutoModelScoreScopeModel {
				row.Model = "alpha"
			}
			var untouched []AutoModelScore
			for _, mutate := range []func(*AutoModelScore){
				func(r *AutoModelScore) { r.Group = "other" },
				func(r *AutoModelScore) { r.Model = "sibling" },
				func(r *AutoModelScore) { r.ChannelID = 2 },
				func(r *AutoModelScore) { r.Scope = "other" },
			} {
				other := row
				mutate(&other)
				untouched = append(untouched, other)
			}
			require.NoError(t, db.Create(&row).Error)
			require.NoError(t, db.Create(&untouched).Error)
			require.NoError(t, DeleteAutoModelScoreSnapshot(context.Background(), db, row))
			got, err := LoadAutoModelScores(context.Background(), db)
			require.NoError(t, err)
			require.ElementsMatch(t, untouched, got, "retire only the rejected full key, including an empty channel model")
			require.NoError(t, DeleteAutoModelScoreSnapshot(context.Background(), db, row), "an already-retired snapshot is an idempotent no-op")
		})
	}
}

func TestAutoModelScoreDeleteSnapshotPreservesReplacement(t *testing.T) {
	for _, scope := range []string{AutoModelScoreScopeModel, AutoModelScoreScopeChannel} {
		for _, change := range []string{"newer_observation", "corrected_clock", "new_version"} {
			t.Run(scope+"/"+change, func(t *testing.T) {
				db := newAutoModelScoreTestDB(t)
				require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
				read := AutoModelScore{Scope: scope, Group: "g", ChannelID: 1, Score: 0.1, Observations: 8, ObservedAt: time.Now().Add(time.Hour).UnixNano(), Version: 1}
				if scope == AutoModelScoreScopeModel {
					read.Model = "alpha"
				}
				require.NoError(t, db.Create(&read).Error)
				stale := read
				replacement := read
				writeTarget := read // GORM Update mutates its Model argument.
				switch change {
				case "newer_observation":
					replacement.ObservedAt++
					require.NoError(t, SaveAutoModelScores(context.Background(), db, []AutoModelScore{replacement}))
				case "corrected_clock":
					replacement.ObservedAt = time.Now().UnixNano()
					require.NoError(t, db.Model(&writeTarget).Update("observed_at", replacement.ObservedAt).Error)
				case "new_version":
					replacement.Version++
					require.NoError(t, db.Model(&writeTarget).Update("version", replacement.Version).Error)
				}
				// The saved snapshot was replaced after it was read but before cleanup.
				require.Equal(t, stale, read, "fixture must retain the original read snapshot before conditional deletion")
				require.NoError(t, DeleteAutoModelScoreSnapshot(context.Background(), db, read))
				got, err := LoadAutoModelScores(context.Background(), db)
				require.NoError(t, err)
				require.Equal(t, []AutoModelScore{replacement}, got, "conditional cleanup must not delete a concurrently replaced observation/version")
			})
		}
	}
}

// Generate server-dialect SQL using only a local SQLite connection, never a server.
func TestAutoModelScoreDeleteSnapshotSQLDialects(t *testing.T) {
	base := newAutoModelScoreTestDB(t)
	sqlDB, err := base.DB()
	require.NoError(t, err)
	for _, dialect := range []gorm.Dialector{
		sqlite.Dialector{Conn: sqlDB},
		mysql.New(mysql.Config{Conn: sqlDB, SkipInitializeWithVersion: true}),
		postgres.New(postgres.Config{Conn: sqlDB}),
	} {
		for _, scope := range []string{AutoModelScoreScopeModel, AutoModelScoreScopeChannel} {
			t.Run(dialect.Name()+"/"+scope, func(t *testing.T) {
				db, err := gorm.Open(dialect, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
				require.NoError(t, err)
				var statements []string
				var variables [][]any
				require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("test:score_delete_dialect", func(tx *gorm.DB) {
					statements = append(statements, tx.Statement.SQL.String())
					variables = append(variables, append([]any(nil), tx.Statement.Vars...))
				}))
				row := AutoModelScore{Scope: scope, Group: "g' OR 1=1 --", ChannelID: 4, ObservedAt: time.Now().Add(time.Hour).UnixNano(), Version: 0}
				if scope == AutoModelScoreScopeModel {
					row.Model = "模型'"
				}
				require.NoError(t, DeleteAutoModelScoreSnapshot(context.Background(), db, row))
				require.Len(t, statements, 1, "retiring a rejected snapshot must execute a conditional SQL delete")
				sql := statements[0]
				t.Logf("%s %s SQL: %s; parameters=%v", dialect.Name(), scope, sql, variables[0])
				quote := "`"
				if dialect.Name() == "postgres" {
					quote = `"`
				}
				q := func(s string) string { return quote + s + quote }
				require.True(t, strings.HasPrefix(sql, "DELETE FROM "+q("auto_model_scores")+" WHERE "))
				for _, field := range []string{"channel_id", "group", "model", "observed_at", "scope", "version"} {
					require.Contains(t, sql, q(field)+" = ", "every identity field must be compared, including zero/empty values")
				}
				require.NotContains(t, sql, row.Group)
				require.Equal(t, []any{row.ChannelID, row.Group, row.Model, row.ObservedAt, row.Scope, row.Version}, variables[0])
			})
		}
	}
}

func TestAutoModelScoreUpsertRejectsOlderOrEqualObservation(t *testing.T) {
	for _, scope := range []string{"model", "channel"} {
		t.Run(scope, func(t *testing.T) {
			db := newAutoModelScoreTestDB(t)
			require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
			ctx := context.Background()
			old := AutoModelScore{Scope: scope, Group: "g", ChannelID: 1, Score: 0.7, LatencyMS: 200, Observations: 4, ObservedAt: time.Now().Add(-time.Minute).UnixNano(), Version: 1}
			if scope == "model" {
				old.Model = "m"
			}
			unrelated := old
			unrelated.Group = "untouched"
			require.NoError(t, SaveAutoModelScores(ctx, db, []AutoModelScore{old, unrelated}))
			fresh := old
			fresh.Score, fresh.LatencyMS, fresh.Observations, fresh.ObservedAt = 0, 0, 5.5, old.ObservedAt+1
			require.NoError(t, SaveAutoModelScores(ctx, db, []AutoModelScore{fresh}), "a newer snapshot must replace all numeric fields, including zeros")
			require.NoError(t, SaveAutoModelScores(ctx, db, []AutoModelScore{old}), "an older batch is a non-destructive no-op")
			equalTime := old
			equalTime.ObservedAt = fresh.ObservedAt
			require.NoError(t, SaveAutoModelScores(ctx, db, []AutoModelScore{equalTime}))
			got, err := LoadAutoModelScores(ctx, db)
			require.NoError(t, err)
			require.ElementsMatch(t, []AutoModelScore{fresh, unrelated}, got, "stale/equal snapshots must not replace later evidence or unrelated keys")
		})
	}
}
