package service

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
)

// AutoModelRetryScope describes how an auto request may advance its route plan.
// It does not change named-model retry behavior or record persistent cooldowns.
type AutoModelRetryScope int

const (
	AutoModelRetryStop AutoModelRetryScope = iota
	AutoModelRetryNextModel
	AutoModelRetryNextChannel
)

// ClassifyAutoModelRetry decides only error eligibility and scope. The caller
// must still enforce dispatch, cancellation, response, lock and budget guards.
func ClassifyAutoModelRetry(err *types.NewAPIError) AutoModelRetryScope {
	if err == nil || types.IsSkipRetryError(err) {
		return AutoModelRetryStop
	}
	status := err.StatusCode
	if status < 100 || status > 599 || (status >= 200 && status < 300) ||
		operation_setting.IsAlwaysSkipRetryStatusCode(status) ||
		operation_setting.IsAlwaysSkipRetryCode(err.GetErrorCode()) {
		return AutoModelRetryStop
	}
	// Explicit channel configuration failures retain their existing failover
	// exception, but cannot bypass skip flags or invalid/success statuses.
	if types.IsChannelError(err) {
		switch err.GetErrorCode() {
		case types.ErrorCodeChannelNoAvailableKey, types.ErrorCodeChannelParamOverrideInvalid,
			types.ErrorCodeChannelHeaderOverrideInvalid, types.ErrorCodeChannelModelMappedError,
			types.ErrorCodeChannelAwsClientError, types.ErrorCodeChannelInvalidKey:
			return AutoModelRetryNextChannel
		}
		if operation_setting.ShouldRetryByStatusCode(status) {
			return AutoModelRetryNextChannel
		}
		return AutoModelRetryStop
	}
	if err.GetErrorType() == types.ErrorTypeNewAPIError {
		switch err.GetErrorCode() {
		case types.ErrorCodeInvalidRequest, types.ErrorCodeConvertRequestFailed,
			types.ErrorCodeReadRequestBodyFailed, types.ErrorCodeBadRequestBody:
			return AutoModelRetryStop
		}
	}
	// 能力不匹配优先于状态码白名单:上游对这类失败常回 400,而 400 默认不在
	// ShouldRetryByStatusCode 里(默认区间是 1xx 与 3xx-5xx),不提前判就根本不会
	// 重试,用户直接吃一个 400。
	//
	// 位置刻意排在前面两个分支之后:它们表达的是"网关自己判定这次请求不成立"
	// (本地请求体构造失败、渠道配置错误、全局 skip 标记),那种情况下没有上游可换,
	// 换模型也没有意义。上游返回的能力错误不会落到 ErrorTypeNewAPIError 上。
	//
	// 作用域固定是"换模型":同一条上游的同名模型通常一样不支持,换渠道是白跑一趟。
	if AutoModelCapabilityFailure(err) != "" {
		return AutoModelRetryNextModel
	}
	modelScoped, incompatible := autoModelRetryModelEvidence(err)
	// Only the former model-switch statuses get a compatibility exception.
	// Model-level permission, rate limit and overload still require opt-in.
	if !operation_setting.ShouldRetryByStatusCode(status) &&
		!(incompatible && autoModelCompatibilityStatus(status)) {
		return AutoModelRetryStop
	}
	if modelScoped {
		return AutoModelRetryNextModel
	}
	return AutoModelRetryNextChannel
}

func autoModelCompatibilityStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed,
		http.StatusUnprocessableEntity, http.StatusNotImplemented:
		return true
	}
	return false
}

