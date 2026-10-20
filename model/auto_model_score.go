package model

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	AutoModelScoreVersion    = 1
	AutoModelScoreScopeModel = "model"

	// autoModelScoresTable 是物理表名:冲突条件必须显式限定"库内已存行",
	// 不能依赖 GORM 的默认别名。
	autoModelScoresTable = "auto_model_scores"
)

// AutoModelScoreSaveOutcome 是数据库对单条快照的处置结果。
//
// 批量 upsert 的 RowsAffected 无法证明"每一条都已写入":被排序规则拒绝的行
// 与已写入的行在总数里无法区分。所以存储提交后按键回读受影响的那些行,逐条
// 给出结论,调用方据此决定哪些快照算"已确认"。
type AutoModelScoreSaveOutcome uint8

const (
	// AutoModelScoreStored:本条快照现在持有该键。
	AutoModelScoreStored AutoModelScoreSaveOutcome = iota + 1
	// AutoModelScoreUnchanged:库内已有完全相同的一行(含全部排序元数据与评分内容)。
	AutoModelScoreUnchanged
	// AutoModelScoreSuperseded:该键被更新的快照持有。调用方不能把它当作已保存,
	// 也不该在内容没有变化时反复重写。
	AutoModelScoreSuperseded
)

// AutoModelScore stores a raw passive score summary for one (group, model,
// channel) route, not request content. The Scope column is kept for schema
// compatibility; only Scope=model rows are written, loaded and updated.
// Legacy scope=channel rows are ignored by reads and left to the generic
// expiration prune.
//
// ObservedAt is the last actual observation in Unix nanoseconds; saving or
// restoring must never refresh it, because decay and recovery both read it.
//
// ObservationSeq/WriterID 是独立的快照排序元数据,不改变 ObservedAt 的语义:
// 同一进程内同一三元组的每次评分更新都在评分锁里递增序号,实例标识按进程生成。
// 冲突比较先看真实 ObservedAt,同刻度再按(序号,实例标识)给出确定性的胜者,
// 于是"同一时间刻度的新观测"不会再被严格时间戳规则拒绝,而延迟到达的旧快照
// 依然无法覆盖更新的快照。多实例仍然是"选一个快照",不合并 EWMA;跨实例的
// 同刻度先后是一条确定性冲突规则,不是全球严格事件顺序。
//
// 两列都可为空(旧行由 ALTER TABLE 增列产生,值为 NULL):读取时按零值处理,
// SQL 比较一律经过 COALESCE,因此旧行与新行可以同表共存。
type AutoModelScore struct {
	Scope        string  `gorm:"type:varchar(8);primaryKey;not null"`
	Group        string  `gorm:"type:varchar(64);primaryKey;not null"`
	Model        string  `gorm:"type:varchar(255);primaryKey;not null"`
	ChannelID    int     `gorm:"primaryKey;autoIncrement:false;not null"`
	Score        float64 `gorm:"not null"`
	LatencyMS    float64 `gorm:"not null"`
	Observations float64 `gorm:"not null"`
	ObservedAt   int64   `gorm:"not null;index;autoCreateTime:false;autoUpdateTime:false"`
	// ObservationSeq 是同一实例内该三元组的观测序号(从 1 起),同刻度时大者胜。
	ObservationSeq int64 `gorm:"column:observation_seq;autoCreateTime:false;autoUpdateTime:false"`
	// WriterID 是写入该快照的进程标识;序号也相同时,字典序大者胜(确定性冲突规则)。
	WriterID string `gorm:"column:writer_id;type:varchar(64);autoCreateTime:false;autoUpdateTime:false"`
	Version  int    `gorm:"not null"`
}

// autoModelScoreConflictFields 是快照被接纳时要替换的全部列。
//
// 顺序对 MySQL 有意义:它的赋值从左到右求值,而守卫条件引用的正是这三列排序
// 元数据,所以 writer_id、observation_seq 必须排在被比较的其它列之后、
// observed_at 之前——observed_at 最后赋值,前面每一列的守卫都还能看到旧值。
var autoModelScoreConflictFields = []string{"score", "latency_ms", "observations", "version", "writer_id", "observation_seq", "observed_at"}

