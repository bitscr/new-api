package model

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
)

// AutoModelRoutingTarget is a scalar snapshot, never a mutable cached channel.
// Weight is the relative selection weight: channel weight in memory, ability
// weight + 10 in DB mode. An all-zero memory score band must be sampled uniformly.
// Memory smoothing only multiplies nonzero weights, so does not change expectation.
type AutoModelRoutingTarget struct {
	ChannelID int
	Priority  int64
	Weight    float64
}

// GetAutoModelRoutingTargets snapshots all enabled routes in one group without
// per-channel queries. Memory and DB modes follow their respective selectors.
func GetAutoModelRoutingTargets(group string) (map[string][]AutoModelRoutingTarget, error) {
	result := make(map[string][]AutoModelRoutingTarget)
	// Column names are initialised by chooseDB(); a caller that wires up its own
	// DB (tests, embedded use) would otherwise build "WHERE  = ?" and get a SQL
	// syntax error. Same guard as GetTokenByKey.
	if commonGroupCol == "" {
		initCol()
	}
	if common.MemoryCacheEnabled {
		channelSyncLock.RLock()
		defer channelSyncLock.RUnlock()
		for name, ids := range group2model2channels[group] {
			for _, id := range ids {
				channel := channelsIDM[id]
				if channel == nil || channel.Status != common.ChannelStatusEnabled {
					continue
				}
				result[name] = append(result[name], AutoModelRoutingTarget{
					ChannelID: id,
					Priority:  channel.GetPriority(),
					Weight:    float64(channel.GetWeight()),
				})
			}
		}
		return result, nil
	}
	if DB == nil {
		return nil, errors.New("routing database is not initialized")
	}
	var abilities []Ability
	if err := DB.Where(commonGroupCol+" = ? AND enabled = ?", group, true).Find(&abilities).Error; err != nil {
		return nil, err
	}
	for _, ability := range abilities {
		priority := int64(0)
		if ability.Priority != nil {
			priority = *ability.Priority
		}
		result[ability.Model] = append(result[ability.Model], AutoModelRoutingTarget{
			ChannelID: ability.ChannelId,
			Priority:  priority,
			Weight:    float64(ability.Weight) + 10,
		})
	}
	return result, nil
}
