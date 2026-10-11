package controller

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// assertNumericRatioData 保证同步给前端的 ratioTypes 下的值全部是有限数字。
// 前端把非数字值当倍率写入配置时会产生 NaN → JSON null，进而报
// “配置 ModelRatio 必须是模型名称到有限数字的 JSON 对象”，这里是那条不变量的守卫。
func assertNumericRatioData(t *testing.T, data map[string]any) {
	t.Helper()
	for _, ratioType := range ratioTypes {
		raw, ok := data[ratioType]
		if !ok {
			continue
		}
		items, ok := raw.(map[string]any)
		require.Truef(t, ok, "%s 必须是对象", ratioType)
		for name, value := range items {
			number, isNumber := value.(float64)
			require.Truef(t, isNumber, "%s 里的 %s 不是数字: %#v", ratioType, name, value)
			require.Falsef(t, math.IsNaN(number) || math.IsInf(number, 0), "%s 里的 %s 不是有限数字", ratioType, name)
		}
	}
}

// TestParseRatioPayloadOfficialPresetBilling 锁定官方倍率预设（ratio_config-v1-base.json）的格式：
// 它现在只给 billing_expr/billing_mode，必须走表达式通道，绝不返回数值倍率。
func TestParseRatioPayloadOfficialPresetBilling(t *testing.T) {
	payload, dropped, errMsg := parseRatioPayload(json.RawMessage(`{
		"billing_expr": {
			"MiniMax-M2": "tier(\"standard\", p * 0.3 + c * 1.2)",
			"claude-opus-4-6": "len <= 512000 ? tier(\"standard\", p * 5 + c * 25 + cr * 0.5 + cc * 6.25 + cc1h * 10) : tier(\"standard\", p * 10 + c * 37.5 + cr * 1 + cc * 12.5 + cc1h * 20)",
			"LongCat-Flash": "tier(\"standard\", p * 0.75 + cr * 0.015 + c * 2.95)"
		},
		"billing_mode": {"MiniMax-M2": "tiered_expr", "claude-opus-4-6": "tiered_expr", "LongCat-Flash": "tiered_expr"}
	}`))

	require.Empty(t, errMsg, "官方预设的 billing_expr 格式必须被识别，不能再报不支持")
	require.Zero(t, dropped)
	require.Empty(t, payload.Ratios, "表达式不能当成数值倍率返回")
	require.Len(t, payload.BillingExpr, 3)
	require.Contains(t, payload.BillingExpr["MiniMax-M2"], "p * 0.3")
	require.Equal(t, "tiered_expr", payload.BillingMode["claude-opus-4-6"])
}

// 上游没给 billing_mode 时按 tiered_expr 处理，空表达式直接丢掉。
func TestParseRatioPayloadBillingNormalization(t *testing.T) {
	payload, _, errMsg := parseRatioPayload(json.RawMessage(`{
		"billing_expr": {"a": "tier(\"standard\", p * 0.3)", "b": "   "},
		"billing_mode": {"a": ""}
	}`))

	require.Empty(t, errMsg)
	require.Len(t, payload.BillingExpr, 1)
	require.Equal(t, "tiered_expr", payload.BillingMode["a"])
	require.NotContains(t, payload.BillingExpr, "b")
}

func TestParseRatioPayloadType1Numeric(t *testing.T) {
	payload, dropped, errMsg := parseRatioPayload(json.RawMessage(`{
		"model_ratio": {"gpt-4o": 2.5},
		"completion_ratio": {"gpt-4o": 4},
		"model_price": {}
	}`))

	require.Empty(t, errMsg)
	require.Zero(t, dropped)
	require.Empty(t, payload.BillingExpr)
	require.InDelta(t, 2.5, payload.Ratios["model_ratio"].(map[string]any)["gpt-4o"], 1e-9)
	require.InDelta(t, 4.0, payload.Ratios["completion_ratio"].(map[string]any)["gpt-4o"], 1e-9)
	assertNumericRatioData(t, payload.Ratios)
}

// 混合响应：同时带数值倍率和 billing_expr 时，数值倍率优先，表达式不参与 diff。
func TestParseRatioPayloadHybridPrefersNumeric(t *testing.T) {
	payload, dropped, errMsg := parseRatioPayload(json.RawMessage(`{
		"model_ratio": {"gpt-4o": 2.5},
		"billing_expr": {"gpt-4o": "tier(\"standard\", p * 0.3)"}
	}`))

	require.Empty(t, errMsg)
	require.Zero(t, dropped)
	require.InDelta(t, 2.5, payload.Ratios["model_ratio"].(map[string]any)["gpt-4o"], 1e-9)
	require.NotContains(t, payload.Ratios, "billing_expr")
	assertNumericRatioData(t, payload.Ratios)
}

// 上游把表达式直接写进 model_ratio：非数字条目被丢弃，不会流到前端。
func TestParseRatioPayloadDropsNonNumericEntries(t *testing.T) {
	payload, dropped, errMsg := parseRatioPayload(json.RawMessage(`{
		"model_ratio": {"gpt-4o": "tier(\"standard\", p * 0.3)", "gpt-4o-mini": 1.5}
	}`))

	require.Empty(t, errMsg)
	require.Equal(t, 1, dropped)
	ratios := payload.Ratios["model_ratio"].(map[string]any)
	require.NotContains(t, ratios, "gpt-4o")
	require.InDelta(t, 1.5, ratios["gpt-4o-mini"], 1e-9)
	assertNumericRatioData(t, payload.Ratios)
}

