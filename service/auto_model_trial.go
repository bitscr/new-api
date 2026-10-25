package service

import (
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Trial routes receive at most one in-flight real business request per process.
// The slot is acquired only at the real dispatch boundary, never during planning
// or page queries, and is released when that attempt has completed.
var (
	autoModelTrialSlotsMu sync.Mutex
	autoModelTrialSlots   = make(map[AutoModelRoute]bool)
)

func AcquireAutoModelTrialSlot(route AutoModelRoute) (release func(), ok bool) {
	if route.Group == "" || route.ModelName == "" || route.ChannelID <= 0 {
		return func() {}, false
	}
	autoModelTrialSlotsMu.Lock()
	if autoModelTrialSlots[route] {
		autoModelTrialSlotsMu.Unlock()
		return func() {}, false
	}
	autoModelTrialSlots[route] = true
	autoModelTrialSlotsMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			autoModelTrialSlotsMu.Lock()
			delete(autoModelTrialSlots, route)
			autoModelTrialSlotsMu.Unlock()
		})
	}, true
}

func IsAutoModelTrialRoute(c *gin.Context, route AutoModelRoute) bool {
	meta := GetAutoModelRoutePlanMeta(c)
	return meta.Trial != nil && meta.Trial[route]
}

// AutoModelExplorationAttemptTimeout limits only unverified exploration that
// has a working immediate fallback. Ordinary priority selection and proven slow
// routes retain their existing deadlines.
func AutoModelExplorationAttemptTimeout(c *gin.Context) time.Duration {
	meta := GetAutoModelRoutePlanMeta(c)
	current, ok := GetCurrentAutoModelRoute(c)
	if !ok || !meta.Explored || current != meta.ExplorationRoute {
		return 0
	}
	working := func(route AutoModelRoute) bool {
		switch meta.Categories[route] {
		case "main", "backup", "successful_trial":
			return true
		}
		return false
	}
	if working(current) {
		return 0
	}
	routes := GetAutoModelRoutePlan(c)
	for i, route := range routes {
		if route == current && i+1 < len(routes) && working(routes[i+1]) {
			return time.Duration(autoModelSlowLatencyMs) * time.Millisecond
		}
	}
	return 0
}
