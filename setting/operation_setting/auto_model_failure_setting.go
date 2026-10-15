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

// AutoModelCapabilityText 是一条"上游明说这个模型不提供某种能力"的判据。
// Feature 是能力名(会出现在日志里,也是将来"按模型记录不支持什么"的键),
// Texts 是大小写不敏感的子串。
type AutoModelCapabilityText struct {
	Feature string
	Texts   []string
}

// autoModelCapabilityTexts 是能力不匹配的措辞表。
//
// 命中后由 service.AutoModelCapabilityFailure 同时决定两件事:按确定性失败处理
// (冷却 → 再犯摘除),以及这次重试换模型而不是换渠道。为什么用一张表而不是往
// 永久关键词里加几条字符串:上游表达同一件事的写法太多
// ("function calling is not supported" / "tool calling is not supported" /
// "does not support tools" / "不支持工具调用"),靠人一条条补必然漏,漏掉的那些
// 会落进抖动档,于是那个废组合永远删不掉。
//
// 表里刻意只收"能力名 + 不支持"连写的措辞。泛化的 unsupported_parameter /
// invalid_request 不收:那些可能是请求体别的字段写错,算到模型头上会误伤。
// 管理员仍可用 AutoModelTransientKeywords 把某个措辞钉回抖动档——那条判据在
// AutoModelPermanentFailure 里排在本表之前,是明确的"这一条别删"。
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

// autoModelBuiltinPermanentKeywords 是"确定性失败"判据的内置初值(本机在用的条目)。
//
// 为什么要有内置初值:这些条目都来自本机日志里实际出现过、且重试永远不会好的失败。
// 没有初值时面板是空的,GET /api/option/ 回显空串,前端再拿空串覆盖自己的预填,
// 管理员看到的就是空框;或者前端与后端各写一份,时间一长两边不一致。
// 所以以这里为唯一来源:未配置时即用初值,GET 回显也就是初值。
//
// 判定方式是在"错误正文 + 错误码 + 错误类型"里做大小写不敏感的子串匹配,
// 所以中文文案、英文词语、状态码三种写法都可写。
//
// 这里收 "function calling is not supported" 一类的能力措辞:本网关的 auto 主要
// 用于工具调用,一个给不了 tools 的模型对这个用途就是废的,应当按确定性失败处理
// (冷却 → 再犯摘除),而不是留着被反复选中、每次白费一次尝试。
// AutoModelCapabilityFailure 另有一份更宽的能力措辞表,但它只影响重试换哪个候选,
// 不参与这里的冷却档位判定。
var autoModelBuiltinPermanentKeywords = []string{
	// 上游明说"这个模型我不提供"——结构化 code 缺失时的自然语言兜底。
	// 实测 ch11 的 tr-alpha-channel 就是反复撞这一类,却只能吃 15 分钟抖动冷却。
	"模型不可用",
	"模型不存在",
	"model not found",
	"community model not found",
	// 已实证会复发:某个上游确实不支持的确定性能力。
	"function calling is not supported",
	// 账号/密钥级确定性失败:换模型也一样,重试没有意义。
	"invalid_api_key",
	"invalid_authentication",
	// 明确的状态码(判定用子串匹配 "status code: 404" 这类文本)。
	"status code: 404",
}

// autoModelBuiltinTransientKeywords 是"按普通冷却处理"判据的内置初值(本机在用)。
//
// 命中即强制降级为抖动冷却(15m 起步的阶梯),不升级为确定性失败。它优先级最高,
// 用来把其实会自愈、却被上面那张表或结构化判据误伤的错误钉回普通档。
//
// 520/524 不在这里:它们是上游/CF 侧的问题,本网关改不了,靠"每 15 分钟再撞一次"
// 等它自愈只会让同一个组合反复失败。现在由 AutoModelPermanentFailure 的状态码
// 分支直接判成确定性失败(冷却 → 再犯摘除)。
//
// 早先这里写的是 "status code: 520" 这样的字符串,但那串**从来没生效过**:
// RelayErrorHandler 把这类失败渲染成 "bad response status code 520",没有冒号,
// 子串匹配不上。它们之所以表现成抖动冷却,只是因为认不出的失败本来就默认走抖动档。
var autoModelBuiltinTransientKeywords = []string{
	// 限流是明确的临时状态,冷却阶梯本来就更适合它。
	"rpm exhausted",
	"token plan entitlement exhausted",
	// 上游自己的容量/健康问题,不是模型不存在。
	"all candidates failed",
	"upstream_response_timeout",
}

var (
	autoModelMaxAttempts            = autoModelDefaultMaxAttempts
	autoModelPermanentCooldownHours = autoModelDefaultPermanentCooldownHours
	autoModelCooldownMaxHours       = autoModelDefaultCooldownMaxHours
	autoModelScoreDecayMinutes      = autoModelDefaultScoreDecayMinutes
	// autoModelPermanentKeywords 是管理员补充的确定性失败关键词(一行一个)。
	// 命中即把该失败升级为长冷却,作用域仍是单个 (group, model, channel)。
	//
	// 为什么不复用 AutomaticDisableKeywords:那个会禁用整条渠道、需要开启主动
	// 测活,而这里既要"只挡这个组合",也绝不能引入任何主动探测。
	//
	// 初始值见 autoModelBuiltinPermanentKeywords:面板没有配置时,GET /api/option/
	// 会把这些内置判据回显到输入框,管理员不用手抄。
	autoModelPermanentKeywords []string
	// autoModelTransientKeywords 是"按普通抖动失败处理"的关键词(一行一个)。
	//
	// 命中即强制降级为抖动冷却(15m 起步的阶梯),不升级为确定性失败。
	// 初始值见 autoModelBuiltinTransientKeywords。
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
// 取封顶换算的分钟数,并保证大于冷却第二档(30 分钟):窗口若等于第二档,
// 第 2 次失败解冻时分数正好归零,惩罚精确作废。
func AutoModelRecommendedScoreDecayMinutes() int {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	minutes := autoModelCooldownMaxHours * 60
	if minutes <= 30 {
		return 31
	}
	return minutes
}

// AutoModelScoreDecayMinutes 是评分线性衰减到中立的窗口(分钟)。
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

// AutoModelTransientKeywords 返回"按普通冷却处理"判据的副本。
//
// 未配置过时返回内置初值:面板回显与判定用同一份数据,管理员打开就能看到
// 本机在用的条目,不用手抄。
func AutoModelTransientKeywords() []string {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	if len(autoModelTransientKeywords) == 0 {
		return append([]string(nil), autoModelBuiltinTransientKeywords...)
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
//
// 未配置过时返回内置初值(理由同 AutoModelTransientKeywords)。
func AutoModelPermanentKeywords() []string {
	autoModelSettingsMutex.RLock()
	defer autoModelSettingsMutex.RUnlock()
	if len(autoModelPermanentKeywords) == 0 {
		return append([]string(nil), autoModelBuiltinPermanentKeywords...)
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