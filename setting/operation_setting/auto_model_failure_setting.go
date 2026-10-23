package operation_setting

import (
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// auto 路由的失败处理没有主动探测:命中管理员配置的"确定性失败关键词"的组合被
// 直接摘除(从渠道模型列表删除,不再有冷却等待),其余失败走普通(抖动)冷却阶梯;
// 单请求的换渠道/换模型次数由 auto 专用上限卡住,不再跟着全局 RetryTimes 放大。
//
// 数值都可以在后台面板改(panel 面板写入 DB option 后以 DB 为准);
// 环境变量只作为首次种子,方便自动化部署时给出初始值。
// 关键词判据完全以数据库里的值为准:源码不内置任何条目,未配置就等于没有判据。
const (
	// autoModelDefaultMaxAttempts 是 auto 单请求的总 dispatch 上限(含首次)。
	autoModelDefaultMaxAttempts = 4
	// autoModelDefaultCooldownBaseMinutes 是抖动(普通)冷却阶梯的起步时长(分钟)。
	// 阶梯从这里起步、每犯一次翻倍,翻倍超出封顶时以封顶为准。可在面板调整
	// (AutoModelCooldownBaseMinutes)。
	autoModelDefaultCooldownBaseMinutes = 15
	// autoModelDefaultCooldownMaxHours 是抖动(普通)冷却阶梯的封顶时长。
	//
	// 阶梯是首档 15 分钟起、每犯一次翻倍:15m→30m→1h→2h→4h→封顶。本值决定封顶。
	autoModelDefaultCooldownMaxHours = 6
	// autoModelDefaultScoreDecayMinutes 是业务证据的时间衰减窗口。
	//
	// 可靠性计数和流式/非流式延迟样本都按已过时间线性折算有效数量:
	//
	//	有效数量 = 原始数量 * (1 - 已过时间/本窗口)
	//
	// 延迟数值本身不衰减,证据陈旧时由新鲜度判定为未知。窗口必须比抖动冷却
	// 阶梯里"最靠前的若干档"都长,不能只比第一档(15 分钟)长。若窗口只有
	// 30 分钟,第 2 次失败的冷却窗口(30 分钟)一到,证据已基本归零,组合会带着
	// "零记忆"回到候选池,前面积累的教训全部作废;第 3 档之后更是每档都在
	// 归零后才解冻,惩罚完全失效。
	//
	// 默认值随封顶走(与 autoModelDefaultCooldownMaxHours 对齐):封顶抬到多久,
	// 记忆就该留多久,否则最高几档解冻时证据已经失效,冷却只剩"等时间"。
	// 这两者的关系由配置校验强制(见 controller/option.go)。
	autoModelDefaultScoreDecayMinutes = 360
	// autoModelDefaultScoreRefreshSeconds 是"auto 路由状态"页面的自动刷新间隔(秒)。
	autoModelDefaultScoreRefreshSeconds = 60
)

// AutoModelCapabilityText 是一条"上游明说这个模型不提供某种能力"的判据。
// Feature 是能力名(会出现在日志里,也是将来"按模型记录不支持什么"的键),
// Texts 是大小写不敏感的子串。
type AutoModelCapabilityText struct {
	Feature string
	Texts   []string
}

// autoModelCapabilityTexts 是能力不匹配的措辞表。
//
// 命中后由 service.AutoModelCapabilityFailure 决定这次重试换模型而不是换渠道;
// 它不参与冷却分档:是否把某个能力措辞当成确定性失败,由管理员配置的关键词决定。
// 重试作用域需要认出它们,是因为上游表达同一件事的写法太多
// ("function calling is not supported" / "tool calling is not supported" /
// "does not support tools" / "不支持工具调用"),换模型才有机会成功。
//
// 表里刻意只收"能力名 + 不支持"连写的措辞。泛化的 unsupported_parameter /
// invalid_request 不收:那些可能是请求体别的字段写错,算到模型头上会误伤。
var autoModelCapabilityTexts = []AutoModelCapabilityText{
	{Feature: "tools", Texts: []string{
		"function calling is not supported",
		"function calling not supported",
		"tool calling is not supported",
		"tools are not supported",
		"tool use is not supported",
		"does not support function calling",
		"does not support tool calling",
		"does not support tools",
		"不支持 function calling",
		"不支持工具调用",
		"不支持函数调用",
	}},
	{Feature: "vision", Texts: []string{
		"image input is not supported",
		"image url is not supported",
		"images are not supported",
		"vision is not supported",
		"does not support image",
		"does not support vision",
		"不支持图片输入",
		"不支持图像",
	}},
	{Feature: "json_schema", Texts: []string{
		"json_schema is not supported",
		"json schema is not supported",
		"structured output is not supported",
		"response_format is not supported",
		"不支持 json_schema",
		"不支持结构化输出",
	}},
	{Feature: "reasoning", Texts: []string{
		"reasoning is not supported",
		"thinking is not supported",
		"does not support reasoning",
		"does not support thinking",
		"不支持推理",
	}},
	{Feature: "audio", Texts: []string{
		"audio input is not supported",
		"audio is not supported",
		"does not support audio",
		"不支持音频",
	}},
}

// AutoModelCapabilityTexts 返回内置措辞表。
//
// 表是只读的常量数据,调用方不得修改;这里返回共享切片而不是深拷贝,
// 因为它不随面板配置变化,复制一份只会让判定与校验各自持有一份可能漂移的副本。
func AutoModelCapabilityTexts() []AutoModelCapabilityText {
	return autoModelCapabilityTexts
}

// MatchAutoModelCapabilityText 在给定文本里找能力不匹配措辞,返回命中的能力名。
// 未命中返回空串。判定与面板校验共用,避免两处各写一份措辞而悄悄失配。
func MatchAutoModelCapabilityText(haystack string) string {
	haystack = strings.ToLower(haystack)
	if haystack == "" {
		return ""
	}
	for _, entry := range autoModelCapabilityTexts {
		for _, text := range entry.Texts {
			if strings.Contains(haystack, text) {
				return entry.Feature
			}
		}
	}
	return ""
}

var (
	autoModelMaxAttempts         = autoModelDefaultMaxAttempts
	autoModelCooldownMaxHours    = autoModelDefaultCooldownMaxHours
	autoModelCooldownBaseMinutes = autoModelDefaultCooldownBaseMinutes
	autoModelScoreDecayMinutes   = autoModelDefaultScoreDecayMinutes
	autoModelScoreRefreshSeconds = autoModelDefaultScoreRefreshSeconds
	// autoModelPermanentKeywords 是管理员配置的确定性失败关键词(一行一个)。
	// 命中即把该 (group, model, channel) 组合直接摘除。
	//
	// 值只来自数据库:源码不内置任何条目,未配置就等于没有确定性判据。
	// 为什么不复用 AutomaticDisableKeywords:那个会禁用整条渠道、需要开启主动
	// 测活,而这里既要"只挡这个组合",也绝不能引入任何主动探测。
	autoModelPermanentKeywords []string
	// autoModelTransientKeywords 是"按普通抖动失败处理"的关键词(一行一个)。
	//
	// 命中即强制降级为抖动冷却(15m 起步的阶梯),不升级为确定性失败。
	// 值只来自数据库,源码不内置任何条目。
	autoModelTransientKeywords []string
	autoModelSettingsMutex     sync.RWMutex
)

// InitAutoModelSettingsFromEnv 用环境变量给这些数值打底。只在启动时调用一次。
// 关键词不在这里:它们只来自数据库,由面板写入。
func InitAutoModelSettingsFromEnv() {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelMaxAttempts = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_MAX_ATTEMPTS", autoModelDefaultMaxAttempts), 1)
	autoModelCooldownMaxHours = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_COOLDOWN_MAX_HOURS", autoModelDefaultCooldownMaxHours), 1)
	autoModelCooldownBaseMinutes = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_COOLDOWN_BASE_MINUTES", autoModelDefaultCooldownBaseMinutes), 1)
	autoModelScoreDecayMinutes = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_SCORE_DECAY_MINUTES", autoModelDefaultScoreDecayMinutes), 1)
	autoModelScoreRefreshSeconds = sanitizePositive(
		common.GetEnvOrDefault("AUTO_MODEL_SCORE_REFRESH_SECONDS", autoModelDefaultScoreRefreshSeconds), 1)
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

// AutoModelCooldownMax 是抖动冷却阶梯的封顶时长(小时换算)。
func AutoModelCooldownMax() time.Duration {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return time.Duration(autoModelCooldownMaxHours) * time.Hour
}

// SetAutoModelCooldownMaxHours 面板写入路径(单位小时,至少 1)。
//
// 只存值,不做任何联动改写:封顶与衰减窗口都是可自定义的,管理员想看什么组合
// 就存什么组合。不合逻辑(衰减 < 封顶)时由写入方返回"建议值"提示,而不是
// 悄悄把用户填的值改掉——改掉会让人以为自己填的生效了,反而更迷惑。
func SetAutoModelCooldownMaxHours(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelCooldownMaxHours = sanitizePositive(value, 1)
}

// AutoModelCooldownMaxHours 返回封顶的小时数,供面板与校验使用。
func AutoModelCooldownMaxHours() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return autoModelCooldownMaxHours
}

// SetAutoModelCooldownBaseMinutes 面板写入路径(单位分钟,至少 1)。
// 阶梯从这里起步、每犯一次翻倍;翻倍超出封顶时以封顶为准(封顶语义见
// SetAutoModelCooldownMaxHours)。
func SetAutoModelCooldownBaseMinutes(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelCooldownBaseMinutes = sanitizePositive(value, 1)
}

// AutoModelCooldownBaseMinutes 返回抖动冷却阶梯的起步时长(分钟)。
func AutoModelCooldownBaseMinutes() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return autoModelCooldownBaseMinutes
}

