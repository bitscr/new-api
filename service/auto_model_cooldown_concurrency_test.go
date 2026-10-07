package service

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAutoModelCooldownConcurrentTripsPreserveEveryLevel(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	const workers = 32
	now := time.Now()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			tripAutoModelCooldown("default", "model-a", 1, "failure", now)
		}()
	}
	close(start)
	wg.Wait()

	state, ok := autoModelCooldowns.Get(autoModelHealthKey("default", "model-a", 1))
	require.True(t, ok)
	require.Equal(t, workers, state.Level, "每一次并发失败都必须升级，不能丢失 Get/Set 间的更新")
	require.Equal(t, now.Add(autoModelCooldownMax), state.Until)
	rows := cooldownRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, state.Level, rows[0].Level)
	require.Equal(t, state.Until.Unix(), rows[0].Until)
}

func TestAutoModelCooldownConcurrentClearPreservesOtherKeys(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	const workers = 24
	now := time.Now()
	for id := 1; id <= workers; id++ {
		tripAutoModelCooldown("default", "old-model", id, "failure", now)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for id := 1; id <= workers; id++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			<-start
			clearAutoModelCooldown("default", "old-model", id, now)
		}(id)
		go func(id int) {
			defer wg.Done()
			<-start
			tripAutoModelCooldown("default", "new-model", id, "failure", now)
		}(id)
	}
	close(start)
	wg.Wait()

	require.Empty(t, GetAutoModelCoolingChannelIDs("default", "old-model"))
	require.Len(t, GetAutoModelCoolingChannelIDs("default", "new-model"), workers)
	rows := cooldownRows(t)
	require.Len(t, rows, workers)
	for _, row := range rows {
		require.Equal(t, "new-model", row.Model)
	}
}

// 阻塞第一个操作的数据库阶段，第二个操作必须等待整个内存/数据库事务完成。
// 同时覆盖两个写入、写入与清除的双向顺序，以及重载快照与请求结果交错。
func TestAutoModelCooldownPersistenceOperationsAreOrdered(t *testing.T) {
	cases := []struct {
		name      string
		first     string
		second    string
		seed      bool
		wantLevel int
	}{
		{name: "trip_then_trip", first: "trip", second: "trip", wantLevel: 2},
		{name: "trip_then_clear", first: "trip", second: "clear"},
		{name: "clear_then_trip", first: "clear", second: "trip", seed: true, wantLevel: 1},
		{name: "reload_then_trip", first: "load", second: "trip", seed: true, wantLevel: 2},
		{name: "reload_then_clear", first: "load", second: "clear", seed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupAutoModelCooldownTestDB(t)
			now := time.Now()
			if tc.seed {
				tripAutoModelCooldown("default", "model-a", 1, "seed", now)
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			var blocked atomic.Bool
			var releaseOnce sync.Once
			var wg sync.WaitGroup
			callback := func(tx *gorm.DB) {
				if tx.Statement.Table == "auto_model_cooldowns" && blocked.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
			}
			const callbackName = "test:auto_model_cooldown_order"
			var removeCallback func() error
			switch tc.first {
			case "trip":
				// 在事务开启前阻塞，避免 SQLite 自己的写锁掩盖服务层乱序。
				require.NoError(t, model.DB.Callback().Create().Before("gorm:begin_transaction").Register(callbackName, callback))
				removeCallback = func() error { return model.DB.Callback().Create().Remove(callbackName) }
			case "clear":
				require.NoError(t, model.DB.Callback().Delete().Before("gorm:begin_transaction").Register(callbackName, callback))
				removeCallback = func() error { return model.DB.Callback().Delete().Remove(callbackName) }
			case "load":
				// 查询已拿到旧快照，但尚未发布到内存时阻塞。
				require.NoError(t, model.DB.Callback().Query().After("gorm:query").Register(callbackName, callback))
				removeCallback = func() error { return model.DB.Callback().Query().Remove(callbackName) }
			}
			t.Cleanup(func() {
				releaseOnce.Do(func() { close(release) })
				wg.Wait()
				require.NoError(t, removeCallback())
			})
			operate := func(operation string) {
				switch operation {
				case "trip":
					tripAutoModelCooldown("default", "model-a", 1, "failure", now)
				case "clear":
					clearAutoModelCooldown("default", "model-a", 1, now)
				case "load":
					LoadAutoModelCooldowns()
				}
			}
			firstDone := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				operate(tc.first)
				close(firstDone)
			}()
			waitAutoModelCooldownOperation(t, entered)

			secondStarted := make(chan struct{})
			secondDone := make(chan struct{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				close(secondStarted)
				operate(tc.second)
				close(secondDone)
			}()
			waitAutoModelCooldownOperation(t, secondStarted)
			select {
			case <-secondDone:
				t.Error("后一个操作不应越过前一个操作尚未完成的数据库阶段")
			case <-time.After(50 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			waitAutoModelCooldownOperation(t, firstDone)
			waitAutoModelCooldownOperation(t, secondDone)

			state, ok := autoModelCooldowns.Get(autoModelHealthKey("default", "model-a", 1))
			rows := cooldownRows(t)
			if tc.wantLevel == 0 {
				require.False(t, ok, "最后的清除不能被旧快照复活")
				require.Empty(t, rows, "最后的清除不能被较早的数据库写入覆盖")
			} else {
				require.True(t, ok)
				require.Equal(t, tc.wantLevel, state.Level)
				require.Len(t, rows, 1)
				require.Equal(t, state.Level, rows[0].Level)
				require.Equal(t, state.Until.Unix(), rows[0].Until)
			}
		})
	}
}

func waitAutoModelCooldownOperation(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for cooldown operation")
	}
}

func TestAutoModelCooldownLoadRetriesAfterUnavailableDatabase(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	tripAutoModelCooldown("default", "persisted-model", 1, "failure", time.Now())
	autoModelCooldowns.Clear()
	autoModelCooldownLoadOnce = sync.Once{}

	db := model.DB
	model.DB = nil
	defer func() { model.DB = db }()
	require.Empty(t, GetAutoModelCoolingChannelIDs("default", "persisted-model"))
	model.DB = db
	require.Equal(t, map[int]bool{1: true}, GetAutoModelCoolingChannelIDs("default", "persisted-model"),
		"数据库后来可用时必须重新载入，不能被失败的 sync.Once 永久挡住")
}

func TestAutoModelCooldownLoadRetriesAfterQueryFailure(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	tripAutoModelCooldown("default", "persisted-model", 1, "failure", time.Now())
	autoModelCooldowns.Clear()
	autoModelCooldownLoadOnce = sync.Once{}
	memoryKey := autoModelHealthKey("default", "memory-model", 2)
	autoModelCooldowns.Set(memoryKey, autoModelCooldownState{Until: time.Now().Add(time.Hour), Level: 1})

	var failed atomic.Bool
	const callbackName = "test:auto_model_cooldown_load_failure"
	require.NoError(t, model.DB.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table == "auto_model_cooldowns" && failed.CompareAndSwap(false, true) {
			tx.AddError(errors.New("temporary cooldown query failure"))
		}
	}))
	t.Cleanup(func() { require.NoError(t, model.DB.Callback().Query().Remove(callbackName)) })

	ensureAutoModelCooldownsLoaded()
	require.True(t, failed.Load())
	_, ok := autoModelCooldowns.Get(memoryKey)
	require.True(t, ok, "载入失败不能清空已有内存状态")
	ensureAutoModelCooldownsLoaded()
	_, ok = autoModelCooldowns.Get(autoModelHealthKey("default", "persisted-model", 1))
	require.True(t, ok, "查询暂时失败后必须允许重试")
}