// AutoModelScoreIsNewer reports whether candidate strictly outranks stored in
// the snapshot ordering: real observation time first, then the per-instance
// sequence, then the writer identity as a deterministic tie-break. Legacy rows
// carry zero metadata and therefore lose every tie against a scored observation.
func AutoModelScoreIsNewer(candidate, stored AutoModelScore) bool {
	if candidate.ObservedAt != stored.ObservedAt {
		return candidate.ObservedAt > stored.ObservedAt
	}
	if candidate.ObservationSeq != stored.ObservationSeq {
		return candidate.ObservationSeq > stored.ObservationSeq
	}
	return candidate.WriterID > stored.WriterID
}

// autoModelScoreStoredOlderExpr renders "库内已存行严格旧于待写入行" for the
// SQLite/PostgreSQL DO UPDATE guard. Both sides go through COALESCE so a NULL
// ordering field left by an older schema behaves exactly like zero.
func autoModelScoreStoredOlderExpr() clause.Expr {
	stored := func(name string) clause.Column { return clause.Column{Table: autoModelScoresTable, Name: name} }
	incoming := func(name string) clause.Column { return clause.Column{Table: "excluded", Name: name} }
	return clause.Expr{
		SQL: "? < ? OR (? = ? AND (COALESCE(?, 0) < COALESCE(?, 0) OR " +
			"(COALESCE(?, 0) = COALESCE(?, 0) AND COALESCE(?, '') < COALESCE(?, ''))))",
		Vars: []any{
			stored("observed_at"), incoming("observed_at"),
			stored("observed_at"), incoming("observed_at"),
			stored("observation_seq"), incoming("observation_seq"),
			stored("observation_seq"), incoming("observation_seq"),
			stored("writer_id"), incoming("writer_id"),
		},
	}
}

// autoModelScoreMySQLStaleGuard renders the same rule with MySQL syntax, where
// OnConflict.Where is ignored and the incoming row is reachable only as VALUES().
func autoModelScoreMySQLStaleGuard() clause.Expr {
	stored := func(name string) clause.Column { return clause.Column{Name: name} }
	return clause.Expr{
		SQL: "? < VALUES(?) OR (? = VALUES(?) AND (COALESCE(?, 0) < COALESCE(VALUES(?), 0) OR " +
			"(COALESCE(?, 0) = COALESCE(VALUES(?), 0) AND COALESCE(?, '') < COALESCE(VALUES(?), ''))))",
		Vars: []any{
			stored("observed_at"), stored("observed_at"),
			stored("observed_at"), stored("observed_at"),
			stored("observation_seq"), stored("observation_seq"),
			stored("observation_seq"), stored("observation_seq"),
			stored("writer_id"), stored("writer_id"),
		},
	}
}