// AutoModelCapabilityFailure 判定一次失败是不是"这个模型不支持本次请求需要的能力",
// 返回命中的能力名(tools / vision / json_schema / reasoning / audio),未命中返回空串。
//
// 它同时决定两件事:
//
//   - 冷却档位:命中即按确定性失败处理(首犯 24 小时,再犯摘除该组合)。本网关的
//     auto 主要用于工具调用,一个给不了 tools 的模型对这个用途就是废的;不摘掉它
//     会被反复选中、每次白费一次尝试,也就是"不适配的一直重复运行"。
//   - 重试作用域:换模型而不是换渠道。同一条上游的同名模型通常一样不支持,换渠道
//     是白跑;而 400 默认不在重试状态码区间里,不提前判用户会直接拿到一个 400,
//     连换模型的机会都没有。
//
// 判据范围与其它关键词一致:错误正文、错误码、错误类型三处都看。
func AutoModelCapabilityFailure(err *types.NewAPIError) string {
	if err == nil {
		return ""
	}
	detail := err.ToOpenAIError()
	for _, haystack := range []string{
		err.Error(),
		detail.Message,
		string(err.GetErrorCode()),
		detail.Type,
	} {
		if feature := operation_setting.MatchAutoModelCapabilityText(haystack); feature != "" {
			return feature
		}
	}
	return ""
}

// AutoModelPermanentFailure 判定一次上游失败是不是"确定性"的:这类失败重试也
// 不会好(鉴权/权限、模型不存在、模型不支持、上游网关/CF 层挂掉),应该给
// 永久级长冷却,而不是和限流一样只冷却 15 分钟。
//
// 判据是正面枚举,不做"认不出就当永久":认不出的失败按抖动处理,宁可多试几次,
// 也不误杀一个只是暂时出问题的模型。上下文超长(context_length_exceeded)与请求
// 内容有关,不算模型不存在,因此也不在这里。
//
// 内置判据之外,管理员可以在面板补关键词(一行一个),覆盖上游冒出的新错误串,
// 否则那些失败只会拿到 15 分钟冷却并被反复重试。关键词只升级冷却时长,
// 作用域仍是单个 (group, model, channel)。
//
// "模型不支持本次要的能力"(如 function calling)同样算确定性失败,见下面那条
// AutoModelCapabilityFailure 判据:一个给不了 tools 的模型对本网关的用途就是废的,
// 留着只会被反复选中、反复失败。
func AutoModelPermanentFailure(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	if autoModelProtectedTransient(err) {
		// 保护项优先于任何关键词:上下文超长和泛化请求错误是"这次请求"的问题,
		// 不是"这个模型不存在"。管理员加错关键词也不能把它们变成永久封禁。
		return false
	}
	if autoModelMatchesTransientKeyword(err) {
		// 管理员显式指定"这属于会自愈的普通失败":它优先于下面所有确定性判据。
		// 作用是防止某个其实会恢复的错误(或过宽的固定文案)被误升级成 24 小时
		// 冷却乃至摘除模型。
		return false
	}
	if AutoModelCapabilityFailure(err) != "" {
		// 能力不匹配按确定性失败处理:本网关的 auto 主要用于工具调用,一个给不了
		// tools 的模型对这个用途就是废的,留着只会被反复选中、每次白费一次尝试。
		//
		// 判据用措辞表而不是单条内置关键词,是因为上游表达同一件事的写法太多:
		// "function calling is not supported" / "tool calling is not supported" /
		// "does not support tools" / "不支持工具调用"……单条字符串只能覆盖其中一种,
		// 其余写法会漏进抖动档,于是这个废组合永远删不掉。
		//
		// 位置在抖动关键词之后:管理员把某个能力措辞写进 AutoModelTransientKeywords
		// 就等于明确要求"这一条别删",那是他的判断,应当生效。
		return true
	}
	switch err.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		// 404 / 502 / 503 / 504 都是"这条上游根本给不出这个模型的回答":404 是端点或
		// 模型不存在,后三者是上游网关本身挂了、或者根本答不上来。重试同一个组合
		// 不会变好。它们也可能短暂,但首犯只冷却 24 小时、再犯才摘除,代价可控。
		return true
	case 520, 521, 522, 523, 524, 525, 526, 527:
		// Cloudflare 自有的 5xx:源站返回空/非法响应(520)、连不上(521/522/523)、
		// 超时(524)、SSL 握手失败(525/526)、非法请求(527)。全部是上游侧的问题,
		// 本网关改不了,也不该靠"每 15 分钟再撞一次"等它自愈——那只会让这个组合
		// 反复被选中、反复失败。按确定性失败处理:冷却,再犯摘除,上游修好了再由
		// 管理员加回来。
		//
		// 判状态码而不是关键词:RelayErrorHandler 把这类失败渲染成
		// "bad response status code 524" 这类文案,匹配 "status code: 524" 那种
		// 带冒号的写法永远命中不了。
		return true
	}
	detail := err.ToOpenAIError()
	codes := []string{
		strings.ToLower(strings.TrimSpace(string(err.GetErrorCode()))),
		strings.ToLower(strings.TrimSpace(detail.Type)),
	}
	modelParam := strings.EqualFold(strings.TrimSpace(detail.Param), "model")
	for _, code := range codes {
		switch code {
		case "model_not_found", "model_not_supported", "unsupported_model", "unsupported_model_feature":
			return true
		case "not_found", "invalid_value", "unsupported_value", "unsupported_parameter", "invalid_request":
			// 只有明确指向 model 参数时才算确定性:泛化的 invalid_request
			// (请求体别的字段写错、端点不支持等)不是模型不存在。
			if modelParam {
				return true
			}
		}
	}
	if autoModelMatchesModelUnavailableText(err) {
		return true
	}
	return autoModelMatchesPermanentKeyword(err)
}

