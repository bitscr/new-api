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