// AutoModelRecommendedCooldownMaxHours 给出"与当前衰减窗口相配"的封顶建议值(小时)。
//
// 取衰减窗口换算的小时数:封顶不该超过记忆窗口,否则最高几档解冻时扣分已是 0,
// 冷却只剩"等时间"。至少 1 小时。
func AutoModelRecommendedCooldownMaxHours() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	hours := autoModelScoreDecayMinutes / 60
	if hours < 1 {
		return 1
	}
	return hours
}

// AutoModelRecommendedScoreDecayMinutes 给出"与当前封顶相配"的衰减窗口建议值(分钟)。
//
// 取封顶换算的分钟数,并保证大于冷却第二档(2×起步值):窗口若等于第二档,
// 第 2 次失败解冻时分数正好归零,惩罚精确作废。
func AutoModelRecommendedScoreDecayMinutes() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	minutes := autoModelCooldownMaxHours * 60
	secondTier := autoModelCooldownBaseMinutes * 2
	if minutes <= secondTier {
		return secondTier + 1
	}
	return minutes
}

// AutoModelScoreDecayMinutes 是业务证据有效数量的线性衰减窗口(分钟)。
func AutoModelScoreDecayMinutes() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return autoModelScoreDecayMinutes
}

// SetAutoModelScoreDecayMinutes 面板写入路径(分钟,至少 1)。
//
// 只存值,不做任何联动改写(理由同 SetAutoModelCooldownMaxHours):
// 不合逻辑时由写入方给建议,不替用户改值。
func SetAutoModelScoreDecayMinutes(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelScoreDecayMinutes = sanitizePositive(value, 1)
}

// AutoModelScoreRefreshSeconds 是"auto 路由状态"页面的自动刷新间隔(秒,至少 1)。
func AutoModelScoreRefreshSeconds() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	return autoModelScoreRefreshSeconds
}

// SetAutoModelScoreRefreshSeconds 面板写入路径(秒,至少 1)。
func SetAutoModelScoreRefreshSeconds(value int) {
	autoModelSettingsMutex.Lock()
	defer autoModelSettingsMutex.Unlock()
	autoModelScoreRefreshSeconds = sanitizePositive(value, 1)
}

// AutoModelTransientKeywords 返回"按普通冷却处理"判据的副本(数据库里配置的值)。
func AutoModelTransientKeywords() []string {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
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

// AutoModelPermanentKeywords 返回管理员配置的确定性失败关键词的副本
// (数据库里配置的值;未配置时为空,判定不命中任何条目)。
func AutoModelPermanentKeywords() []string {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
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
//  3. 泛化到吃掉大半失败文本的词(如 error)。命中即直接摘除该组合,这种条目会把
//     几乎任何失败都判成确定性失败,等于把所有组合轮着删一遍。
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
			return "关键词过于宽泛,会把绝大多数无关失败都判成确定性失败并摘除组合:" + keyword
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