func TestParseRatioPayloadDropsTypeWhenAllEntriesNonNumeric(t *testing.T) {
	payload, dropped, errMsg := parseRatioPayload(json.RawMessage(`{
		"model_ratio": {"gpt-4o": "tier(\"standard\", p * 0.3)", "gpt-4o-mini": null}
	}`))

	require.Empty(t, errMsg)
	require.Equal(t, 2, dropped)
	require.NotContains(t, payload.Ratios, "model_ratio")
	assertNumericRatioData(t, payload.Ratios)
}

func TestParseRatioPayloadType2PricingList(t *testing.T) {
	payload, dropped, errMsg := parseRatioPayload(json.RawMessage(`[
		{"model_name": "a", "quota_type": 0, "model_ratio": 1.5, "completion_ratio": 2},
		{"model_name": "b", "quota_type": 1, "model_price": 0.02}
	]`))

	require.Empty(t, errMsg)
	require.Zero(t, dropped)
	require.InDelta(t, 1.5, payload.Ratios["model_ratio"].(map[string]any)["a"], 1e-9)
	require.InDelta(t, 2.0, payload.Ratios["completion_ratio"].(map[string]any)["a"], 1e-9)
	require.InDelta(t, 0.02, payload.Ratios["model_price"].(map[string]any)["b"], 1e-9)
	assertNumericRatioData(t, payload.Ratios)
}

// type2 列表里按 billing_mode 分流：tiered_expr 的条目进表达式通道，不混进倍率。
func TestParseRatioPayloadType2SplitsTieredExpr(t *testing.T) {
	payload, dropped, errMsg := parseRatioPayload(json.RawMessage(`[
		{"model_name": "a", "quota_type": 0, "model_ratio": 1.5, "completion_ratio": 2},
		{"model_name": "b", "quota_type": 0, "model_ratio": 9, "billing_mode": "tiered_expr", "billing_expr": "tier(\"standard\", p * 0.3 + c * 1.2)"}
	]`))

	require.Empty(t, errMsg)
	require.Zero(t, dropped)
	require.Len(t, payload.BillingExpr, 1)
	require.Equal(t, "tiered_expr", payload.BillingMode["b"])
	require.NotContains(t, payload.Ratios["model_ratio"].(map[string]any), "b")
	require.InDelta(t, 1.5, payload.Ratios["model_ratio"].(map[string]any)["a"], 1e-9)
	assertNumericRatioData(t, payload.Ratios)
}

func TestParseRatioPayloadUnrecognized(t *testing.T) {
	payload, _, errMsg := parseRatioPayload(json.RawMessage(`{"foo": 1}`))
	require.NotEmpty(t, errMsg)
	require.True(t, strings.Contains(errMsg, "无法解析"), errMsg)
	require.Nil(t, payload.Ratios)
	require.Empty(t, payload.BillingExpr)
}

// mergeBillingSettings：合法表达式写进去，非法表达式跳过并回报，同名模型覆盖、其它模型保留。
func TestMergeBillingSettings(t *testing.T) {
	currentExpr := map[string]string{"keep-me": "tier(\"standard\", p * 1)", "MiniMax-M2": "old"}
	currentMode := map[string]string{"keep-me": "tiered_expr", "MiniMax-M2": "tiered_expr"}

	expr, mode, imported, changed, skipped := mergeBillingSettings(
		currentExpr, currentMode,
		map[string]string{
			"MiniMax-M2": "tier(\"standard\", p * 0.3 + c * 1.2)",
			"broken-one": "p * ",
		},
		map[string]string{"MiniMax-M2": "tiered_expr"},
	)

	require.Equal(t, 1, imported)
	require.Equal(t, 1, changed)
	require.Len(t, skipped, 1)
	require.Equal(t, "broken-one", skipped[0].Model)
	require.NotEmpty(t, skipped[0].Error)
	require.Contains(t, expr["MiniMax-M2"], "p * 0.3")
	require.Equal(t, "old", currentExpr["MiniMax-M2"], "原 map 不应被就地改写")
	require.Equal(t, "tier(\"standard\", p * 1)", expr["keep-me"], "无关模型必须保留")
	require.Equal(t, "tiered_expr", mode["keep-me"])
}

// 值没变时 changed 为 0，避免前端误报“导入了 N 个新模型”。
func TestMergeBillingSettingsCountsUnchanged(t *testing.T) {
	exprMap := map[string]string{"m": "tier(\"standard\", p * 0.3)"}
	modeMap := map[string]string{"m": "tiered_expr"}

	_, _, imported, changed, skipped := mergeBillingSettings(
		map[string]string{"m": "tier(\"standard\", p * 0.3)"},
		map[string]string{"m": "tiered_expr"},
		exprMap, modeMap,
	)

	require.Equal(t, 1, imported)
	require.Zero(t, changed)
	require.Empty(t, skipped)
}