// autoModelBuiltinPermanentTexts 是"上游明说这个模型它不提供"的固定文案判据。
//
// 为什么需要它:很多中转上游只回一句自然语言(中文/英文),既不给结构化 code,
// 也不带 param=model。早先这类失败只能落到"抖动失败"的 15 分钟冷却,于是
// 同一个不可用的组合每 15 分钟被选中一次、再失败一次,循环一整天
// (实测 ch11 的 tr-alpha-channel 就是这样被反复选中)。
//
// 这里只收"明确说模型不可用/不存在"的措辞。刻意不收泛化的 400 文案
// (如 invalid_request、Bad Request):那些可能是请求体自己写坏了,不该算到模型头上。
var autoModelBuiltinPermanentTexts = []string{
	"模型不可用",
	"模型不存在",
	"模型未找到",
	"model not found",
	"model is not available",
	"model unavailable",
	"community model not found",
	"no such model",
	"unsupported model",
	"model_not_found",
	"model_not_supported",
}

// autoModelMatchesModelUnavailableText 在错误正文里找"模型不可用"类固定文案。
func autoModelMatchesModelUnavailableText(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	detail := err.ToOpenAIError()
	haystacks := []string{
		strings.ToLower(err.Error()),
		strings.ToLower(detail.Message),
		strings.ToLower(string(err.GetErrorCode())),
		strings.ToLower(detail.Type),
	}
	for _, text := range autoModelBuiltinPermanentTexts {
		for _, haystack := range haystacks {
			if haystack != "" && strings.Contains(haystack, text) {
				return true
			}
		}
	}
	return false
}

// autoModelProtectedTransient 列出永远按抖动处理的失败,即使管理员关键词命中。
// 它描述的是"这次请求/这次输入"而不是"这个模型不存在",冷却一整天是错的:
// context_length_exceeded 只要截断重发就能过,不是模型坏了。
//
// 注意这里只保护上下文超长。泛化 400/422 不在这里,因为它们本来就不会被判永久
// (判据是正面枚举);但如果管理员明确加了某个 400 关键词,那是他的判断,应当生效。
func autoModelProtectedTransient(err *types.NewAPIError) bool {
	detail := err.ToOpenAIError()
	for _, code := range []string{
		strings.ToLower(strings.TrimSpace(string(err.GetErrorCode()))),
		strings.ToLower(strings.TrimSpace(detail.Type)),
	} {
		if code == "context_length_exceeded" {
			return true
		}
	}
	return false
}

