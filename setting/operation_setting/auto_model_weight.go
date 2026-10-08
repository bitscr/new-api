package operation_setting

import (
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// DefaultAutoModelWeight applies only to models within a selected channel.
// Channel selection continues to use the channel's own configured weight.
const DefaultAutoModelWeight = 1.0

var autoModelWeights = struct {
	sync.RWMutex
	groups map[string]map[string]float64
}{groups: make(map[string]map[string]float64)}

// SetAutoModelWeights replaces the group -> model -> weight configuration.
// Missing entries default to one; an explicit zero disables that model for auto.
// Validation completes before the atomic replacement, so a rejected update never
// clears the live configuration. Empty input restores the default weights.
func SetAutoModelWeights(value string) error {
	groups, err := parseAutoModelWeights(value)
	if err != nil {
		return err
	}
	autoModelWeights.Lock()
	autoModelWeights.groups = groups
	autoModelWeights.Unlock()
	return nil
}

// ValidateAutoModelWeights checks an option value without changing live routing.
func ValidateAutoModelWeights(value string) error {
	_, err := parseAutoModelWeights(value)
	return err
}

func parseAutoModelWeights(value string) (map[string]map[string]float64, error) {
	groups := make(map[string]map[string]float64)
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		var parsed map[string]map[string]*float64
		if err := common.UnmarshalJsonStr(trimmed, &parsed); err != nil {
			return nil, err
		}
		if parsed == nil {
			return nil, fmt.Errorf("auto model weights must be a JSON object")
		}
		for rawGroup, models := range parsed {
			group := strings.TrimSpace(rawGroup)
			if group == "" || models == nil {
				return nil, fmt.Errorf("auto model weights require a non-empty group and a model weight object")
			}
			if _, exists := groups[group]; exists {
				return nil, fmt.Errorf("duplicate auto model weight group %q", group)
			}
			cleaned := make(map[string]float64, len(models))
			for rawName, weight := range models {
				name := strings.TrimSpace(rawName)
				if name == "" || weight == nil || *weight < 0 || math.IsNaN(*weight) || math.IsInf(*weight, 0) {
					return nil, fmt.Errorf("auto model weight for %q/%q must be a finite non-negative number with a non-empty model name", group, name)
				}
				if _, exists := cleaned[name]; exists {
					return nil, fmt.Errorf("duplicate auto model weight for %q/%q", group, name)
				}
				cleaned[name] = *weight
			}
			groups[group] = cleaned
		}
	}
	return groups, nil
}

// GetAutoModelWeights returns a deep copy that a request can use as one snapshot.
func GetAutoModelWeights() map[string]map[string]float64 {
	autoModelWeights.RLock()
	defer autoModelWeights.RUnlock()
	groups := make(map[string]map[string]float64, len(autoModelWeights.groups))
	for group, models := range autoModelWeights.groups {
		copyModels := make(map[string]float64, len(models))
		for name, weight := range models {
			copyModels[name] = weight
		}
		groups[group] = copyModels
	}
	return groups
}

func GetAutoModelWeight(group, name string) float64 {
	autoModelWeights.RLock()
	defer autoModelWeights.RUnlock()
	if weight, exists := autoModelWeights.groups[strings.TrimSpace(group)][strings.TrimSpace(name)]; exists {
		return weight
	}
	return DefaultAutoModelWeight
}

func AutoModelWeightsToJSONString() string {
	data, err := common.Marshal(GetAutoModelWeights())
	if err != nil {
		return "{}"
	}
	return string(data)
}

// AutoModelWeightsOptionKey is the persisted JSON option, not a channel weight.
const AutoModelWeightsOptionKey = "AutoModelWeights"
