package service

import (
	"testing"
	"time"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/require"
)

// 起始时间面板可配:阶梯从配置值起步、逐级翻倍、翻倍超出封顶时以封顶为准。
func TestAutoModelCooldownBaseConfigurable(t *testing.T) {
	oldBase := operation_setting.AutoModelCooldownBaseMinutes()
	oldCap := operation_setting.AutoModelCooldownMaxHours()
	t.Cleanup(func() {
		operation_setting.SetAutoModelCooldownBaseMinutes(oldBase)
		operation_setting.SetAutoModelCooldownMaxHours(oldCap)
	})

	// 起始 60 分钟,封顶 24h(默认):60m→120m→240m→…→封顶。
	operation_setting.SetAutoModelCooldownBaseMinutes(60)
	operation_setting.SetAutoModelCooldownMaxHours(24)

	now := time.Now()
	state := autoModelCooldownState{}
	state = nextAutoModelCooldown(state, "请求失败", now)
	require.Equal(t, 60*time.Minute, state.Until.Sub(now), "首犯按配置的起步值冷却")

	state = nextAutoModelCooldown(state, "请求失败", now)
	require.Equal(t, 120*time.Minute, state.Until.Sub(now), "第二次在上一次基础上翻倍")

	state = nextAutoModelCooldown(state, "请求失败", now)
	require.Equal(t, 240*time.Minute, state.Until.Sub(now), "第三次继续翻倍")

	// 起始 40 分钟、封顶 1 小时:首犯 40m;第二次翻倍 80m 超出封顶 → 以封顶 1h 为准;
	// 此后继续犯错也维持封顶。
	operation_setting.SetAutoModelCooldownBaseMinutes(40)
	operation_setting.SetAutoModelCooldownMaxHours(1)

	state = autoModelCooldownState{}
	state = nextAutoModelCooldown(state, "请求失败", now)
	require.Equal(t, 40*time.Minute, state.Until.Sub(now), "首犯 40 分钟(未超封顶,原样)")

	state = nextAutoModelCooldown(state, "请求失败", now)
	require.Equal(t, time.Hour, state.Until.Sub(now), "翻倍 80 分钟超出 1 小时封顶,以封顶为准")

	state = nextAutoModelCooldown(state, "请求失败", now)
	require.Equal(t, time.Hour, state.Until.Sub(now), "后续继续维持封顶")
}

// 校验口子必须跟随面板值,而不是默认 15 分钟。
func TestAutoModelCooldownValidationFollowsConfiguredBase(t *testing.T) {
	oldBase := operation_setting.AutoModelCooldownBaseMinutes()
	t.Cleanup(func() { operation_setting.SetAutoModelCooldownBaseMinutes(oldBase) })

	operation_setting.SetAutoModelCooldownBaseMinutes(45)
	require.Equal(t, 45*time.Minute, AutoModelCooldownBaseForValidation())
	require.Equal(t, 90*time.Minute, AutoModelCooldownSecondTierForValidation(),
		"第二档 = 2×起始值,衰减窗口建议也依赖它")
}
