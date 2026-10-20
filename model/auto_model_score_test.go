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
			require.ElementsMatch(t, []string{"scope", "group", "model", "channel_id", "score", "latency_ms", "observations", "observed_at", "observation_seq", "writer_id", "version"}, names, "排序元数据必须随 GORM 跨库增列一起迁移")
		})
	}
}

func TestAutoModelScoreRoundTripPreservesRawFieldsAndSkipsLegacyScope(t *testing.T) {
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
	}
	legacy := []AutoModelScore{
		{Scope: "channel", Group: "g", Model: "", ChannelID: 1, Score: 0.4, LatencyMS: 321.25, Observations: 12.25, ObservedAt: observed + 4, Version: 1},
		{Scope: "channel", Group: "vip", Model: "", ChannelID: 1, Score: 0.2, LatencyMS: 900, Observations: 2.75, ObservedAt: observed + 5, Version: 1},
	}
	outcomes, err := SaveAutoModelScores(ctx, db, append(append([]AutoModelScore(nil), want...), legacy...))
	require.NoError(t, err)
	require.Equal(t, []AutoModelScoreSaveOutcome{
		AutoModelScoreStored, AutoModelScoreStored, AutoModelScoreStored, AutoModelScoreStored,
		AutoModelScoreStored, AutoModelScoreStored,
	}, outcomes, "每一条都必须单独确认,批量 RowsAffected 不能代替逐行结论")
	got, err := LoadAutoModelScores(ctx, db)
	require.NoError(t, err)
	require.ElementsMatch(t, want, got, "all raw fields must survive SQL round-trip without re-timestamping, while legacy scope=channel rows stay invisible to reads")
}

