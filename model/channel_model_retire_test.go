package model

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func newAutoModelRetireTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
	return db
}

func TestRetireChannelModelHonorsDeadlineWhileQueryBlocks(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "retire-deadline.db")), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
	oldDB := DB
	DB = db
	t.Cleanup(func() { DB = oldDB })

	require.NoError(t, db.Create(&Channel{
		Id: 5, Key: "k", Status: common.ChannelStatusEnabled, Group: "g", Models: "a,b",
	}).Error)
	require.NoError(t, db.Create(&Ability{
		Group: "g", Model: "a", ChannelId: 5, Enabled: true,
	}).Error)

	const callbackName = "test:retire_blocked_query"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callbackName, func(tx *gorm.DB) {
		if tx.Statement.Table != "channels" {
			return
		}
		<-tx.Statement.Context.Done()
		tx.AddError(tx.Statement.Context.Err())
	}))
	removed := false
	t.Cleanup(func() {
		if !removed {
			_ = db.Callback().Query().Remove(callbackName)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = RetireChannelModel(ctx, 5, "a", "确定性失败")
	require.Error(t, err, "a blocked retire query must end at the caller's deadline")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, db.Callback().Query().Remove(callbackName))
	removed = true

	var channel Channel
	require.NoError(t, db.First(&channel, "id = ?", 5).Error)
	require.Equal(t, "a,b", channel.Models, "a timed-out retire must not commit anything")
	var abilities []Ability
	require.NoError(t, db.Where("channel_id = ?", 5).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	require.Equal(t, "a", abilities[0].Model)
}

// 摘除必须是原子的:models 更新、abilities 重建、摘空后的状态变更要么全成,要么全不成。
func TestRetireChannelModelUpdatesModelsAndAbilitiesAtomically(t *testing.T) {
	db := newAutoModelRetireTestDB(t)
	oldDB := DB
	DB = db
	t.Cleanup(func() { DB = oldDB })

	require.NoError(t, db.Create(&Channel{
		Id: 1, Key: "k", Status: common.ChannelStatusEnabled, Group: "g", Models: "a,b",
	}).Error)
	require.NoError(t, db.Create(&[]Ability{
		{Group: "g", Model: "a", ChannelId: 1, Enabled: true},
		{Group: "g", Model: "b", ChannelId: 1, Enabled: true},
	}).Error)

	result, err := RetireChannelModel(context.Background(), 1, "a", "确定性失败")
	require.NoError(t, err)
	require.True(t, result.Removed)
	require.False(t, result.Emptied)

	var channel Channel
	require.NoError(t, db.First(&channel, "id = ?", 1).Error)
	require.Equal(t, "b", channel.Models)
	var abilities []Ability
	require.NoError(t, db.Where("channel_id = ?", 1).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	require.Equal(t, "b", abilities[0].Model, "被摘除的模型不能再留在 abilities 里")
}

// 摘空最后一个模型时:渠道在同一事务里被自动禁用,abilities 变空。
func TestRetireChannelModelDisablesEmptiedChannel(t *testing.T) {
	db := newAutoModelRetireTestDB(t)
	oldDB := DB
	DB = db
	t.Cleanup(func() { DB = oldDB })

	require.NoError(t, db.Create(&Channel{
		Id: 2, Key: "k", Status: common.ChannelStatusEnabled, Group: "g", Models: "only",
	}).Error)
	require.NoError(t, db.Create(&Ability{
		Group: "g", Model: "only", ChannelId: 2, Enabled: true,
	}).Error)

	result, err := RetireChannelModel(context.Background(), 2, "only", "确定性失败")
	require.NoError(t, err)
	require.True(t, result.Removed)
	require.True(t, result.Emptied)

	var channel Channel
	require.NoError(t, db.First(&channel, "id = ?", 2).Error)
	require.Empty(t, channel.Models, "最后一个模型被摘掉后 models 必须是空串")
	require.Equal(t, common.ChannelStatusAutoDisabled, channel.Status)
	var count int64
	require.NoError(t, db.Model(&Ability{}).Where("channel_id = ?", 2).Count(&count).Error)
	require.Zero(t, count, "摘空后不得留下空模型名的幽灵 ability")
}

// 幂等:模型已不在 models 里时不重复更新 models;但历史遗留的 ability 必须被
// 修复——否则一个已摘除的模型仍能被打到上游。
func TestRetireChannelModelRepairsStaleAbility(t *testing.T) {
	db := newAutoModelRetireTestDB(t)
	oldDB := DB
	DB = db
	t.Cleanup(func() { DB = oldDB })

	require.NoError(t, db.Create(&Channel{
		Id: 3, Key: "k", Status: common.ChannelStatusEnabled, Group: "g", Models: "b",
	}).Error)
	// 历史不一致:模型已不在 models 里,却还挂着 ability。
	require.NoError(t, db.Create(&[]Ability{
		{Group: "g", Model: "a", ChannelId: 3, Enabled: true},
		{Group: "g", Model: "b", ChannelId: 3, Enabled: true},
	}).Error)

	result, err := RetireChannelModel(context.Background(), 3, "a", "确定性失败")
	require.NoError(t, err)
	require.False(t, result.Removed, "模型本来就不在列表里,不算本次删除")

	var abilities []Ability
	require.NoError(t, db.Where("channel_id = ?", 3).Find(&abilities).Error)
	require.Len(t, abilities, 1)
	require.Equal(t, "b", abilities[0].Model, "遗留 ability 必须被修复")
}

// 并发摘除同一渠道的两个模型:最终两个模型都不能复活。
//
// 这是 CAS + 有限重试要保证的核心不变量:两个请求都基于各自的旧快照读改写,
// 如果不做冲突检测,后提交的那个会把先提交的那个删掉的模型写回来。
func TestRetireChannelModelConcurrentRetiresDoNotResurrectModels(t *testing.T) {
	// 用文件库 + WAL + 多连接,让两个事务真正并发地读改写,触发 CAS 冲突后重试。
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "retire.db")+
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
	oldDB := DB
	DB = db
	oldSQLite := common.UsingSQLite
	common.UsingSQLite = true
	t.Cleanup(func() {
		DB = oldDB
		common.UsingSQLite = oldSQLite
	})

	require.NoError(t, db.Create(&Channel{
		Id: 4, Key: "k", Status: common.ChannelStatusEnabled, Group: "g", Models: "a,b,c",
	}).Error)
	for _, name := range []string{"a", "b"} {
		require.NoError(t, db.Create(&Ability{
			Group: "g", Model: name, ChannelId: 4, Enabled: true,
		}).Error)
	}

	var wg sync.WaitGroup
	retireErrs := make([]error, 2)
	for i, name := range []string{"a", "b"} {
		wg.Add(1)
		go func(index int, modelName string) {
			defer wg.Done()
			_, retireErrs[index] = RetireChannelModel(context.Background(), 4, modelName, "确定性失败")
		}(i, name)
	}
	wg.Wait()
	for _, retireErr := range retireErrs {
		require.NoError(t, retireErr, "并发摘除最终都必须成功(CAS 重试之后)")
	}

	var channel Channel
	require.NoError(t, db.First(&channel, "id = ?", 4).Error)
	require.NotContains(t, channel.GetModels(), "a", "被摘除的模型不能复活")
	require.NotContains(t, channel.GetModels(), "b", "被摘除的模型不能复活")
	require.Contains(t, channel.GetModels(), "c", "未被摘除的模型必须留下")

	var abilities []Ability
	require.NoError(t, db.Where("channel_id = ?", 4).Find(&abilities).Error)
	for _, ability := range abilities {
		require.NotEqual(t, "a", ability.Model)
		require.NotEqual(t, "b", ability.Model)
	}
}