// SaveAutoModelScores commits one captured batch atomically and reports, per
// input row and in input order, whether the database now holds that snapshot.
// All I/O uses the caller's context and a stable DB handle.
//
// Per-key ordering prevents a delayed instance/batch from replacing a newer
// stored snapshot, while a same-tick observation with a larger sequence still
// wins: accepting new evidence must not require the wall clock to advance.
func SaveAutoModelScores(ctx context.Context, db *gorm.DB, rows []AutoModelScore) ([]AutoModelScoreSaveOutcome, error) {
	if db == nil {
		return nil, errors.New("auto model score database is not initialized")
	}
	if len(rows) == 0 {
		return nil, nil
	}
	conflict := clause.OnConflict{
		Columns:   []clause.Column{{Name: "scope"}, {Name: "group"}, {Name: "model"}, {Name: "channel_id"}},
		DoUpdates: clause.AssignmentColumns(autoModelScoreConflictFields),
		Where:     clause.Where{Exprs: []clause.Expression{autoModelScoreStoredOlderExpr()}},
	}
	if db.Dialector.Name() == "mysql" {
		// MySQL 5.7 ignores OnConflict.Where. Guard every assignment with the same
		// ordering rule and update observed_at LAST: MySQL evaluates assignments
		// from left to right.
		guard := autoModelScoreMySQLStaleGuard()
		conflict.Where = clause.Where{}
		conflict.DoUpdates = nil
		for _, field := range autoModelScoreConflictFields {
			column := clause.Column{Name: field}
			vars := append(append([]any(nil), guard.Vars...), column, column)
			conflict.DoUpdates = append(conflict.DoUpdates, clause.Assignment{
				Column: column,
				Value:  clause.Expr{SQL: "CASE WHEN " + guard.SQL + " THEN VALUES(?) ELSE ? END", Vars: vars},
			})
		}
	}
	var affected int64
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Clauses(conflict).CreateInBatches(&rows, 100)
		affected = res.RowsAffected
		return res.Error
	}); err != nil {
		return nil, err
	}
	if db.DryRun {
		// Dry-run callers inspect the generated SQL only; there is no stored row
		// to verify, so no per-row outcome is reported.
		return nil, nil
	}
	if autoModelScoreSaveAcceptedAll(db, affected, len(rows)) {
		// 每一条都被数据库接纳:此刻的批量影响行数足以证明,不需要额外回读。
		return autoModelScoreStoredOutcomes(len(rows)), nil
	}
	// 至少有一条可能没被接纳(库内已有完全相同的一行,或已被更新的快照持有)。
	// RowsAffected 无法区分这三种情况,所以提交后按键回读核实。回读放在提交之后:
	// 不值得为它延长写事务(SQLite 上写事务会独占库文件,阻塞其它连接的写入);
	// 提交与回读之间若有并发替换,只会让本批被保守地判为"已被更新快照取代"。
	return verifyAutoModelScoresStored(db.WithContext(ctx), rows)
}

// autoModelScoreSaveAcceptedAll 判断一个批次是否每一条都被接纳。
//
// SQLite/PostgreSQL 的 upsert 对"插入"与"真正更新"各计 1 行,被排序规则拒绝的
// 行计 0;MySQL 对更新计 2 行、对"值没变"计 0。所以非 MySQL 用"等于行数"、
// MySQL 用"不少于行数"作为判据,任何"可能没写进去"的批次都交给逐条回读核实。
func autoModelScoreSaveAcceptedAll(db *gorm.DB, affected int64, rows int) bool {
	if db.Dialector.Name() == "mysql" {
		return affected >= int64(rows)
	}
	return affected == int64(rows)
}

func autoModelScoreStoredOutcomes(count int) []AutoModelScoreSaveOutcome {
	outcomes := make([]AutoModelScoreSaveOutcome, count)
	for i := range outcomes {
		outcomes[i] = AutoModelScoreStored
	}
	return outcomes
}

type autoModelScoreRowKey struct {
	scope, group, model string
	channelID           int
}

func autoModelScoreRowKeyOf(row AutoModelScore) autoModelScoreRowKey {
	return autoModelScoreRowKey{row.Scope, row.Group, row.Model, row.ChannelID}
}