func TestAutoModelScoreDatabaseFailuresAreReturned(t *testing.T) {
	row := AutoModelScore{Scope: "model", Group: "g", Model: "m", ChannelID: 1, Observations: 1, ObservedAt: time.Now().UnixNano(), Version: 1}
	operations := map[string]func(context.Context, *gorm.DB) error{
		"save": func(ctx context.Context, db *gorm.DB) error {
			_, err := SaveAutoModelScores(ctx, db, []AutoModelScore{row})
			return err
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
	_, err := SaveAutoModelScores(ctx, db, rows)
	require.NoError(t, err)
	require.NoError(t, PruneAutoModelScores(ctx, db, cutoff))
	got, err := LoadAutoModelScores(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []AutoModelScore{rows[2]}, got, "the newer model snapshot survives and stays readable")
	var remaining int64
	require.NoError(t, db.Model(&AutoModelScore{}).Count(&remaining).Error)
	require.EqualValues(t, 2, remaining, "expiration must prune stale rows of every scope without deleting a newer key")
}

func TestAutoModelScoreBatchFailureRollsBackEntireBatch(t *testing.T) {
	db := newAutoModelScoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
	ctx := context.Background()
	original := AutoModelScore{Scope: "model", Group: "g", Model: "m", ChannelID: 1, Score: 0.8, Observations: 4, ObservedAt: time.Now().UnixNano(), Version: 1}
	_, err := SaveAutoModelScores(ctx, db, []AutoModelScore{original})
	require.NoError(t, err)
	// The second SQL chunk genuinely fails after the first chunk has executed.
	// The store must own atomicity even when the caller disables GORM's default
	// per-statement transactions; updating an existing row must roll back too.
	require.NoError(t, db.Exec(`CREATE TRIGGER reject_last_row BEFORE INSERT ON auto_model_scores
		WHEN NEW.channel_id = 999 BEGIN SELECT RAISE(ABORT, 'reject captured batch'); END`).Error)
	batch := make([]AutoModelScore, 0, 102)
	for id := 1; id <= 101; id++ {
		row := original
		row.ChannelID, row.Score, row.ObservedAt = id, 0.1, original.ObservedAt+1
		batch = append(batch, row)
	}
	rejected := original
	rejected.ChannelID, rejected.ObservedAt = 999, original.ObservedAt+1
	batch = append(batch, rejected)
	_, err = SaveAutoModelScores(ctx, db.Session(&gorm.Session{SkipDefaultTransaction: true, CreateBatchSize: 100}), batch)
	require.ErrorContains(t, err, "reject captured batch")
	got, err := LoadAutoModelScores(ctx, db)
	require.NoError(t, err)
	require.Equal(t, []AutoModelScore{original}, got, "a captured batch must roll back every earlier chunk on failure")
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
			row := AutoModelScore{Scope: "model", Group: "g' OR 1=1 --", Model: "模型'", ChannelID: 4, Score: 0.8, LatencyMS: 123, Observations: 7.5, ObservedAt: time.Now().UnixNano(), ObservationSeq: 3, WriterID: "writer-a", Version: 1}
			outcomes, saveErr := SaveAutoModelScores(context.Background(), db, []AutoModelScore{row})
			require.NoError(t, saveErr)
			require.Nil(t, outcomes, "dry-run 只生成 SQL,不回读逐行确认")
			require.Len(t, statements, 1)
			sql := statements[0]
			t.Logf("%s SQL: %s; parameter_count=%d", dialect.Name(), sql, len(variables[0]))
			require.NotContains(t, sql, row.Group)
			require.NotContains(t, sql, row.Model)
			require.Equal(t, []any{row.Scope, row.Group, row.Model, row.ChannelID, row.Score, row.LatencyMS, row.Observations, row.ObservedAt, row.ObservationSeq, row.WriterID, row.Version}, variables[0])
			if dialect.Name() == "mysql" {
				guard := "`observed_at` < VALUES(`observed_at`) OR (`observed_at` = VALUES(`observed_at`) AND (COALESCE(`observation_seq`, 0) < COALESCE(VALUES(`observation_seq`), 0) OR (COALESCE(`observation_seq`, 0) = COALESCE(VALUES(`observation_seq`), 0) AND COALESCE(`writer_id`, '') < COALESCE(VALUES(`writer_id`), ''))))"
				require.Contains(t, sql, "ON DUPLICATE KEY UPDATE")
				for _, field := range []string{"score", "latency_ms", "observations", "version", "writer_id", "observation_seq", "observed_at"} {
					require.Contains(t, sql, "`"+field+"`=CASE WHEN "+guard+" THEN VALUES(`"+field+"`) ELSE `"+field+"` END", "MySQL ignores OnConflict.Where; each field must carry the same ordering guard")
				}
				require.True(t, strings.HasSuffix(sql, "`observed_at`=CASE WHEN "+guard+" THEN VALUES(`observed_at`) ELSE `observed_at` END"), "MySQL must update observed_at last because assignments run left-to-right")
				require.True(t, strings.Index(sql, "`writer_id`=CASE") < strings.Index(sql, "`observation_seq`=CASE"), "writer_id must be assigned before observation_seq so its guard still reads the stored sequence")
			} else {
				quote := "`"
				if dialect.Name() == "postgres" {
					quote = `"`
				}
				q := func(s string) string { return quote + s + quote }
				require.Contains(t, sql, "ON CONFLICT ("+q("scope")+","+q("group")+","+q("model")+","+q("channel_id")+") DO UPDATE SET")
				require.Contains(t, sql, "WHERE "+q("auto_model_scores")+"."+q("observed_at")+" < "+q("excluded")+"."+q("observed_at"))
				require.Contains(t, sql, "COALESCE("+q("auto_model_scores")+"."+q("observation_seq")+", 0) < COALESCE("+q("excluded")+"."+q("observation_seq")+", 0)")
				require.Contains(t, sql, "COALESCE("+q("auto_model_scores")+"."+q("writer_id")+", '') < COALESCE("+q("excluded")+"."+q("writer_id")+", '')")
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
	db := newAutoModelScoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
	row := AutoModelScore{Scope: AutoModelScoreScopeModel, Group: "g", Model: "alpha", ChannelID: 1, Score: 0.1, Observations: 8, ObservedAt: time.Now().Add(time.Hour).UnixNano(), Version: 1}
	var untouched []AutoModelScore
	var hiddenScopes []AutoModelScore
	for _, mutate := range []func(*AutoModelScore){
		func(r *AutoModelScore) { r.Group = "other" },
		func(r *AutoModelScore) { r.Model = "sibling" },
		func(r *AutoModelScore) { r.ChannelID = 2 },
		func(r *AutoModelScore) { r.Scope = "other" },
	} {
		other := row
		mutate(&other)
		if other.Scope == AutoModelScoreScopeModel {
			untouched = append(untouched, other)
		} else {
			hiddenScopes = append(hiddenScopes, other)
		}
	}
	require.NoError(t, db.Create(&row).Error)
	require.NoError(t, db.Create(&untouched).Error)
	require.NoError(t, db.Create(&hiddenScopes).Error)
	require.NoError(t, DeleteAutoModelScoreSnapshot(context.Background(), db, row))
	got, err := LoadAutoModelScores(context.Background(), db)
	require.NoError(t, err)
	require.ElementsMatch(t, untouched, got, "retire only the rejected full key")
	var remaining []AutoModelScore
	require.NoError(t, db.Find(&remaining).Error)
	require.ElementsMatch(t, append(append([]AutoModelScore(nil), untouched...), hiddenScopes...), remaining, "a different scope key must not be retired either")
	require.NoError(t, DeleteAutoModelScoreSnapshot(context.Background(), db, row), "an already-retired snapshot is an idempotent no-op")
}

func TestAutoModelScoreDeleteSnapshotPreservesReplacement(t *testing.T) {
	for _, change := range []string{"newer_observation", "corrected_clock", "new_version"} {
		t.Run(change, func(t *testing.T) {
			db := newAutoModelScoreTestDB(t)
			require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
			read := AutoModelScore{Scope: AutoModelScoreScopeModel, Group: "g", Model: "alpha", ChannelID: 1, Score: 0.1, Observations: 8, ObservedAt: time.Now().Add(time.Hour).UnixNano(), Version: 1}
			require.NoError(t, db.Create(&read).Error)
			stale := read
			replacement := read
			writeTarget := read // GORM Update mutates its Model argument.
			switch change {
			case "newer_observation":
				replacement.ObservedAt++
				_, err := SaveAutoModelScores(context.Background(), db, []AutoModelScore{replacement})
				require.NoError(t, err)
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
		t.Run(dialect.Name(), func(t *testing.T) {
			db, err := gorm.Open(dialect, &gorm.Config{DryRun: true, DisableAutomaticPing: true})
			require.NoError(t, err)
			var statements []string
			var variables [][]any
			require.NoError(t, db.Callback().Delete().After("gorm:delete").Register("test:score_delete_dialect", func(tx *gorm.DB) {
				statements = append(statements, tx.Statement.SQL.String())
				variables = append(variables, append([]any(nil), tx.Statement.Vars...))
			}))
			row := AutoModelScore{Scope: AutoModelScoreScopeModel, Group: "g' OR 1=1 --", Model: "模型'", ChannelID: 4, ObservedAt: time.Now().Add(time.Hour).UnixNano(), ObservationSeq: 5, WriterID: "w", Version: 0}
			require.NoError(t, DeleteAutoModelScoreSnapshot(context.Background(), db, row))
			require.Len(t, statements, 1, "retiring a rejected snapshot must execute a conditional SQL delete")
			sql := statements[0]
			t.Logf("%s SQL: %s; parameters=%v", dialect.Name(), sql, variables[0])
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
			require.Contains(t, sql, "COALESCE("+q("observation_seq")+", 0) = COALESCE(?, 0)")
			require.Contains(t, sql, "COALESCE("+q("writer_id")+", '') = COALESCE(?, '')")
			require.Equal(t, []any{row.ChannelID, row.Group, row.Model, row.ObservedAt, row.Scope, row.Version, row.ObservationSeq, row.WriterID}, variables[0], "身份字段与完整排序元数据都必须参与条件删除")
		})
	}
}

func TestAutoModelScoreUpsertOrdersByObservationThenSequence(t *testing.T) {
	db := newAutoModelScoreTestDB(t)
	require.NoError(t, db.AutoMigrate(&AutoModelScore{}))
	ctx := context.Background()
	base := AutoModelScore{Scope: AutoModelScoreScopeModel, Group: "g", Model: "m", ChannelID: 1, Score: 0.7, LatencyMS: 200, Observations: 4, ObservedAt: time.Now().Add(-time.Minute).UnixNano(), ObservationSeq: 1, WriterID: "a", Version: 1}
	unrelated := base
	unrelated.Group = "untouched"
	_, err := SaveAutoModelScores(ctx, db, []AutoModelScore{base, unrelated})
	require.NoError(t, err)

	// 更晚时间刻度胜出,哪怕序号/实例标识反而更小:排序先看真实观测时间。
	fresh := base
	fresh.Score, fresh.LatencyMS, fresh.Observations, fresh.ObservedAt = 0, 0, 5.5, base.ObservedAt+1
	fresh.ObservationSeq, fresh.WriterID = 1, "z"
	outcomes, err := SaveAutoModelScores(ctx, db, []AutoModelScore{fresh})
	require.NoError(t, err)
	require.Equal(t, []AutoModelScoreSaveOutcome{AutoModelScoreStored}, outcomes, "更晚的 observed_at 必须胜出,哪怕序号和实例标识更小")

	// 同一时间刻度、更高序号:必须接受,不再依赖墙钟前进。
	sameTick := fresh
	sameTick.Score, sameTick.ObservationSeq = 0.31, fresh.ObservationSeq+1
	outcomes, err = SaveAutoModelScores(ctx, db, []AutoModelScore{sameTick})
	require.NoError(t, err)
	require.Equal(t, []AutoModelScoreSaveOutcome{AutoModelScoreStored}, outcomes, "同刻度更高序号必须胜出")

	// 同刻度、同序号、更大实例标识:确定性决胜。
	sameSeq := sameTick
	sameSeq.Score, sameSeq.WriterID = 0.32, "zz"
	outcomes, err = SaveAutoModelScores(ctx, db, []AutoModelScore{sameSeq})
	require.NoError(t, err)
	require.Equal(t, []AutoModelScoreSaveOutcome{AutoModelScoreStored}, outcomes, "同刻度同序号按实例标识确定性决胜")

	// 更旧快照是 no-op,并被明确标成 superseded,不产生错误。
	outcomes, err = SaveAutoModelScores(ctx, db, []AutoModelScore{base})
	require.NoError(t, err)
	require.Equal(t, []AutoModelScoreSaveOutcome{AutoModelScoreSuperseded}, outcomes, "旧快照不能伪装成已保存,但也不能覆盖更新证据")

	got, err := LoadAutoModelScores(ctx, db)
	require.NoError(t, err)
	require.ElementsMatch(t, []AutoModelScore{sameSeq, unrelated}, got, "胜者必须是最新排序元数据对应的那一行")
}
