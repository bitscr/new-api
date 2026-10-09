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
	parsed         bool // 见过结构可识别的完成体（带 choices）
	protocolError  bool // 显式上游协议错误，即使已有部分正文也不能算成功
	textSeen       bool // 横幅之外还有正文
	reasoningSeen  bool // 见过思考内容
	toolsSeen      bool // 见过工具调用
	undecidable    bool // 截断/过滤、选择索引不明或观察超限：不下结论
	deliveryBroken bool // 有一批字节没能写完：客户端在交付完成前断开
	choices        []answerChoiceObservation
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
	}
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
	if !observation.parsed || observation.undecidable {
		return false, ""
	}
	if observation.textSeen || observation.reasoningSeen || observation.toolsSeen {
		return false, ""
	}
	// Only classify whole delivered choices here, never each delta in observe.
	// A read during a partial label must not permanently mark it as real text.
	sample := ""
	for _, choice := range observation.choices {
		text := string(choice.content)
		visible, banners := stripAnswerBanners(text)
		if strings.TrimSpace(visible) != "" {
			return false, ""
		}
		if banners > 0 && sample == "" {
			sample = truncateAnswerSample(strings.TrimSpace(text))
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
		if rawHasValue(body.ToolCalls) || rawHasValue(body.FunctionCall) {
			observation.toolsSeen = true
		}
		if strings.TrimSpace(body.ReasoningContent) != "" || rawHasValue(body.Reasoning) {
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

func truncateAnswerSample(text string) string {
	runes := []rune(text)
	if len(runes) <= answerSampleLimit {
		return text
	}
	return string(runes[:answerSampleLimit]) + "…"
}