// autoModelMatchesTransientKeyword 在错误正文与 code 里找"按普通冷却处理"关键词。
// 命中即强制降级为抖动失败(15m 起步的阶梯冷却),不升级为确定性失败。
func autoModelMatchesTransientKeyword(err *types.NewAPIError) bool {
	keywords := operation_setting.AutoModelTransientKeywords()
	if len(keywords) == 0 {
		return false
	}
	detail := err.ToOpenAIError()
	haystacks := []string{
		strings.ToLower(err.Error()),
		strings.ToLower(detail.Message),
		strings.ToLower(string(err.GetErrorCode())),
		strings.ToLower(detail.Type),
	}
	for _, keyword := range keywords {
		if keyword == "" {
			continue
		}
		for _, haystack := range haystacks {
			if haystack != "" && strings.Contains(haystack, keyword) {
				return true
			}
		}
	}
	return false
}

// autoModelMatchesPermanentKeyword 在错误正文与错误 code 里找管理员关键词。
// 命中范围刻意包含 code,因为很多上游只在结构化字段里给原因。
func autoModelMatchesPermanentKeyword(err *types.NewAPIError) bool {
	keywords := operation_setting.AutoModelPermanentKeywords()
	if len(keywords) == 0 {
		return false
	}
	detail := err.ToOpenAIError()
	haystacks := []string{
		strings.ToLower(err.Error()),
		strings.ToLower(detail.Message),
		strings.ToLower(string(err.GetErrorCode())),
		strings.ToLower(detail.Type),
	}
	for _, keyword := range keywords {
		for _, haystack := range haystacks {
			if haystack != "" && strings.Contains(haystack, keyword) {
				return true
			}
		}
	}
	return false
}

// Evidence must identify a model, not merely a generic invalid request. Param
// alone is insufficient: require a rejection code whose meaning is compatible.
func autoModelRetryModelEvidence(err *types.NewAPIError) (modelScoped, incompatible bool) {
	detail := err.ToOpenAIError()
	codes := []string{strings.ToLower(strings.TrimSpace(string(err.GetErrorCode()))), strings.ToLower(strings.TrimSpace(detail.Type))}
	// Explicit account/auth or transport failures outweigh incidental model text,
	// including contradictory model-specific types supplied by a provider.
	for _, code := range codes {
		switch code {
		case string(types.ErrorCodeDoRequestFailed), "invalid_api_key", "invalid_key",
			"authentication_error", "authentication_failed", "unauthorized", "invalid_authentication",
			"insufficient_quota", "insufficient_balance", "account_deactivated", "account_disabled",
			"account_suspended", "billing_hard_limit_reached", "billing_not_active",
			"account_quota_exceeded", "account_rate_limit_exceeded",
			"organization_quota_exceeded", "organization_rate_limit_exceeded":
			return false, false
		}
	}
	message := strings.ToLower(strings.TrimSpace(detail.Message))
	// A whole-account failure can mention the requested model incidentally.
	// Require an affirmative clause, not merely "account" or "API key" text.
	if autoModelRetryAuthMessage.MatchString(message) {
		return false, false
	}
	modelScoped, channelScoped := autoModelRetryAccountMessageScope(message)
	if channelScoped {
		return false, false
	}
	modelParam := strings.EqualFold(strings.TrimSpace(detail.Param), "model")
	for _, code := range codes {
		switch code {
		case "model_not_found", "model_not_supported", "unsupported_model",
			"context_length_exceeded", "unsupported_model_feature":
			return true, true
		case "model_access_denied", "model_rate_limit_exceeded", "model_overloaded":
			modelScoped = true
		case "not_found", "invalid_value", "unsupported_value", "unsupported_parameter":
			if modelParam {
				return true, true
			}
		case "access_denied", "permission_denied", "rate_limit_exceeded", "overloaded_error":
			modelScoped = modelScoped || modelParam
		}
	}
	if modelScoped {
		return true, false
	}
	if autoModelRetryCompatibilityMessage.MatchString(message) || autoModelRetryContextOverflow(message) {
		return true, true
	}
	return autoModelRetryScopedMessage.MatchString(message), false
}

