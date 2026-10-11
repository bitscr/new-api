package operation_setting

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 关键词判据的写库前校验:只挡显然无意义或会误伤的条目。
//
// 这里的价值全在边界上:挡太多会让这个功能没用(管理员看到日志里的具体错误串
// 却存不进去),挡太少则一次粘贴就把一片组合封 30 天。所以正向和反向都要钉。
func TestValidateAutoModelPermanentKeyword(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keyword string
		accept  bool
	}{
		{"empty_line_is_skipped", "", true},
		{"whitespace_only_is_skipped", "   ", true},
		// 真实日志里的样本必须放行,否则这个功能就没用了。
		{"real_upstream_phrase", "function calling is not supported", true},
		{"real_upstream_phrase_upper", "Tools / Function Calling Is Not Supported", true},
		{"structured_error_code", "model_not_supported", true},
		{"chinese_upstream_text", "无权访问 超低特价专属 分组", true},
		// 太短:子串匹配下会命中大量无关失败。
		{"too_short", "e", false},
		{"two_chars_is_allowed", "ok", true},
		// 保护项自身:判定里保护项优先,存了也永远不生效。
		{"protected_by_name", "context_length_exceeded", false},
		{"protected_inside_longer_phrase", "error: context_length_exceeded", false},
		// 过于宽泛:命中即 24h 冷却,等于轮着封所有组合。
		{"too_broad_error", "error", false},
		{"too_broad_chinese", "失败", false},
		// 但包含这些词的具体短语要放行(精确匹配才拒绝)。
		{"broad_word_inside_specific_phrase", "upstream error: model deprecated", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := ValidateAutoModelPermanentKeyword(tc.keyword)
			if tc.accept {
				require.Empty(t, message, "具体错误串必须能存进去")
			} else {
				require.NotEmpty(t, message, "会误伤的条目必须在写库前被挡住")
			}
		})
	}
}

// 被拒绝的条目不能进入运行时集合:校验和解析必须是同一套规范化规则。
func TestRejectedKeywordStaysOutOfRuntimeSet(t *testing.T) {
	original := AutoModelPermanentKeywordsToString()
	t.Cleanup(func() { SetAutoModelPermanentKeywords(original) })

	for _, rejected := range []string{"error", "e", "context_length_exceeded"} {
		require.NotEmpty(t, ValidateAutoModelPermanentKeyword(rejected), rejected)
	}
	SetAutoModelPermanentKeywords("function calling is not supported\n\nerror\n\n  ")
	require.Equal(t, []string{"function calling is not supported", "error"}, AutoModelPermanentKeywords(),
		"运行时的规范化规则(去空行、转小写、去重)是既有契约,不能因为校验而改变")
}