func TestAutoModelCoolingChannelIDsIsScopedIndependentSnapshot(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()
	active := autoModelCooldownState{Until: now.Add(time.Hour), Level: 1}
	autoModelCooldowns.AddAll(map[string]autoModelCooldownState{
		autoModelHealthKey("default", "model-a", 1):       active,
		autoModelHealthKey("default", "model-a", 2):       {Until: now.Add(-time.Second), Level: 1},
		autoModelHealthKey("default", "model-b", 3):       active,
		autoModelHealthKey("other-group", "model-a", 4):   active,
		autoModelHealthKey("default", "", 5):              active,
		autoModelHealthKey("default", "model-a", -1):      active,
		autoModelHealthKey("default", "model-a", 0):       active,
		autoModelHealthKey("default", "model-a-extra", 6): active,
		autoModelHealthPrefix("default", "model-a") + "invalid": active,
	})

	cooling := GetAutoModelCoolingChannelIDs(" default ", " model-a ")
	require.Equal(t, map[int]bool{1: true}, cooling)
	delete(cooling, 1)
	cooling[99] = true
	require.Equal(t, map[int]bool{1: true}, GetAutoModelCoolingChannelIDs("default", "model-a"))
	require.Empty(t, GetAutoModelCoolingChannelIDs("", "model-a"))
	require.Empty(t, GetAutoModelCoolingChannelIDs("default", ""))
	require.False(t, autoModelCooldownActive("default", "model-a", []int{1, 2}))
	require.True(t, autoModelCooldownActive("default", "model-a", []int{1}))
}

func TestAutoModelCooldownLegacyPurgePreservesModelRows(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	now := time.Now()
	// 先插入旧渠道级记录，确保清理空模型时不会把同渠道的模型级记录删掉。
	require.NoError(t, model.UpsertAutoModelCooldown(&model.AutoModelCooldown{
		Group: "default", Model: "", ChannelId: 1,
		Until: now.Add(time.Hour).Unix(), Level: 1, UpdatedAt: now.Unix(),
	}))
	tripAutoModelCooldown("default", "model-a", 1, "failure", now)
	LoadAutoModelCooldowns()
	rows := cooldownRows(t)
	require.Len(t, rows, 1)
	require.Equal(t, "model-a", rows[0].Model)
	LoadAutoModelCooldowns()
	require.Equal(t, map[int]bool{1: true}, GetAutoModelCoolingChannelIDs("default", "model-a"))
}

func TestAutoModelCooldownMapAtomicUpdateAndDelete(t *testing.T) {
	cache := types.NewRWMap[string, int]()
	cache.Set("keep", 7)
	const workers = 64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.Update("count", func(current int, exists bool) int { return current + 1 })
			cache.Delete("missing")
		}()
	}
	wg.Wait()
	count, ok := cache.Get("count")
	require.True(t, ok)
	require.Equal(t, workers, count)
	require.True(t, cache.Delete("count"))
	require.False(t, cache.Delete("count"))
	kept, ok := cache.Get("keep")
	require.True(t, ok)
	require.Equal(t, 7, kept)
}