// A context capacity is not a rejection. Without an explicit overflow claim,
// compare the limit with an adjacent, affirmative requested-token count.
func autoModelRetryContextOverflow(message string) bool {
	for _, match := range autoModelRetryContextCounts.FindAllStringSubmatch(message, -1) {
		maximum, maxErr := strconv.ParseUint(match[1], 10, 64)
		requested, requestedErr := strconv.ParseUint(match[2], 10, 64)
		if maxErr == nil && requestedErr == nil && requested > maximum {
			return true
		}
	}
	return false
}

// Read scope qualifiers across explanatory punctuation before deciding whether
// an account limit is global. Unknown continuations cannot override model codes
// or params, and an explicitly model-only limit is not a compatibility error.
func autoModelRetryAccountMessageScope(message string) (modelScoped, channelScoped bool) {
	// Every clause is a suffix of this normalized, right-trimmed message. Its
	// length identifies its position without hashing/scanning the whole tail.
	memo := make(map[autoModelRetryAccountClauseKey]autoModelRetryAccountClauseResult)
	for _, match := range autoModelRetryAccountStart.FindAllStringSubmatchIndex(message, -1) {
		model, channel := autoModelRetryAccountClauseScope(message[match[1]:], match[2] >= 0, memo)
		if channel {
			return false, true
		}
		modelScoped = modelScoped || model
	}
	return modelScoped, false
}

type autoModelRetryAccountClauseKey struct {
	remaining int
	wide      bool
}

type autoModelRetryAccountClauseResult struct {
	model, channel bool
}

// All account-limit word orders share this scope/completion decision. A rejected
// account clause must not acquire model scope from the generic model matcher.
func autoModelRetryAccountClauseScope(clause string, wide bool, memo map[autoModelRetryAccountClauseKey]autoModelRetryAccountClauseResult) (modelScoped, channelScoped bool) {
	key := autoModelRetryAccountClauseKey{remaining: len(clause), wide: wide}
	if result, ok := memo[key]; ok {
		return result.model, result.channel
	}
	defer func() {
		memo[key] = autoModelRetryAccountClauseResult{model: modelScoped, channel: channelScoped}
	}()
	if !wide {
		if match := autoModelRetryAccountModelRateLimit.FindStringSubmatchIndex(clause); match != nil {
			// The forward form ends at its target; the reversed form ends at
			// its failure predicate. Leave punctuation for the completion check.
			end := match[3]
			if end < 0 {
				end = match[5]
			}
			rest := clause[end:]
			return autoModelRetryAccountAvailableSiblings.MatchString(rest) || autoModelRetryAccountClauseComplete(rest, memo), false
		}
	}
	match := autoModelRetryAccountExhaustionStart.FindStringIndex(clause)
	if match == nil {
		return false, false
	}
	suffix := clause[match[1]:]
	continuation := strings.TrimLeft(suffix, " 	,;:")
	if scope := autoModelRetryAccountModelQualifier.FindStringSubmatch(continuation); scope != nil {
		rest := continuation[len(scope[0]):]
		only := scope[1] != "" || scope[2] != ""
		siblingsAvailable := autoModelRetryAccountAvailableSiblings.MatchString(rest)
		if siblingsAvailable || autoModelRetryAccountClauseComplete(rest, memo) {
			if wide && !only && !siblingsAvailable {
				return false, true
			}
			return true, false
		}
		return false, false
	}
	if scope := autoModelRetryAccountAllModelsQualifier.FindStringIndex(continuation); scope != nil {
		return false, autoModelRetryAccountClauseComplete(continuation[scope[1]:], memo)
	}
	return false, autoModelRetryAccountClauseComplete(suffix, memo)
}

