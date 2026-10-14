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

// autoModelDefaultPriorityOverrideMargin 是渠道健康分的"同档宽度"。
//
// 背景:渠道的 priority 是管理员配的硬语义(高优先级 = 更该优先用),但它是静态的;
// 健康分是动态的。只有 priority 而没有这一档规则时,一条 priority=1 的慢渠道会永远
// 排在健康的 priority=2 渠道前面——系统明明已经把它算到 0.42、把后者算到 0.87,
// 却没有权力用这个结论。于是它每 15 分钟冷却到期就归队、又排第一、又慢一次,
// 循环烧掉真实用户请求。这不是"没有数据",是"有数据但没有决策权"。
//
// 0.25 的含义:健康分差距小于 0.25 的两条渠道算"一样健康",这时由 priority 说话;
// 差距超过 0.25,健康分自己分出档来,priority 不再能压住它。健康分的中立点是 0.5、
// 量程是 1.0,所以 0.25 是半个量程的差距——足以区分"这条明显更好"和"只是抖动"。
//
// 这个档宽本身就是迟滞:健康分要跨过一整档才会改变顺序,不会在临界处来回跳,
// 不需要额外的计时器、冷却或滑动窗口来防抖。
//
// 两个端点都自然落位:
//   - 填 0:每个不同的健康分各占一档,顺序完全等于健康分顺序,priority 只用于同分;
//   - 填 >= 1.0(健康分上限是 1.0):所有渠道永远同档,priority 完全决定顺序,
//     也就是回到旧行为。
//
// 两个端点都有一处共同的例外:从未被观测过的渠道始终留在最好的档里,由 priority
// 决定先后。它既不是健康也不是不健康,而是"不知道";本系统不做任何主动探测,证据
// 只能来自真实流量,把它按中性分压下去就永远拿不到证据。所以更准确的说法是
// "健康分在有证据的渠道之间决定顺序"。
const autoModelDefaultPriorityOverrideMargin = 0.25

var (
	autoModelPriorityOverrideMargin = autoModelDefaultPriorityOverrideMargin
	autoModelRoutingSettingsMutex   sync.RWMutex
)

// InitAutoModelRoutingSettingsFromEnv 用环境变量给选路设置打底。只在启动时调用一次。
func InitAutoModelRoutingSettingsFromEnv() {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	raw := strings.TrimSpace(common.GetEnvOrDefaultString("AUTO_MODEL_PRIORITY_OVERRIDE_MARGIN", ""))
	if raw == "" {
		return
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		common.SysError("failed to parse AUTO_MODEL_PRIORITY_OVERRIDE_MARGIN: " + err.Error())
		return
	}
	autoModelPriorityOverrideMargin = sanitizePriorityOverrideMargin(parsed)
}

// AutoModelPriorityOverrideMargin 返回渠道健康分的同档宽度。
func AutoModelPriorityOverrideMargin() float64 {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return autoModelPriorityOverrideMargin
}

// SetAutoModelPriorityOverrideMargin 面板写入路径。
func SetAutoModelPriorityOverrideMargin(value float64) {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	autoModelPriorityOverrideMargin = sanitizePriorityOverrideMargin(value)
}

// ValidateAutoModelPriorityOverrideMargin 在写库前挡住不可能的数值,返回空串表示接受。
//
// 只拒绝负数:负档宽会让分档反过来(健康分更高的渠道反而落到更差的档),
// 语义正好反了,那不是调参而是关掉了这条规则。0 是合法且有意义的选择
// (顺序完全等于健康分顺序),>= 1 也合法(等于回到 priority 完全决定顺序的旧行为),
// 两者都放行。
func ValidateAutoModelPriorityOverrideMargin(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "健康分同档宽度必须是有限数字"
	}
	if value < 0 {
		return "健康分同档宽度不能为负数:负值会让健康分更高的渠道反而落到更差的档,顺序被反转"
	}
	return ""
}

// sanitizePriorityOverrideMargin 把不可能的数值收敛到默认值。
func sanitizePriorityOverrideMargin(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return autoModelDefaultPriorityOverrideMargin
	}
	return value
}
