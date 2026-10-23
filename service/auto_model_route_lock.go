package service

import "sync"

// Auto-model feedback, deterministic retirement and administrator clears for
// one route must publish in a single order. The keyed lock is intentionally
// narrower than the score/cooldown mutexes: it serializes whole route actions,
// while those global locks continue to protect only their in-memory maps (and
// never wrap the retirement SQL transaction).
//
// Lock order is always: route publication lock -> score lock / cooldown lock.
// Never acquire a route lock while holding either global lock.
type autoModelRouteLockEntry struct {
	mu   sync.Mutex
	refs int
}

var autoModelRouteLockRegistry = struct {
	sync.Mutex
	entries map[string]*autoModelRouteLockEntry
}{entries: make(map[string]*autoModelRouteLockEntry)}

func lockAutoModelRoute(key string) func() {
	registry := &autoModelRouteLockRegistry
	registry.Lock()
	entry := registry.entries[key]
	if entry == nil {
		entry = &autoModelRouteLockEntry{}
		registry.entries[key] = entry
	}
	entry.refs++
	registry.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		registry.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(registry.entries, key)
		}
		registry.Unlock()
	}
}
