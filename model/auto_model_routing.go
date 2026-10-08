package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm/clause"
)

// AutoModelRoutingTarget is a caller-owned scalar snapshot of an enabled ability
// on an existing, enabled channel, never a mutable cached Channel reference.
// Priority is Channel.Priority (nil means zero), not Ability.Priority.
// Weight is a CHANNEL selection weight, never a per-model selection weight:
// it is Channel.Weight (nil means zero) in memory-cache mode and Channel.Weight
// + 10 in DB mode to preserve the existing DB sampling smoothing. Ability.Weight
// is deliberately ignored. An all-zero memory weight band is sampled uniformly;
// model-selection weights are configured independently of this snapshot.
type AutoModelRoutingTarget struct {
	ChannelID int
	Priority  int64
	Weight    float64
}

// GetAutoModelRoutingTargets snapshots a group's enabled abilities joined to
// existing, enabled channels in one batch query, with no per-channel queries.
// Both cache modes query the database: the legacy channel cache is built from
// Channel.Models/Group and does not preserve Ability.Enabled. Only the channel
// weight smoothing differs between modes; eligibility and priorities do not.
// The result has no guaranteed order and can be modified freely by the caller.
func GetAutoModelRoutingTargets(group string) (map[string][]AutoModelRoutingTarget, error) {
	if DB == nil {
		return nil, errors.New("routing database is not initialized")
	}
	var rows []struct {
		Model     string
		ChannelID int
		Priority  *int64
		Weight    *uint
	}
	if err := DB.Table("abilities").
		Select("abilities.model, channels.id AS channel_id, channels.priority, channels.weight").
		Joins("INNER JOIN channels ON channels.id = abilities.channel_id").
		Where(clause.Eq{Column: clause.Column{Table: "abilities", Name: "group"}, Value: group}).
		Where("abilities.enabled = ? AND channels.status = ?", true, common.ChannelStatusEnabled).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make(map[string][]AutoModelRoutingTarget)
	for _, row := range rows {
		target := AutoModelRoutingTarget{ChannelID: row.ChannelID}
		if row.Priority != nil {
			target.Priority = *row.Priority
		}
		if row.Weight != nil {
			target.Weight = float64(*row.Weight)
		}
		if !common.MemoryCacheEnabled {
			target.Weight += 10
		}
		result[row.Model] = append(result[row.Model], target)
	}
	return result, nil
}
