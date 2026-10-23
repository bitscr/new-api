package service

import (
	"sync"

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
