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
	// autoModelDefaultPermanentCooldownHours 是确定性失败的首次冷却窗口。
	//
	// 不再有"封顶天数":确定性失败只保留两档——首犯 24 小时,再犯即永久剔除。
	// 阶梯式翻倍(24h→48h→…→30 天)对本机的意义不大:需要几十天才收敛的组合,
	// 早该被人工或自动清掉,而不是挂着一个"30 天后还会回来试"的窗口。
	autoModelDefaultPermanentCooldownHours = 24
	// autoModelDefaultCooldownMaxHours 是抖动(普通)冷却阶梯的封顶时长。
	//
	// 阶梯是首档 15 分钟起、每犯一次翻倍:15m→30m→1h→2h→4h→封顶。本值决定封顶。
	autoModelDefaultCooldownMaxHours = 6
	// autoModelDefaultScoreDecayMinutes 是评分时间衰减窗口。
	//
	// 它必须比抖动冷却阶梯里"最靠前的若干档"都长,不能只比第一档(15 分钟)长。
	// 一次失败把分数从 0.5 压到 0.35(见 autoModelScoreAlpha);衰减是线性的:
	//
	//	分数 = 0.5 + (旧分-0.5) * (1 - 已过时间/本窗口)
	//
	// 若窗口只有 30 分钟,那么第 2 次失败的冷却窗口(30 分钟)一到,分数正好
	// 归零回 0.5——它带着"零记忆"回到候选池,前面积累的教训全部作废;第 3 档
	// 之后更是每档都在归零后才解冻,惩罚完全失效。
	//
	// 默认值随封顶走(与 autoModelDefaultCooldownMaxHours 对齐):封顶抬到多久,
	// 记忆就该留多久,否则最高几档解冻时扣分已经是 0,冷却只剩"等时间"。
	// 这两者的关系由配置校验强制(见 controller/option.go)。
	autoModelDefaultScoreDecayMinutes = 360
)

var (
	autoModelMaxAttempts            = autoModelDefaultMaxAttempts
	autoModelPermanentCooldownHours = autoModelDefaultPermanentCooldownHours
	autoModelCooldownMaxHours       = autoModelDefaultCooldownMaxHours
	autoModelScoreDecayMinutes      = autoModelDefaultScoreDecayMinutes
	// autoModelScoreDecayExplicit 记录"衰减窗口是否被显式设过"。
	//
	// 没被显式设过时,它跟着封顶走(改封顶 → 衰减同步跟着改);一旦管理员在面板
	// 单独填过,就以他填的为准,不再被改封顶时的联动覆盖。
	autoModelScoreDecayExplicit bool
	// autoModelPermanentKeywords 是管理员补充的确定性失败关键词(一行一个)。
	// 命中即把该失败升级为长冷却,作用域仍是单个 (group, model, channel)。
	//
	// 为什么不复用 AutomaticDisableKeywords:那个会禁用整条渠道、需要开启主动
	// 测活,而这里既要"只挡这个组合",也绝不能引入任何主动探测。
	autoModelPermanentKeywords []string
	// autoModelTransientKeywords 是管理员指定的"按普通抖动失败处理"关键词(一行一个)。
	//
	// 与上面那张表的分工:
	//   - 确定性表:命中 → 首犯 24h、再犯摘除模型;
	//   - 普通表:命中 → 永远只走抖动冷却(15m 起步的阶梯),绝不升级为确定性失败。
	//
	// 它存在的理由:确定性判据里既有结构化错误码也有固定文案,还可能被管理员写宽。
	// 有了这张表,就能把某个其实会自愈的错误钉在"普通冷却"这一档,防止它被
	// 误升级成 24 小时冷却甚至摘除。
	autoModelTransientKeywords []string
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
	autoModelCooldownMaxHours = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_COOLDOWN_MAX_HOURS", autoModelDefaultCooldownMaxHours), 1)
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

// AutoModelPermanentCooldownMax 返回确定性失败的封顶值。
//
// 语义已改为"首犯 24 小时,再犯即永久剔除":封顶等于首次窗口,不再逐级翻倍。
// 保留这个导出只为不改动 service 层的调用点。
func AutoModelPermanentCooldownMax() time.Duration {
	return AutoModelPermanentCooldownBase()
}

// AutoModelCooldownMax 是抖动冷却阶梯的封顶时长(小时换算)。
func AutoModelCooldownMax() time.Duration {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return time.Duration(autoModelCooldownMaxHours) * time.Hour
}

// SetAutoModelCooldownMaxHours 面板写入路径(单位小时,至少 1)。
//
// 联动规则(与 SetAutoModelScoreDecayMinutes 配合,保证不变式"衰减 >= 封顶"
// 在任何到达顺序下都成立):
//   - 衰减窗口从未被显式设过 → 跟着封顶走,保持"封顶多久就记多久";
//   - 已被显式设过 → 保留他填的值,但若小于新封顶则抬到封顶。
//
// 为什么必须在这里兜底、而不是配套校验拒绝:前端把改动项放在同一批并发 PUT 里,
// 两个键的到达顺序不确定。若只是拒绝,会出现"一个被拒、另一个已生效"的半应用;
// 让联动兜底则两种顺序都收敛到同一结果。
func SetAutoModelCooldownMaxHours(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelCooldownMaxHours = sanitizePositive(value, 1)
	target := autoModelCooldownMaxHours * 60
	if !autoModelScoreDecayExplicit || autoModelScoreDecayMinutes < target {
		autoModelScoreDecayMinutes = target
	}
}

// AutoModelCooldownMaxHours 返回封顶的小时数,供面板与校验使用。
func AutoModelCooldownMaxHours() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return autoModelCooldownMaxHours
}

// AutoModelScoreDecayMinutes 是评分线性衰减到中立的窗口(分钟)。
func AutoModelScoreDecayMinutes() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return autoModelScoreDecayMinutes
}

// SetAutoModelScoreDecayMinutes 面板写入路径(分钟,至少 1)。
//
// 显式设置后置位 autoModelScoreDecayExplicit:此后改封顶不再联动覆盖它。
// 但"记忆窗口小于封顶"这条不变式永远成立:小则抬到封顶,避免最高几档解冻时
// 扣分已是 0、冷却只剩"等时间"。
func SetAutoModelScoreDecayMinutes(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelScoreDecayMinutes = sanitizePositive(value, 1)
	autoModelScoreDecayExplicit = true
	if autoModelScoreDecayMinutes < autoModelCooldownMaxHours*60 {
		autoModelScoreDecayMinutes = autoModelCooldownMaxHours * 60
	}
}

// AutoModelTransientKeywords 返回"按普通冷却处理"判据的副本。
func AutoModelTransientKeywords() []string {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	if len(autoModelTransientKeywords) == 0 {
		return nil
	}
	return append([]string(nil), autoModelTransientKeywords...)
}

// SetAutoModelTransientKeywords 覆盖普通冷却关键词列表(一行一个,大小写不敏感)。
func SetAutoModelTransientKeywords(value string) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelTransientKeywords = parseAutoModelPermanentKeywords(value)
}

// AutoModelTransientKeywordsToString 导出为面板可编辑的多行文本。
func AutoModelTransientKeywordsToString() string {
	return strings.Join(AutoModelTransientKeywords(), "\n")
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