package operation_setting

import (
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// auto 路由的失败处理没有主动探测:确定性失败给永久级长冷却,抖动失败走短冷却;
// 单请求的换渠道/换模型次数由 auto 专用上限卡住,不再跟着全局 RetryTimes 放大。
//
// 数值都可以在后台面板改(panel 面板写入 DB option 后以 DB 为准);
// 环境变量只作为首次种子,方便自动化部署时给出初始值。
const (
	// autoModelDefaultMaxAttempts 是 auto 单请求的总 dispatch 上限(含首次)。
	autoModelDefaultMaxAttempts = 4
	// autoModelDefaultPermanentCooldownHours / ...MaxDays 是确定性失败的冷却阶梯。
	autoModelDefaultPermanentCooldownHours = 24
	autoModelDefaultPermanentCooldownMaxDays = 30
	// autoModelDefaultScoreDecayMinutes 是评分时间衰减窗口。
	//
	// 它必须比抖动冷却窗口(15 分钟)长,否则冷却一到期,评分已经衰减到中立 0.5,
	// 那个组合会带着"零记忆"回到候选池,惩罚和教训完全脱节。
	autoModelDefaultScoreDecayMinutes = 30
)

var (
	autoModelMaxAttempts            = autoModelDefaultMaxAttempts
	autoModelPermanentCooldownHours = autoModelDefaultPermanentCooldownHours
	autoModelPermanentCooldownMaxDays = autoModelDefaultPermanentCooldownMaxDays
	autoModelScoreDecayMinutes      = autoModelDefaultScoreDecayMinutes
	// autoModelPermanentKeywords 是管理员补充的确定性失败关键词(一行一个)。
	// 命中即把该失败升级为长冷却,作用域仍是单个 (group, model, channel)。
	//
	// 为什么不复用 AutomaticDisableKeywords:那个会禁用整条渠道、需要开启主动
	// 测活,而这里既要"只挡这个组合",也绝不能引入任何主动探测。
	autoModelPermanentKeywords []string
	autoModelSettingsMutex     sync.RWMutex
)

// InitAutoModelSettingsFromEnv 用环境变量给这四个数值打底。只在启动时调用一次。
func InitAutoModelSettingsFromEnv() {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelMaxAttempts = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_MAX_ATTEMPTS", autoModelDefaultMaxAttempts), 1)
	autoModelPermanentCooldownHours = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_PERMANENT_COOLDOWN_HOURS", autoModelDefaultPermanentCooldownHours), 1)
	autoModelPermanentCooldownMaxDays = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_PERMANENT_COOLDOWN_MAX_DAYS", autoModelDefaultPermanentCooldownMaxDays), 1)
	autoModelScoreDecayMinutes = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_SCORE_DECAY_MINUTES", autoModelDefaultScoreDecayMinutes), 1)
}

// sanitizePositive 把不可能的数值收敛到默认值,避免面板写进 0 或负数。
func sanitizePositive(value, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

// AutoModelMaxAttempts 返回 auto 请求允许的总尝试次数(含首次),至少 1。
func AutoModelMaxAttempts() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return autoModelMaxAttempts
}

// SetAutoModelMaxAttempts 面板写入路径。
func SetAutoModelMaxAttempts(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelMaxAttempts = sanitizePositive(value, 1)
}

// AutoModelPermanentCooldownBase 是确定性失败的首次冷却窗口。
func AutoModelPermanentCooldownBase() time.Duration {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return time.Duration(autoModelPermanentCooldownHours) * time.Hour
}

// SetAutoModelPermanentCooldownHours 面板写入路径(单位小时,至少 1)。
func SetAutoModelPermanentCooldownHours(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelPermanentCooldownHours = sanitizePositive(value, 1)
}

// AutoModelPermanentCooldownMax 是确定性失败冷却的封顶值,始终 >= Base。
func AutoModelPermanentCooldownMax() time.Duration {
	autoModelSettingsMutex.RLock()
	maximum := time.Duration(autoModelPermanentCooldownMaxDays) * 24 * time.Hour
	base := time.Duration(autoModelPermanentCooldownHours) * time.Hour
	autoModelSettingsMutex.RUnlock()
	if maximum < base {
		return base
	}
	return maximum
}

// SetAutoModelPermanentCooldownMaxDays 面板写入路径(单位天,至少 1)。
func SetAutoModelPermanentCooldownMaxDays(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelPermanentCooldownMaxDays = sanitizePositive(value, 1)
}

// AutoModelScoreDecayMinutes 是评分线性衰减到中立的窗口(分钟)。
func AutoModelScoreDecayMinutes() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return autoModelScoreDecayMinutes
}

// SetAutoModelScoreDecayMinutes 面板写入路径(分钟,至少 1)。
func SetAutoModelScoreDecayMinutes(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelScoreDecayMinutes = sanitizePositive(value, 1)
}

// AutoModelPermanentKeywords 返回管理员补充判据的副本。
func AutoModelPermanentKeywords() []string {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	if len(autoModelPermanentKeywords) == 0 {
		return nil
	}
	return append([]string(nil), autoModelPermanentKeywords...)
}

// SetAutoModelPermanentKeywords 覆盖关键词列表(一行一个,大小写不敏感)。
func SetAutoModelPermanentKeywords(value string) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelPermanentKeywords = parseAutoModelPermanentKeywords(value)
}

// AutoModelPermanentKeywordsToString 导出为面板可编辑的多行文本。
func AutoModelPermanentKeywordsToString() string {
	return strings.Join(AutoModelPermanentKeywords(), "\n")
}

// ValidateAutoModelPermanentKeyword 在写库前挡住"注定不会生效或被严重滥用"的关键词。
// 返回空字符串表示接受。
//
// 三类必须拒绝:
//  1. 过短(少于 2 个字符):子串匹配下会命中大量无关失败。
//  2. 只描述受保护抖动失败的词(如 context_length_exceeded)。判定里保护项优先于
//     关键词,这种条目永远不生效——接受它只会让管理员以为配好了。
//  3. 泛化到吃掉大半失败文本的词(如 error)。命中即 24h 冷却,这种条目会把几乎
//     任何失败都升级成长冷却,等于把所有组合轮着封一遍。
//
// 只挡这些显然无意义/会误伤的条目,不猜测管理员的意图:具体错误串一律放行,
// 这正是这个功能存在的理由。
func ValidateAutoModelPermanentKeyword(raw string) string {
	keyword := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(raw, "\r")))
	if keyword == "" {
		return ""
	}
	if len([]rune(keyword)) < 2 {
		return "关键词过短(少于 2 个字符)会误伤大量无关失败:" + keyword
	}
	for _, protected := range []string{"context_length_exceeded", "context length exceeded"} {
		if strings.Contains(keyword, protected) {
			return "该关键词只描述受保护的抖动失败,判定里保护项优先,它永远不会生效:" + keyword
		}
	}
	for _, overbroad := range []string{"error", "failed", "失败", "错误"} {
		if keyword == overbroad {
			return "关键词过于宽泛,会把绝大多数无关失败都升级为长冷却:" + keyword
		}
	}
	return ""
}

// parseAutoModelPermanentKeywords 按行切分,去空白、去重、转小写。
func parseAutoModelPermanentKeywords(value string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, line := range strings.Split(value, "\n") {
		keyword := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(line, "\r")))
		if keyword == "" || seen[keyword] {
			continue
		}
		seen[keyword] = true
		result = append(result, keyword)
	}
	return result
}