package model

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	AutoModelScoreVersion      = 1
	AutoModelScoreScopeModel   = "model"
	AutoModelScoreScopeChannel = "channel"
)

// AutoModelScore stores a raw passive score summary, not request content. Scope
// distinguishes (group, model, channel) evidence from independently aggregated
// (group, channel) evidence, whose Model is empty. ObservedAt is the last actual
// observation in Unix nanoseconds; saving/restoring must never refresh it.
// Shared databases reuse last-observation snapshots, not distributed EWMA merges.
type AutoModelScore struct {
	Scope        string  `gorm:"type:varchar(8);primaryKey;not null"`
	Group        string  `gorm:"type:varchar(64);primaryKey;not null"`
	Model        string  `gorm:"type:varchar(255);primaryKey;not null"`
	ChannelID    int     `gorm:"primaryKey;autoIncrement:false;not null"`
	Score        float64 `gorm:"not null"`
	LatencyMS    float64 `gorm:"not null"`
	Observations float64 `gorm:"not null"`
	ObservedAt   int64   `gorm:"not null;index;autoCreateTime:false;autoUpdateTime:false"`
	Version      int     `gorm:"not null"`
}

// SaveAutoModelScores commits one captured batch atomically. All I/O uses the
// caller's context and stable DB handle. Per-key strict observation ordering
// prevents a delayed instance/batch from replacing a newer stored snapshot.
func SaveAutoModelScores(ctx context.Context, db *gorm.DB, rows []AutoModelScore) error {
	if db == nil {
		return errors.New("auto model score database is not initialized")
	}
	fields := []string{"score", "latency_ms", "observations", "version", "observed_at"}
	conflict := clause.OnConflict{
		Columns:   []clause.Column{{Name: "scope"}, {Name: "group"}, {Name: "model"}, {Name: "channel_id"}},
		DoUpdates: clause.AssignmentColumns(fields),
		Where: clause.Where{Exprs: []clause.Expression{clause.Lt{
			Column: clause.Column{Table: "auto_model_scores", Name: "observed_at"},
			Value:  clause.Column{Table: "excluded", Name: "observed_at"},
		}}},
	}
	if db.Dialector.Name() == "mysql" {
		// MySQL 5.7 ignores OnConflict.Where. Guard every assignment and update
		// observed_at LAST: MySQL evaluates assignments from left to right.
		conflict.Where = clause.Where{}
		conflict.DoUpdates = nil
		for _, field := range fields {
			column := clause.Column{Name: field}
			conflict.DoUpdates = append(conflict.DoUpdates, clause.Assignment{
				Column: column,
				Value: clause.Expr{SQL: "CASE WHEN ? < VALUES(?) THEN VALUES(?) ELSE ? END", Vars: []any{
					clause.Column{Name: "observed_at"}, clause.Column{Name: "observed_at"}, column, column,
				}},
			})
		}
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return tx.Clauses(conflict).CreateInBatches(&rows, 100).Error
	})
}

// DeleteAutoModelScoreSnapshot retires a rejected observation without deleting a
// concurrent replacement. Match the full key, observation time and version,
// including zero/empty fields; the normal strict upsert ordering is unchanged.
func DeleteAutoModelScoreSnapshot(ctx context.Context, db *gorm.DB, row AutoModelScore) error {
	if db == nil {
		return errors.New("auto model score database is not initialized")
	}
	return db.WithContext(ctx).Where(map[string]any{
		"scope": row.Scope, "group": row.Group, "model": row.Model, "channel_id": row.ChannelID,
		"observed_at": row.ObservedAt, "version": row.Version,
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
	err := db.WithContext(ctx).Find(&rows).Error
	return rows, err
}
