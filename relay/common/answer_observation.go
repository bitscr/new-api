package common

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	basecommon "github.com/QuantumNous/new-api/common"
)

// 背景（实测 2026-10-07）：
//
// 免费中转会在正文位置塞一条"不是回答"的告警横幅，而 HTTP 状态码依然是 200。
// 客户端侧只能看到被中转自己截断的半句，实测样本：
//
//	data: {"choices":[{"delta":{"content":"\n\n[Gateway Warning: 当前渠道负…
//
// 这种"200 但客户端拿不到答案"的请求，旧逻辑会当成"快速成功"喂给 auto 的成绩表，
// 于是 auto 越选越多这个渠道的模型，而调用方每次都拿到假答案。这里只做一件事：
// 从写给客户端的响应体里判断这次到底有没有有效回答。
const answerSampleLimit = 120

// Retain only possible banner content, never an unbounded completion or a map
// sized by an upstream choice index. Exceeding either limit is inconclusive.
const (
	answerChoiceLimit  = 16
	answerContentLimit = 4096
)

// answerBannerPatterns 识别网关告警横幅。只有"整段正文只剩横幅"才算无效回答，
// 所以规则宁可窄一点：横幅 + 其它正文 = 正常回答（模型可能只是在引用这句话）。
//
// 注意最后一组：实测样本的横幅是被上游截断的，**没有闭合方括号**
// （"\n\n[Gateway Warning: 当前渠道负" 就到头了），只匹配闭合形式的规则会漏判。
var answerBannerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)[\[【]\s*(?:gateway\s+)?(?:warning|warn)\b[^\]】]*[\]】]`),
	regexp.MustCompile(`(?is)[\[【]\s*(?:gateway\s+)?(?:notice|alert)\b[^\]】]*[\]】]`),
	regexp.MustCompile(`(?is)[\[【]\s*(?:网关)?(?:告警|警告)[^\]】]*[\]】]`),
	regexp.MustCompile(`(?is)[\[【]\s*upstream\s+error\s*[:：][^\]】]*[\]】]`),
	regexp.MustCompile(`(?is)^\s*(?:gateway\s+)?(?:warning|warn)\s*[:：][^\n]*`),
	// 未闭合：从方括号一路到行尾/文本末尾，中间不能再出现闭合方括号
	// An unfinished Upstream Error label and body must both exclude CR/LF.
	regexp.MustCompile(`(?is)[\[【][ 	\f]*upstream[ 	\f]+error[ 	\f]*[:：][^\]】\r\n]*$`),
	regexp.MustCompile(`(?is)[\[【]\s*(?:gateway\s+)?(?:warning|warn)\b[^\]】\n]*$`),
	regexp.MustCompile(`(?is)[\[【]\s*(?:网关)?(?:告警|警告)\s*[:：]?[^\]】\n]*$`),
}

// answerChoiceBody 只取判断需要的字段；content 可能是字符串或分片数组，
// 所以用 RawMessage，flattenAnswerContent 会统一成字符串。
type answerChoiceBody struct {
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        json.RawMessage `json:"reasoning"`
	ToolCalls        json.RawMessage `json:"tool_calls"`
	FunctionCall     json.RawMessage `json:"function_call"`
}

type answerChoiceObservation struct {
	index      int
	content    []byte
	prefix     string
	prefixDone bool
}

// A cheap incremental prefix gate, not the banner grammar. A completed prefix
// only permits bounded retention; stripAnswerBanners makes the final decision.
// Keep this a superset of the starts accepted by answerBannerPatterns.
var answerBannerPrefixes = []string{
	"[warn", "[gateway warn", "[notice", "[gateway notice", "[alert", "[gateway alert",
	"[告警", "[警告", "[网关告警", "[网关警告", "[upstream error",
	"warn", "gateway warn",
}

type answerObservation struct {
	mu             sync.Mutex
	parsed         bool // 见过结构可识别的完成体（OpenAI choices 或原生协议正文通道）
	protocolError  bool // 显式上游协议错误，即使已有部分正文也不能算成功
	textSeen       bool // 横幅之外还有正文
	reasoningSeen  bool // 见过思考内容
	toolsSeen      bool // 见过工具调用
	undecidable    bool // 截断/过滤、选择索引不明或观察超限：不下结论
	deliveryBroken bool // 有一批字节没能写完：客户端在交付完成前断开
	// exhausted 由 streamScanner 在上游正常收尾(EOF/DONE 且无软错误)时置位。
	// 它回答的是"上游这条流是不是已经结束",而不是"有没有内容",所以零内容流
	// 同样会置位,供 ClientAnswerUnusable 区分"结束且为空"和"还没走到这里"。
	exhausted bool
	choices   []answerChoiceObservation
	// nativeTexts 按原生协议的正文通道累积文本片段。不能逐包判断“非空文本”：
	// 网关告警横幅完全可能被拆成多个 SSE delta，逐包都会被当成正文。
	nativeTexts map[string]answerChoiceObservation
}

// MarkClientDeliveryBroken 记录"有一批写给客户端的字节没能完整送达"。
//
// 这是判断客户端是否在拿到完整回答之前断开的唯一可靠信号。只看
// c.Request.Context().Err() 会误判：客户端收完正文后正常关闭连接（curl、SDK、
// 下位机都是收到完整响应立即断开）时 ctx 同样变成 canceled，于是"客户端已断开"
// 永远为真，所有 auto 反馈都被丢弃：分数不再更新，慢成功也不再触发冷却。
func (info *RelayInfo) MarkClientDeliveryBroken() {
	if info == nil || info.answer == nil {
		return
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.deliveryBroken = true
}

// ClientDeliveryBroken 报告本次尝试的响应是否有字节没能写完。
func (info *RelayInfo) ClientDeliveryBroken() bool {
	if info == nil || info.answer == nil {
		return false
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.deliveryBroken
}

// MarkUpstreamContentExhausted 记录"上游这条流已经正常收尾"(EOF/[DONE],且没有
// 软错误)。它不表示有内容:整条流一个 chunk 都没有时也会走到这里。
func (info *RelayInfo) MarkUpstreamContentExhausted() {
	if info == nil || info.answer == nil {
		return
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.exhausted = true
}

// UpstreamContentExhausted 报告上游是否已经正常收尾。客户端没有收到任何
// 字节时，若上游也已经正常结束，应优先判定为"上游空响应"，不能误判成
// 客户端主动断开；只有上游尚未收尾时，才把零交付视为客户端提前离开。
func (info *RelayInfo) UpstreamContentExhausted() bool {
	if info == nil || info.answer == nil {
		return false
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.exhausted
}

// ObserveClientAnswer 观察一段即将写给客户端的数据（SSE 行或整包 JSON）。
func (info *RelayInfo) ObserveClientAnswer(data []byte) {
	if info == nil || len(data) == 0 {
		return
	}
	observation := info.answer
	if observation == nil {
		return // 非 genBaseRelayInfo 构造的 RelayInfo：不观察，也就永远不下判断
	}
	for _, payload := range clientAnswerPayloads(data) {
		info.ObserveUpstreamError(payload)
		observation.observe(payload)
		// OpenAI-shaped payloads keep the stricter banner/credit parser above;
		// observeNativeAnswer checks for a real top-level choices key, not the
		// literal text inside a native answer.
		observation.observeNativeAnswer(payload)
		if info.IsStream && observation.hasValidOutput() {
			info.MarkFirstValidOutput()
		}
	}
}

func (observation *answerObservation) hasValidOutput() bool {
	if observation == nil {
		return false
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.textSeen || observation.reasoningSeen || observation.toolsSeen
}

// ClientAnswerHasValidOutput reports whether this attempt has produced actual
// text, reasoning or a tool call. The auto staging writer uses it to release a
// buffered stream as soon as delivery becomes safe.
func (info *RelayInfo) ClientAnswerHasValidOutput() bool {
	if info == nil {
		return false
	}
	return info.answer.hasValidOutput()
}

// ClientAnswerUndetermined reports that the observation layer could not decide
// whether the delivered bytes contain a usable answer (unknown protocol shape,
// truncated/ambiguous choices, or an observation limit). ClientAnswerUnusable
// deliberately stays false in that state, so the caller must not turn it into a
// success: an unjudgeable answer is neutral evidence, not a healthy delivery.
func (info *RelayInfo) ClientAnswerUndetermined() bool {
	if info == nil || info.answer == nil {
		return true
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.protocolError || observation.textSeen || observation.reasoningSeen || observation.toolsSeen {
		return false
	}
	return !observation.parsed || observation.undecidable
}

// observeNativeAnswer keeps bounded per-channel text observations for native
// Claude/Gemini/Responses frames. A non-empty delta is not enough: the gateway
// banner may be split across several SSE packets, and each fragment looks like
// ordinary text on its own.
func (observation *answerObservation) observeNativeAnswer(payload []byte) {
	if len(payload) == 0 {
		return
	}
	var envelope map[string]any
	if err := basecommon.Unmarshal(payload, &envelope); err != nil {
		return
	}
	if _, openAIChoices := envelope["choices"]; openAIChoices {
		return
	}
	kind, _ := envelope["type"].(string)
	if strings.HasPrefix(kind, "response.") {
		observation.observeResponsesEvent(kind, envelope)
		return
	}
	switch kind {
	case "content_block_delta", "content_block_start":
		observation.observeClaudeEvent(envelope)
		return
	case "message":
		observation.observeClaudeEnvelope(envelope)
		return
	}
	if candidates, ok := envelope["candidates"].([]any); ok {
		observation.observeGeminiCandidates(candidates)
		return
	}
	if output, ok := envelope["output"].([]any); ok {
		observation.observeResponsesOutput(output)
		return
	}
	if content, ok := envelope["content"].([]any); ok {
		observation.markNativeParsed()
		observation.observeClaudeContent(content)
	}
}

func answerKeyPart(value any) string {
	if value == nil {
		return "0"
	}
	return basecommon.Interface2String(value)
}

func (observation *answerObservation) markNativeParsed() {
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.parsed = true
}

func (observation *answerObservation) markNativeReasoning() {
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.parsed = true
	observation.reasoningSeen = true
	observation.choices = nil
	observation.nativeTexts = nil
}

func (observation *answerObservation) markNativeTools() {
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.parsed = true
	observation.toolsSeen = true
	observation.choices = nil
	observation.nativeTexts = nil
}

func (observation *answerObservation) markNativeInline() {
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.parsed = true
	observation.textSeen = true
	observation.choices = nil
	observation.nativeTexts = nil
}

// observeNativeText applies the same prefix/banner grammar used for OpenAI
// choices, but keyed by a protocol text channel.
func (observation *answerObservation) observeNativeText(key, text string) {
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.textSeen || observation.reasoningSeen || observation.toolsSeen {
		return
	}
	observation.parsed = true
	if text == "" {
		return
	}
	if observation.nativeTexts == nil {
		observation.nativeTexts = make(map[string]answerChoiceObservation)
	}
	choice, exists := observation.nativeTexts[key]
	if !exists {
		// Only the first fragment of a channel may drop leading whitespace.
		// Later fragments must be appended verbatim: a split banner such as
		// "[Gateway" + " Warning: busy]" depends on its separating space.
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
		if text == "" {
			return
		}
		if len(observation.nativeTexts) >= answerChoiceLimit {
			observation.undecidable = true
			observation.nativeTexts = nil
			return
		}
	}
	if !choice.acceptPrefix(text) {
		observation.textSeen = true
		observation.choices = nil
		observation.nativeTexts = nil
		return
	}
	if len(text) > answerContentLimit-len(choice.content) {
		observation.undecidable = true
		observation.choices = nil
		observation.nativeTexts = nil
		return
	}
	choice.content = append(choice.content, text...)
	if choice.prefixDone {
		visible, banners := stripAnswerBanners(string(choice.content))
		if banners > 0 && strings.TrimSpace(visible) != "" && !answerTextLooksLikeBannerStart(visible) {
			observation.textSeen = true
			observation.choices = nil
			observation.nativeTexts = nil
			return
		}
	}
	observation.nativeTexts[key] = choice
}

// observeNativeTextSnapshot receives a complete text for one channel (part.done,
// item.done, completed or a non-stream response). It must replace/merge the
// channel rather than append: the standard end events repeat the same full body
// already delivered as deltas, and appending would double-count its length.
func (observation *answerObservation) observeNativeTextSnapshot(key, text string) {
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.textSeen || observation.reasoningSeen || observation.toolsSeen {
		return
	}
	observation.parsed = true
	if text == "" {
		return
	}
	if observation.nativeTexts == nil {
		observation.nativeTexts = make(map[string]answerChoiceObservation)
	}
	existing, exists := observation.nativeTexts[key]
	if !exists && len(observation.nativeTexts) >= answerChoiceLimit {
		observation.undecidable = true
		observation.nativeTexts = nil
		return
	}
	merged := text
	if exists && len(existing.content) > 0 {
		current := string(existing.content)
		switch {
		case strings.HasPrefix(text, current):
			merged = text // snapshot extends the deltas
		case strings.HasPrefix(current, text):
			merged = current // deltas already contain the snapshot
		case len(current) > len(text):
			merged = current // never shrink a more complete channel
		}
	}
	if len(merged) > answerContentLimit {
		observation.undecidable = true
		observation.nativeTexts = nil
		return
	}
	visible, banners := stripAnswerBanners(merged)
	if banners > 0 && strings.TrimSpace(visible) != "" && !answerTextLooksLikeBannerStart(visible) {
		observation.textSeen = true
		observation.choices = nil
		observation.nativeTexts = nil
		return
	}
	if banners == 0 && strings.TrimSpace(visible) != "" && !answerTextLooksLikeBannerStart(visible) {
		observation.textSeen = true
		observation.choices = nil
		observation.nativeTexts = nil
		return
	}
	observation.nativeTexts[key] = answerChoiceObservation{
		content:    []byte(merged),
		prefixDone: true,
	}
}

// --- Claude ---

func (observation *answerObservation) observeClaudeEvent(event map[string]any) {
	index := answerKeyPart(event["index"])
	if delta, ok := event["delta"].(map[string]any); ok {
		switch kind, _ := delta["type"].(string); kind {
		case "text_delta":
			text := stringValue(delta["text"])
			if text == "" {
				observation.markNativeParsed()
			} else {
				observation.observeNativeText("claude:"+index, text)
			}
		case "thinking_delta":
			if strings.TrimSpace(stringValue(delta["thinking"])) != "" {
				observation.markNativeReasoning()
			}
		case "input_json_delta":
			if strings.TrimSpace(stringValue(delta["partial_json"])) != "" {
				observation.markNativeTools()
			}
		}
	}
	if block, ok := event["content_block"].(map[string]any); ok {
		observation.observeClaudeBlock(block, index)
	}
}

func (observation *answerObservation) observeClaudeEnvelope(event map[string]any) {
	if message, ok := event["message"].(map[string]any); ok {
		if content, ok := message["content"].([]any); ok {
			observation.markNativeParsed()
			observation.observeClaudeContent(content)
			return
		}
	}
	if content, ok := event["content"].([]any); ok {
		observation.markNativeParsed()
		observation.observeClaudeContent(content)
	}
}

func (observation *answerObservation) observeClaudeContent(content []any) {
	for index, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		observation.observeClaudeBlock(block, answerKeyPart(index))
	}
}

func (observation *answerObservation) observeClaudeBlock(block map[string]any, key string) {
	switch kind, _ := block["type"].(string); kind {
	case "text":
		text := stringValue(block["text"])
		if text == "" {
			observation.markNativeParsed()
		} else {
			observation.observeNativeText("claude:"+key, text)
		}
	case "thinking":
		if strings.TrimSpace(stringValue(block["thinking"])) != "" {
			observation.markNativeReasoning()
		}
	case "redacted_thinking":
		if strings.TrimSpace(stringValue(block["data"])) != "" {
			observation.markNativeReasoning()
		}
	case "tool_use", "server_tool_use":
		if strings.TrimSpace(stringValue(block["name"])) != "" {
			observation.markNativeTools()
		} else {
			observation.markNativeParsed()
		}
	}
}

// --- Gemini ---

func (observation *answerObservation) observeGeminiCandidates(candidates []any) {
	for candidateIndex, raw := range candidates {
		candidate, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		observation.markNativeParsed()
		content, ok := candidate["content"].(map[string]any)
		if !ok {
			continue
		}
		parts, ok := content["parts"].([]any)
		if !ok {
			continue
		}
		for partIndex, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok {
				if text == "" {
					continue
				}
				if thought, _ := part["thought"].(bool); thought {
					observation.markNativeReasoning()
				} else {
					observation.observeNativeText("gemini:"+answerKeyPart(candidateIndex)+":"+answerKeyPart(partIndex), text)
				}
				continue
			}
			if call, ok := part["functionCall"].(map[string]any); ok {
				if strings.TrimSpace(stringValue(call["name"])) != "" {
					observation.markNativeTools()
				} else {
					observation.markNativeParsed()
				}
				continue
			}
			if inline, ok := part["inlineData"].(map[string]any); ok && strings.TrimSpace(stringValue(inline["data"])) != "" {
				observation.markNativeInline()
				continue
			}
			if file, ok := part["fileData"].(map[string]any); ok && strings.TrimSpace(stringValue(file["fileUri"])) != "" {
				observation.markNativeInline()
			}
		}
	}
}

// --- OpenAI Responses ---

func responsesItemKey(event map[string]any) string {
	// output_index is present on delta, content-part and item events and is the
	// canonical identity. item_id/item.id are only fallbacks for synthetic data.
	if index, ok := event["output_index"]; ok && index != nil {
		return answerKeyPart(index)
	}
	if itemID := stringValue(event["item_id"]); itemID != "" {
		return itemID
	}
	return "0"
}

func responsesItemMapKey(item map[string]any, outputIndex any) string {
	if outputIndex != nil {
		return answerKeyPart(outputIndex)
	}
	if itemID := stringValue(item["id"]); itemID != "" {
		return itemID
	}
	return "0"
}

func (observation *answerObservation) observeResponsesEvent(kind string, event map[string]any) {
	switch kind {
	case "response.output_text.delta":
		delta := stringValue(event["delta"])
		if delta == "" {
			observation.markNativeParsed()
			return
		}
		key := "responses:" + responsesItemKey(event) + ":" + answerKeyPart(event["content_index"])
		observation.observeNativeText(key, delta)
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if strings.TrimSpace(stringValue(event["delta"])) != "" {
			observation.markNativeReasoning()
		}
	case "response.function_call_arguments.delta":
		if strings.TrimSpace(stringValue(event["delta"])) != "" {
			observation.markNativeTools()
		} else {
			observation.markNativeParsed()
		}
	case "response.output_item.added", "response.output_item.done":
		if item, ok := event["item"].(map[string]any); ok {
			// The added event is usually empty and the done event repeats the full
			// item; both are snapshot entries, never additional deltas.
			observation.observeResponsesItem(item, event["output_index"])
		}
	case "response.content_part.added", "response.content_part.done":
		if part, ok := event["part"].(map[string]any); ok {
			key := "responses:" + responsesItemKey(event) + ":" + answerKeyPart(event["content_index"])
			observation.observeResponsesPart(part, key, true)
		}
	case "response.completed":
		observation.markNativeParsed()
		if response, ok := event["response"].(map[string]any); ok {
			if output, ok := response["output"].([]any); ok {
				observation.observeResponsesOutput(output)
			}
		}
	}
}

func (observation *answerObservation) observeResponsesOutput(output []any) {
	observation.markNativeParsed()
	for index, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		observation.observeResponsesItem(item, index)
	}
}

func (observation *answerObservation) observeResponsesItem(item map[string]any, outputIndex any) {
	outputKey := responsesItemMapKey(item, outputIndex)
	switch kind, _ := item["type"].(string); kind {
	case "message":
		observation.markNativeParsed()
		content, ok := item["content"].([]any)
		if !ok {
			return
		}
		for index, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			key := "responses:" + outputKey + ":" + answerKeyPart(index)
			observation.observeResponsesPart(part, key, true)
		}
	case "function_call":
		if strings.TrimSpace(stringValue(item["name"])) != "" {
			observation.markNativeTools()
		} else {
			observation.markNativeParsed()
		}
	case "reasoning":
		observation.markNativeParsed()
		for _, field := range []string{"summary", "content"} {
			parts, ok := item[field].([]any)
			if !ok {
				continue
			}
			for _, rawPart := range parts {
				part, ok := rawPart.(map[string]any)
				if ok && answerTextNonEmpty(stringValue(part["text"])) {
					observation.markNativeReasoning()
					return
				}
			}
		}
	}
}

func (observation *answerObservation) observeResponsesPart(part map[string]any, key string, snapshot bool) {
	switch kind, _ := part["type"].(string); kind {
	case "output_text":
		text := stringValue(part["text"])
		if text == "" {
			observation.markNativeParsed()
			return
		}
		if snapshot {
			observation.observeNativeTextSnapshot(key, text)
		} else {
			observation.observeNativeText(key, text)
		}
	case "refusal":
		refusal := stringValue(part["refusal"])
		if refusal == "" {
			observation.markNativeParsed()
			return
		}
		if snapshot {
			observation.observeNativeTextSnapshot(key, refusal)
		} else {
			observation.observeNativeText(key, refusal)
		}
	case "reasoning_text":
		if strings.TrimSpace(stringValue(part["text"])) != "" {
			observation.markNativeReasoning()
		}
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func answerTextNonEmpty(text string) bool {
	visible, _ := stripAnswerBanners(text)
	return strings.TrimSpace(visible) != ""
}

// answerTextLooksLikeBannerStart reports whether the remaining text could still
// be the beginning of a gateway banner (including a second banner or a partial
// label). Such text is not enough to credit a first valid output.
func answerTextLooksLikeBannerStart(text string) bool {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
	keywords := []string{"gateway", "warning", "warn", "notice", "alert", "upstream", "网关", "告警", "警告"}
	if strings.HasPrefix(lower, "[") || strings.HasPrefix(lower, "【") {
		rest := strings.TrimLeftFunc(lower[1:], unicode.IsSpace)
		if rest == "" {
			return true
		}
		for _, keyword := range keywords {
			if strings.HasPrefix(rest, keyword) || strings.HasPrefix(keyword, rest) {
				return true
			}
		}
		return false
	}
	for _, keyword := range []string{"warn", "gateway warn", "gateway warning"} {
		if strings.HasPrefix(lower, keyword) || strings.HasPrefix(keyword, lower) {
			return true
		}
	}
	return false
}

// MarkUpstreamProtocolError 标记"上游在流里明确报了失败"。
//
// 为什么需要它:有的失败不以 HTTP 状态码体现,而是 200 流里夹一个终止事件
// (Responses 的 response.failed / response.error,内容里的显式 error 等)。
// 这类事件被 sr.Stop() 记为软错误后,流就不算"正常收尾",exhausted 不置位;
// 而 hasExplicitProtocolError 只认顶层 error 键,response.failed 的 error 是
// 嵌在 response 里的,也认不出来。结果 ClientAnswerUnusable 判不出来,
// 这次尝试反而被记成成功——实测渠道 #5 的 410(它自己作为中转把候选全试失败,
// 用 response.failed 回话)就是这样既没冷却也没换候选。
//
// 与 deliveryBroken 的区别:那是"客户端没收到",这是"上游明说失败"。
func (info *RelayInfo) MarkUpstreamProtocolError() {
	if info == nil || info.answer == nil {
		return
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.protocolError = true
}

// ObserveUpstreamError only records explicit top-level protocol errors in raw JSON.
// Call before lossy response conversions; raw choices must never count as a
// delivered answer. Repeated upstream/client observations are idempotent.
func (info *RelayInfo) ObserveUpstreamError(payload []byte) {
	if info == nil || info.answer == nil || !hasExplicitProtocolError(payload) {
		return
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.protocolError = true
}

func hasExplicitProtocolError(payload []byte) bool {
	// JSON Unicode escapes may hide the literal error key.
	if !bytes.Contains(payload, []byte(`"error"`)) && !bytes.ContainsRune(payload, '\\') {
		return false
	}
	var envelope map[string]any
	if err := basecommon.Unmarshal(payload, &envelope); err != nil {
		return false
	}
	switch value := envelope["error"].(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case map[string]any:
		for _, key := range []string{"message", "type"} {
			if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
				return true
			}
		}
		switch code := value["code"].(type) {
		case string:
			switch strings.ToLower(strings.TrimSpace(code)) {
			case "", "0", "200", "success", "ok":
				return false
			default:
				return true
			}
		case float64:
			return code != 0 && code != 200
		}
	}
	return false
}

// HasExplicitProtocolError 报告上游是否在响应里明确报了错。
// 这是跨格式都可靠的失败信号；空正文判断只对 OpenAI Chat 格式可靠。
func (info *RelayInfo) HasExplicitProtocolError() bool {
	if info == nil || info.answer == nil {
		return false
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.protocolError
}

// ClientAnswerUnusable 判断这次请求是否"HTTP 200 但没有可用回答"。
// 判断不出来时一律返回 false（宁可漏判，也不能把正常回答当失败）。
func (info *RelayInfo) ClientAnswerUnusable() (bool, string) {
	if info == nil || info.answer == nil {
		return false, ""
	}
	observation := info.answer
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.protocolError {
		return true, "200 但上游返回了显式协议错误"
	}
	// 先于 parsed 这道门判断:上游正常收尾、请求也确实到过上游,却没有任何响应
	// 内容时,观察器可能压根没收到可解析载荷(整条流只有一个 [DONE],或最后一批
	// chunk 为空/无 choices),parsed 恒为 false,旧逻辑会把"输入有值、输出为空"
	// 放行成成功。这里要求正文/思考/工具调用全空且没有保留任何 choice,所以一旦
	// 有过真实内容就不会命中。exhausted 由 streamScanner 仅在上游正常收尾
	// (EOF/DONE)且无软错误时标记;client_gone/timeout/scanner_error 各有专门路径。
	if observation.exhausted && !observation.undecidable &&
		!observation.textSeen && !observation.reasoningSeen && !observation.toolsSeen &&
		len(observation.choices) == 0 && len(observation.nativeTexts) == 0 {
		return true, "200 但上游未返回任何响应内容(输入有值、输出为空)"
	}
	if !observation.parsed || observation.undecidable {
		return false, ""
	}
	if observation.textSeen || observation.reasoningSeen || observation.toolsSeen {
		return false, ""
	}
	// Only classify whole delivered choices here, never each delta in observe.
	// A read during a partial label must not permanently mark it as real text.
	sample := ""
	classify := func(text string) bool {
		visible, banners := stripAnswerBanners(text)
		if strings.TrimSpace(visible) != "" {
			return true // real text outside a banner
		}
		if sample == "" && banners > 0 {
			sample = truncateAnswerSample(strings.TrimSpace(text))
		}
		return false
	}
	for _, choice := range observation.choices {
		if classify(string(choice.content)) {
			return false, ""
		}
	}
	for _, choice := range observation.nativeTexts {
		if classify(string(choice.content)) {
			return false, ""
		}
	}
	if sample != "" {
		return true, "响应正文只是上游网关的告警横幅：" + sample
	}
	return true, "200 但响应正文为空（无正文/思考内容/工具调用）"
}

func (observation *answerObservation) observe(payload []byte) {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"choices"`)) {
		return
	}
	var probe struct {
		Choices []struct {
			Index        json.RawMessage   `json:"index"`
			Delta        *answerChoiceBody `json:"delta"`
			Message      *answerChoiceBody `json:"message"`
			FinishReason string            `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := basecommon.Unmarshal(payload, &probe); err != nil || len(probe.Choices) == 0 {
		return
	}

	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.parsed = true
	for i := range probe.Choices {
		choice := &probe.Choices[i]
		switch strings.ToLower(strings.TrimSpace(choice.FinishReason)) {
		case "length", "content_filter":
			// 被截断/被过滤的回答说明不了上游服务能力，不下结论。
			observation.undecidable = true
		}
		body := choice.Delta
		if body == nil {
			body = choice.Message
		}
		if body == nil {
			continue
		}
		if validOpenAIToolCalls(body.ToolCalls) || validOpenAIFunctionCall(body.FunctionCall) {
			observation.toolsSeen = true
		}
		if strings.TrimSpace(body.ReasoningContent) != "" || rawReasoningHasValue(body.Reasoning) {
			observation.reasoningSeen = true
		}
		if observation.textSeen || observation.reasoningSeen || observation.toolsSeen || observation.undecidable {
			observation.choices = nil
			continue
		}
		text := flattenAnswerContent(body.Content)
		if text == "" {
			continue
		}
		index := 0
		missingIndex := len(choice.Index) == 0 || bytes.Equal(bytes.TrimSpace(choice.Index), []byte("null"))
		if (missingIndex && len(probe.Choices) != 1) ||
			(!missingIndex && (basecommon.Unmarshal(choice.Index, &index) != nil || index < 0)) {
			// An ambiguous/malformed index cannot safely join any previous choice.
			observation.undecidable = true
			observation.choices = nil
			continue
		}
		observation.observeContent(index, text)
	}
}

// observeContent runs under observation.mu. Apart from a small label prefix,
// fragments are only appended, so byte-at-a-time delivery does not repeatedly
// regex-parse a growing answer. Ordinary prose drops all retained candidates.
func (observation *answerObservation) observeContent(index int, text string) {
	var choice *answerChoiceObservation
	for i := range observation.choices {
		if observation.choices[i].index == index {
			choice = &observation.choices[i]
			break
		}
	}
	if choice == nil {
		text = strings.TrimLeftFunc(text, unicode.IsSpace)
		if text == "" {
			return
		}
		if len(observation.choices) == answerChoiceLimit {
			observation.undecidable = true
			observation.choices = nil
			return
		}
		if observation.choices == nil {
			observation.choices = make([]answerChoiceObservation, 0, answerChoiceLimit)
		}
		observation.choices = append(observation.choices, answerChoiceObservation{index: index})
		choice = &observation.choices[len(observation.choices)-1]
	}
	if !choice.acceptPrefix(text) {
		observation.textSeen = true
		observation.choices = nil
		return
	}
	if len(text) > answerContentLimit-len(choice.content) {
		// Never classify a truncated prefix as an error-only answer.
		observation.undecidable = true
		observation.choices = nil
		return
	}
	if choice.content == nil {
		choice.content = make([]byte, 0, answerContentLimit)
	}
	choice.content = append(choice.content, text...)
	if choice.prefixDone {
		// 增量横幅前缀可能后面才接正常正文。只有确实识别出一条完整横幅、且
		// 横幅之外剩下的内容不再像下一条横幅的开头时,才提前记录首有效输出。
		// 多个连续横幅或未闭合的多行横幅都保持“未判定”,等流结束再定。
		if visible, banners := stripAnswerBanners(string(choice.content)); banners > 0 &&
			strings.TrimSpace(visible) != "" && !answerTextLooksLikeBannerStart(visible) {
			observation.textSeen = true
			observation.choices = nil
		}
	}
}

func (choice *answerChoiceObservation) acceptPrefix(text string) bool {
	if choice.prefixDone {
		return true
	}
	for _, r := range text {
		if r == '【' && choice.prefix == "" {
			r = '['
		}
		switch r {
		case ' ', '	', '\n', '\r', '\f': // regexp's ASCII \s
			if choice.prefix == "" || choice.prefix == "[" || strings.HasSuffix(choice.prefix, " ") {
				continue
			}
			r = ' '
		}
		choice.prefix += string(r)
		size := utf8.RuneCountInString(choice.prefix)
		possible := false
		for _, prefix := range answerBannerPrefixes {
			runes := []rune(prefix)
			if size <= len(runes) && strings.EqualFold(choice.prefix, string(runes[:size])) {
				possible = true
				if size == len(runes) {
					choice.prefixDone = true
					choice.prefix = ""
					return true
				}
			}
		}
		if !possible {
			return false
		}
	}
	return true
}

// clientAnswerPayloads 从一段写给客户端的数据里取出可解析的 JSON 载荷。
// 流式是若干 "data: {...}" 行；非流式是整包 JSON（可能带换行的缩进格式）。
func clientAnswerPayloads(data []byte) [][]byte {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	if !bytes.Contains(trimmed, []byte("data:")) {
		if trimmed[0] == '{' {
			return [][]byte{trimmed}
		}
		return nil
	}
	var payloads [][]byte
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			body := bytes.TrimSpace(line[len("data:"):])
			// [DONE]、注释行、事件名都不是 JSON
			if len(body) == 0 || body[0] != '{' {
				continue
			}
			payloads = append(payloads, body)
			continue
		}
		if line[0] == '{' {
			payloads = append(payloads, line)
		}
	}
	return payloads
}

// flattenAnswerContent 把 content 统一成字符串：字符串直接返回，
// 分片数组（[{"type":"text","text":"..."}]）拼起来。
func flattenAnswerContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := basecommon.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := basecommon.Unmarshal(raw, &parts); err == nil {
		var builder strings.Builder
		for _, part := range parts {
			builder.WriteString(part.Text)
		}
		return builder.String()
	}
	return ""
}

// stripAnswerBanners 摘掉告警横幅，返回剩下的正文和命中的横幅数。
func stripAnswerBanners(text string) (string, int) {
	visible := text
	hits := 0
	for _, pattern := range answerBannerPatterns {
		matched := pattern.FindAllString(visible, -1)
		if len(matched) == 0 {
			continue
		}
		hits += len(matched)
		visible = pattern.ReplaceAllString(visible, " ")
	}
	return visible, hits
}

// rawHasValue 判断一个可空 JSON 字段是不是"有内容"（空数组/空对象/null 都不算）。
func rawHasValue(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "[]", "{}", `""`:
		return false
	}
	return true
}

// validOpenAIToolCalls accepts only actual tool-call output. A tool definition
// (name/description/parameters with no call) does not belong in delta/message
// tool_calls, and an empty structure is not a call.
func validOpenAIToolCalls(raw json.RawMessage) bool {
	if !rawHasValue(raw) {
		return false
	}
	var calls []map[string]any
	if err := basecommon.Unmarshal(raw, &calls); err != nil {
		return false
	}
	for _, call := range calls {
		if function, ok := call["function"].(map[string]any); ok {
			if strings.TrimSpace(stringValue(function["name"])) != "" ||
				strings.TrimSpace(stringValue(function["arguments"])) != "" {
				return true
			}
		}
		if strings.TrimSpace(stringValue(call["name"])) != "" ||
			strings.TrimSpace(stringValue(call["arguments"])) != "" ||
			strings.TrimSpace(stringValue(call["input"])) != "" {
			return true
		}
	}
	return false
}

// rawReasoningHasValue accepts reasoning text but not an empty reasoning shape
// or metadata object.
func rawReasoningHasValue(raw json.RawMessage) bool {
	if !rawHasValue(raw) {
		return false
	}
	var text string
	if err := basecommon.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text) != ""
	}
	var object map[string]any
	if err := basecommon.Unmarshal(raw, &object); err != nil {
		return false
	}
	for _, key := range []string{"content", "text", "summary", "reasoning"} {
		child, ok := object[key]
		if !ok {
			continue
		}
		switch typed := child.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return true
			}
		case []any:
			for _, item := range typed {
				switch value := item.(type) {
				case string:
					if strings.TrimSpace(value) != "" {
						return true
					}
				case map[string]any:
					if answerTextNonEmpty(stringValue(value["text"])) {
						return true
					}
				}
			}
		}
	}
	return false
}

func validOpenAIFunctionCall(raw json.RawMessage) bool {
	if !rawHasValue(raw) {
		return false
	}
	var call map[string]any
	if err := basecommon.Unmarshal(raw, &call); err != nil {
		return false
	}
	return strings.TrimSpace(stringValue(call["name"])) != "" ||
		strings.TrimSpace(stringValue(call["arguments"])) != ""
}

func truncateAnswerSample(text string) string {
	runes := []rune(text)
	if len(runes) <= answerSampleLimit {
		return text
	}
	return string(runes[:answerSampleLimit]) + "…"
}
