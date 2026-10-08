package common

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
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

// answerBannerPatterns 识别网关告警横幅。只有"整段正文只剩横幅"才算无效回答，
// 所以规则宁可窄一点：横幅 + 其它正文 = 正常回答（模型可能只是在引用这句话）。
//
// 注意最后一组：实测样本的横幅是被上游截断的，**没有闭合方括号**
// （"\n\n[Gateway Warning: 当前渠道负" 就到头了），只匹配闭合形式的规则会漏判。
var answerBannerPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)[\[【]\s*(?:gateway\s+)?(?:warning|warn)\b[^\]】]*[\]】]`),
	regexp.MustCompile(`(?is)[\[【]\s*(?:gateway\s+)?(?:notice|alert)\b[^\]】]*[\]】]`),
	regexp.MustCompile(`(?is)[\[【]\s*(?:网关)?(?:告警|警告)[^\]】]*[\]】]`),
	regexp.MustCompile(`(?is)^\s*(?:gateway\s+)?(?:warning|warn)\s*[:：][^\n]*`),
	// 未闭合：从方括号一路到行尾/文本末尾，中间不能再出现闭合方括号
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

type answerObservation struct {
	mu             sync.Mutex
	parsed         bool // 见过结构可识别的完成体（带 choices）
	textSeen       bool // 横幅之外还有正文
	bannerSeen     bool // 见过告警横幅
	reasoningSeen  bool // 见过思考内容
	toolsSeen      bool // 见过工具调用
	undecidable    bool // finish_reason=length/content_filter：不下结论
	deliveryBroken bool // 有一批字节没能写完：客户端在交付完成前断开
	sample         string
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
		observation.observe(payload)
	}
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
	if !observation.parsed || observation.undecidable {
		return false, ""
	}
	if observation.textSeen || observation.reasoningSeen || observation.toolsSeen {
		return false, ""
	}
	if observation.bannerSeen {
		return true, "响应正文只是上游网关的告警横幅：" + observation.sample
	}
	return true, "200 但响应正文为空（无正文/思考内容/工具调用）"
}

func (observation *answerObservation) observe(payload []byte) {
	if len(payload) == 0 || !bytes.Contains(payload, []byte(`"choices"`)) {
		return
	}
	var probe struct {
		Choices []struct {
			Delta        *answerChoiceBody `json:"delta"`
			Message      *answerChoiceBody `json:"message"`
			FinishReason string            `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil || len(probe.Choices) == 0 {
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
		text := flattenAnswerContent(body.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		visible, banners := stripAnswerBanners(text)
		if banners > 0 {
			observation.bannerSeen = true
			if observation.sample == "" {
				observation.sample = truncateAnswerSample(strings.TrimSpace(text))
			}
		}
		if strings.TrimSpace(visible) != "" {
			observation.textSeen = true
			if observation.sample == "" {
				observation.sample = truncateAnswerSample(strings.TrimSpace(visible))
			}
		}
	}
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
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
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
