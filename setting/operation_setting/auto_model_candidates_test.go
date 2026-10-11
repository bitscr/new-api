package operation_setting

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 白名单的三种状态必须互相可区分,而且都要能设置:
//
//	分组名不出现  -> 该分组不限制,用它的全部启用模型
//	分组名 + 空数组 -> 该分组一个模型都不选
//	整个配置为空   -> 清空,所有分组回退默认
//
// 前两者在原始 JSON 里长得很像但后果相反,所以这里把三者一起钉住。
func TestAutoModelCandidatesDistinguishesAbsentFromEmpty(t *testing.T) {
	t.Cleanup(func() { _ = SetAutoModelCandidates("") })

	require.NoError(t, SetAutoModelCandidates(""))
	require.Empty(t, GetAutoModelCandidates(), "空配置必须是彻底清空")
	_, configured := GetAutoModelCandidates()["default"]
	require.False(t, configured, "清空后分组不存在,即该分组不限制")

	require.NoError(t, SetAutoModelCandidates("{}"))
	require.Empty(t, GetAutoModelCandidates(), "{} 与空串同义")

	require.NoError(t, SetAutoModelCandidates(`{"default": []}`))
	allowed, configured := GetAutoModelCandidates()["default"]
	require.True(t, configured, "空数组必须是\"已配置\"而不是\"不存在\"")
	require.Empty(t, allowed, "空数组表示该分组一个模型都不选")

	require.NoError(t, SetAutoModelCandidates(`{"default": ["gpt-4o", " gpt-4o ", ""]}`))
	require.Equal(t, []string{"gpt-4o"}, GetAutoModelCandidates()["default"],
		"去空白并去重是既有契约")
}

// 空串和 "{}" 都是 setter 明确接受的输入,所以面板的写库前校验也必须接受它们。
// 这条锁的是 controller 那条分支与 setter 语义一致:早先 controller 无条件
// Unmarshal,空串会报 "unexpected end of JSON input",于是管理员把文本框删空后
// 永远无法把白名单改回不限制。
func TestAutoModelCandidatesEmptyInputIsValid(t *testing.T) {
	t.Cleanup(func() { _ = SetAutoModelCandidates("") })

	for _, value := range []string{"", "   ", "{}", "\n{}"} {
		require.NoError(t, SetAutoModelCandidates(value), "setter 接受 %q", value)
		require.Empty(t, GetAutoModelCandidates())
	}
}

// 权重:0 与缺省语义不同。0 = 该模型退出 auto;缺省 = 权重 1。
func TestAutoModelWeightsZeroDisablesWhileAbsentDefaults(t *testing.T) {
	t.Cleanup(func() { _ = SetAutoModelWeights("") })

	require.NoError(t, SetAutoModelWeights(`{"default": {"gpt-4o": 0, "claude": 2}}`))
	require.Equal(t, 0.0, GetAutoModelWeight("default", "gpt-4o"), "显式 0 表示退出 auto")
	require.Equal(t, 2.0, GetAutoModelWeight("default", "claude"))
	require.Equal(t, DefaultAutoModelWeight, GetAutoModelWeight("default", "unlisted"),
		"未列出的模型保持默认权重")

	require.NoError(t, SetAutoModelWeights(""))
	require.Equal(t, DefaultAutoModelWeight, GetAutoModelWeight("default", "gpt-4o"),
		"清空后回到默认权重,即重新参与 auto")
}

// 非法输入必须被拒绝,并且不能破坏已经在跑的配置。
func TestRejectedAutoModelWeightsLeaveLiveConfigIntact(t *testing.T) {
	t.Cleanup(func() { _ = SetAutoModelWeights("") })

	require.NoError(t, SetAutoModelWeights(`{"default": {"gpt-4o": 3}}`))
	for _, bad := range []string{
		`{"default": {"gpt-4o": -1}}`,
		`{"default": {"gpt-4o": "high"}}`,
		`{"default": null}`,
		`[]`,
		`not json`,
	} {
		require.Error(t, ValidateAutoModelWeights(bad), "%q 必须被拒绝", bad)
	}
	require.Equal(t, 3.0, GetAutoModelWeight("default", "gpt-4o"),
		"校验失败不能清掉正在生效的权重")
}