// verifyAutoModelScoresStored re-reads the keys of this batch after the write
// commits and classifies each row. The key filters are a superset (the
// cross product of the batch's scopes/groups/models/channels); matching happens
// on the full key, so no unrelated row can acknowledge a write.
func verifyAutoModelScoresStored(db *gorm.DB, rows []AutoModelScore) ([]AutoModelScoreSaveOutcome, error) {
	scopes := make([]any, 0, len(rows))
	groups := make([]any, 0, len(rows))
	models := make([]any, 0, len(rows))
	channels := make([]any, 0, len(rows))
	seen := make(map[string]bool, len(rows)*4)
	appendUnique := func(target *[]any, key string, value any) {
		if seen[key] {
			return
		}
		seen[key] = true
		*target = append(*target, value)
	}
	for _, row := range rows {
		appendUnique(&scopes, "scope:"+row.Scope, row.Scope)
		appendUnique(&groups, "group:"+row.Group, row.Group)
		appendUnique(&models, "model:"+row.Model, row.Model)
		appendUnique(&channels, fmt.Sprintf("channel:%d", row.ChannelID), row.ChannelID)
	}
	var stored []AutoModelScore
	if err := db.Where(clause.IN{Column: clause.Column{Name: "scope"}, Values: scopes}).
		Where(clause.IN{Column: clause.Column{Name: "group"}, Values: groups}).
		Where(clause.IN{Column: clause.Column{Name: "model"}, Values: models}).
		Where(clause.IN{Column: clause.Column{Name: "channel_id"}, Values: channels}).
		Find(&stored).Error; err != nil {
		return nil, err
	}
	byKey := make(map[autoModelScoreRowKey]AutoModelScore, len(stored))
	for _, row := range stored {
		byKey[autoModelScoreRowKeyOf(row)] = row
	}
	outcomes := make([]AutoModelScoreSaveOutcome, len(rows))
	for i, row := range rows {
		current, ok := byKey[autoModelScoreRowKeyOf(row)]
		if !ok {
			return nil, fmt.Errorf("auto model score snapshot %s/%s#%d disappeared while saving", row.Group, row.Model, row.ChannelID)
		}
		switch {
		case current == row:
			outcomes[i] = AutoModelScoreUnchanged
		case AutoModelScoreIsNewer(current, row):
			outcomes[i] = AutoModelScoreSuperseded
		default:
			// The database still holds an older, or equally ordered but different,
			// snapshot, so this write cannot be acknowledged. Reporting a failure
			// keeps the caller's retry instead of claiming a false success.
			return nil, fmt.Errorf("auto model score snapshot %s/%s#%d was not accepted by the ordering guard", row.Group, row.Model, row.ChannelID)
		}
	}
	return outcomes, nil
}

// DeleteAutoModelScoreSnapshot retires a rejected observation without deleting a
// concurrent replacement. Match the full key, observation time, version and the
// complete ordering metadata, including zero/empty fields; a NULL ordering field
// left by an older schema is compared as zero. The normal upsert ordering is
// unchanged.
func DeleteAutoModelScoreSnapshot(ctx context.Context, db *gorm.DB, row AutoModelScore) error {
	if db == nil {
		return errors.New("auto model score database is not initialized")
	}
	return db.WithContext(ctx).Where(map[string]any{
		"scope": row.Scope, "group": row.Group, "model": row.Model, "channel_id": row.ChannelID,
		"observed_at": row.ObservedAt, "version": row.Version,
	}).Where(clause.Expr{
		SQL: "COALESCE(?, 0) = COALESCE(?, 0) AND COALESCE(?, '') = COALESCE(?, '')",
		Vars: []any{
			clause.Column{Name: "observation_seq"}, row.ObservationSeq,
			clause.Column{Name: "writer_id"}, row.WriterID,
		},
	}).Delete(&AutoModelScore{}).Error
}

// PruneAutoModelScores removes only expired observation snapshots; it does not
// replace the table or race a newer per-key write with a key-only deletion.
func PruneAutoModelScores(ctx context.Context, db *gorm.DB, observedAtOrBefore int64) error {
	if db == nil {
		return errors.New("auto model score database is not initialized")
	}
	return db.WithContext(ctx).Where(clause.Lte{Column: clause.Column{Name: "observed_at"}, Value: observedAtOrBefore}).Delete(&AutoModelScore{}).Error
}

func LoadAutoModelScores(ctx context.Context, db *gorm.DB) ([]AutoModelScore, error) {
	if db == nil {
		return nil, errors.New("auto model score database is not initialized")
	}
	var rows []AutoModelScore
	err := db.WithContext(ctx).Where(&AutoModelScore{Scope: AutoModelScoreScopeModel}).Find(&rows).Error
	return rows, err
}
