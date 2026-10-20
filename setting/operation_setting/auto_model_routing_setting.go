package operation_setting

import (
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// auto 的选路质量设置。
//
// 与"失败处理"(auto_model_failure_setting.go)刻意分开:那边回答的是"这次请求
// 失败了怎么办",这里回答的是"下一次请求该先打谁"。调参时的心智完全不同,
// 混在一个文件里会让只想调选路的人先翻过一堆冷却阶梯。

// autoModelDefaultScoreBandWidth 是"模型得分同档宽度"的默认值。
//
// auto 的排序规则是:渠道 priority 降序(硬边界)→ 每个优先级内按模型有效得分
// 分档 → 档内保留渠道份额的两级加权随机。本值决定"分多近的分数算同一档":
//
//   - 每一档锚定在"尚未分档路线里的最高分",与它差距 <= 档宽的路线同档
//     (边界含恰好等于档宽),对剩余路线重复此过程;
//   - 同档以内精确分数不再决定先后,只按权重随机;
//   - 档宽 0 = 仅同分随机(浮点比较保留 1e-9 数值容差);
//   - 档宽 >= 1 = 同优先级内所有正常得分同档,优先级完全决定分层;
//   - 负数、NaN、无穷大非法:写库前校验拒绝,运行时回退默认值并记录错误。
//
// 它替代了更早的"渠道健康分同档宽度"(AutoModelPriorityOverrideMargin):旧配置
// 作用于渠道层健康分,渠道聚合健康分链路已随模型得分分档方案整体移除;历史
// 数据库值由 model/option.go 的一次性迁移复制到本键,运行时不做新旧键双重解释。
const autoModelDefaultScoreBandWidth = 0.01

var (
	autoModelScoreBandWidth       = autoModelDefaultScoreBandWidth
	autoModelRoutingSettingsMutex sync.RWMutex
)

// InitAutoModelRoutingSettingsFromEnv 用环境变量给选路设置打底。只在启动时调用一次。
// 只接受新键 AUTO_MODEL_SCORE_BAND_WIDTH;数据库配置在加载后覆盖环境变量,
// 历史旧键由 model/option.go 的一次性迁移处理,不再作为环境变量回退来源。
func InitAutoModelRoutingSettingsFromEnv() {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	raw := strings.TrimSpace(common.GetEnvOrDefaultString("AUTO_MODEL_SCORE_BAND_WIDTH", ""))
	if raw == "" {
		return
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		common.SysError("failed to parse AUTO_MODEL_SCORE_BAND_WIDTH: " + err.Error())
		autoModelScoreBandWidth = autoModelDefaultScoreBandWidth
		return
	}
	autoModelScoreBandWidth = sanitizeAutoModelScoreBandWidth(parsed)
}

// AutoModelScoreBandWidth 返回模型有效得分的同档宽度。
func AutoModelScoreBandWidth() float64 {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return autoModelScoreBandWidth
}

// SetAutoModelScoreBandWidth 面板写入路径(新键)。
func SetAutoModelScoreBandWidth(value float64) {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	autoModelScoreBandWidth = sanitizeAutoModelScoreBandWidth(value)
}

// ValidateAutoModelScoreBandWidth 在写库前挡住不可能的数值,返回空串表示接受。
//
// 只拒绝负数与 NaN/Inf:负档宽会让分档边界反过来(得分更高的路线反而落到
// 更差的档),语义正好反了。0 是合法且有意义的选择(仅同分随机),>= 1 也合法
// (同优先级内全部同档,优先级完全决定分层),两者都放行。
func ValidateAutoModelScoreBandWidth(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "模型得分同档宽度必须是有限数字"
	}
	if value < 0 {
		return "模型得分同档宽度不能为负数:负值会让分档边界反向,得分更高的路线反而落到更差的档"
	}
	return ""
}

// sanitizeAutoModelScoreBandWidth 把不可能的数值收敛到默认值。
func sanitizeAutoModelScoreBandWidth(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return autoModelDefaultScoreBandWidth
	}
	return value
}