// A period/end finishes a claim. A colon, comma or semicolon only finishes it
// when followed by a separate affirmative clause or incidental model metadata,
// not a scope qualifier, condition or unrecognized explanation.
func autoModelRetryAccountClauseComplete(suffix string, memo map[autoModelRetryAccountClauseKey]autoModelRetryAccountClauseResult) bool {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" || strings.ContainsRune(".!?", rune(suffix[0])) {
		return true
	}
	if !strings.ContainsRune(",;:", rune(suffix[0])) {
		return false
	}
	clause := strings.TrimLeft(suffix, " 	,;:")
	for _, pattern := range autoModelRetryAccountCompletionPatterns {
		if match := pattern.FindStringIndex(clause); match != nil && match[0] == 0 {
			return true
		}
	}
	// Keep the previously accepted account-rate-limit continuation syntax,
	// but let the same account owner validate the entire claim, not its prefix.
	if match := autoModelRetryAccountCompletionStart.FindStringSubmatchIndex(clause); match != nil && match[2] < 0 {
		claim := clause[match[1]:]
		if autoModelRetryAccountModelRateLimit.MatchString(claim) {
			model, channel := autoModelRetryAccountClauseScope(claim, false, memo)
			return model || channel
		}
	}
	return false
}

const autoModelRetryAccountRef = `(?:the |your )?(?:account|organization|organisation|project)`
const autoModelRetryAccountExhaustion = `(?:rate[- ]limit|quota|usage limit|billing limit) (?:(?:has been|is|was) )?(?:exceeded|reached|exhausted)`

var autoModelRetryAccountStart = regexp.MustCompile(
	autoModelRetryClauseStart + autoModelRetryAccountRef + `(?:'s|([- ]wide))? `)
var autoModelRetryAccountExhaustionStart = regexp.MustCompile(`^` + autoModelRetryAccountExhaustion + `\b`)

// These are the account-prefixed rate-limit forms formerly accepted by the
// generic scoped matcher. Syntax recognition alone is not a scope decision.
var autoModelRetryAccountModelRateLimit = regexp.MustCompile(
	`^rate limit (?:(?:exceeded|reached) (?:for|on) (` + autoModelRetryTargetRef + `)(?:[.!?,;:]|$)|` +
		`(?:for|on) ` + autoModelRetryTargetRef + ` (?:has been )?((?:exceeded|reached)\b(?: only)?))`)
var autoModelRetryAccountModelQualifier = regexp.MustCompile(`^(only )?for ` + autoModelRetryTargetRef + `( only)?`)
var autoModelRetryAccountAllModelsQualifier = regexp.MustCompile(`^(?:for|across) all models\b`)
var autoModelRetryAccountAvailableSiblings = regexp.MustCompile(`^[,;:]\s+other models remain available(?:[.!?]|$)`)
var autoModelRetryAccountModelContext = regexp.MustCompile(
	`^(?:requested|selected) model:\s+(?:` + autoModelRetryQuotedRef + `|[a-z0-9]+[-._/][a-z0-9._:/-]+)(?:[.!?;:]|$)`)

var autoModelRetryAuthMessage = regexp.MustCompile(
	autoModelRetryClauseStart + `(?:(?:the |your |provided )?(?:(?:invalid|incorrect|expired|revoked) api[- ]key\b|` +
		`api[- ]key (?:is |was )?(?:invalid|incorrect|expired|revoked)\b)|authentication (?:has )?failed\b)`)

// Require an affirmative clause subject, not a substring of "no model ..." or
// "if this model ...". Recognize explicit feature/parameter subjects as well as
// models, including provider labels followed by a colon. Limit named references
// to quoted or ID-shaped names so "model gateway" is not a model reference.
const autoModelRetryClauseStart = `(?:^|[.!?;:]\s+)`
const autoModelRetryQuotedRef = "[\"'`][^\"'`\\r\\n]{1,120}[\"'`]"
const autoModelRetryModelRef = `\bmodel(?:\s+(?:` + autoModelRetryQuotedRef + `|[a-z0-9]+[-._/][a-z0-9._:/-]+))?`
const autoModelRetryTargetRef = `(?:(?:this|the(?: requested| selected)?|requested|selected)\s+)?` + autoModelRetryModelRef
const autoModelRetryFeatureRef = `(?:` + autoModelRetryQuotedRef + `|(?:(?:this|the)(?: requested)?\s+)?(?:operation|endpoint|feature|parameter|value))`
const autoModelRetryContextLimit = autoModelRetryTargetRef + `(?:'s)? maximum context length`

// Require a complete overflow claim: an unexplained continuation after a colon
// or semicolon may still qualify or contradict the apparent assertion.
const autoModelRetryContextClaimEnd = `(?:[.!?]|$)`

var autoModelRetryContextCounts = regexp.MustCompile(
	autoModelRetryClauseStart + autoModelRetryContextLimit + ` (?:is|of) (\d+) tokens[.,;:]\s+(?:however,?\s+)?you requested (\d+) tokens` + autoModelRetryContextClaimEnd)

var autoModelRetryCompatibilityMessage = regexp.MustCompile(
	autoModelRetryClauseStart + `(?:` + autoModelRetryTargetRef + `\s+(?:(?:is|was)\s+)?(?:not found|not supported|unsupported|does not exist|does not support|doesn't support)\b|` +
		`(?:` + autoModelRetryFeatureRef + `\s+(?:(?:is|was)\s+)?)?(?:not supported|unsupported|not implemented)\s+(?:for|by|with|on)\s+` + autoModelRetryTargetRef + `(?:[.!?,;:]|$)|` +
		`(?:` + autoModelRetryFeatureRef + `\s+)?does not support (?:[^\r\n.!?;:]|\.[0-9]){1,120} with ` + autoModelRetryTargetRef + `(?:[.!?,;:]|$)|` +
		`models/[a-z0-9._:/-]+\s+(?:is\s+)?(?:not found|not supported)\b|` +
		autoModelRetryContextLimit + `(?: of \d+ tokens)? (?:(?:has been|is|was) )?exceeded` + autoModelRetryContextClaimEnd + `)`)

// Account-prefixed limits are owned by autoModelRetryAccountClauseScope, not
// this model-only fallback, even when the account owner found no complete claim.
var autoModelRetryScopedMessage = regexp.MustCompile(
	autoModelRetryClauseStart + `(?:` + autoModelRetryTargetRef + `\s+(?:is\s+)?(?:(?:currently|temporarily)\s+)?(?:overloaded|at capacity|rate[- ]limited)\b|` +
		`access to ` + autoModelRetryTargetRef + ` (?:is )?(?:denied|forbidden)\b|` +
		`(?:access|permission) (?:is )?denied (?:to|for) ` + autoModelRetryTargetRef + `(?:[.!?,;:]|$)|` +
		`(?:you )?(?:do not|don't) have access to ` + autoModelRetryTargetRef + `(?:[.!?,;:]|$)|` +
		`rate limit (?:(?:exceeded|reached) (?:for|on) ` + autoModelRetryTargetRef + `(?:[.!?,;:]|$)|` +
		`(?:for|on) ` + autoModelRetryTargetRef + ` (?:has been )?(?:exceeded|reached)\b))`)

// Completion accepts only index-zero matches. Anchor the same expressions so
// regexp does not scan every remaining clause merely to discard a later match.
var autoModelRetryAccountCompletionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^(?:` + autoModelRetryScopedMessage.String() + `)`),
	regexp.MustCompile(`^(?:` + autoModelRetryCompatibilityMessage.String() + `)`),
	autoModelRetryAccountModelContext,
}
var autoModelRetryAccountCompletionStart = regexp.MustCompile(`^(?:` + autoModelRetryAccountStart.String() + `)`)
