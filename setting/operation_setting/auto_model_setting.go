package operation_setting

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
)

// AutoModelEnabled 控制分组内虚拟模型 auto 是否启用。
// 启用后，OpenAI 兼容 chat 请求中 model=auto 时按被动健康度自动路由，
// 不发送任何主动探测量。
var AutoModelEnabled = true

// AutoModelCandidates 按分组配置 auto 候选模型白名单。
// key 为分组名；value 为该分组允许被 auto 路由的模型列表。
// 未配置的分组默认纳入该分组全部启用模型（仍受计费配置过滤）。
//
// 语义（三种，必须区分）：
//   - 分组键不存在：该分组不限制候选，回退默认（全部启用模型）；
//   - 分组显式空列表：该分组禁用 auto 候选；
//   - 模型权重显式 0：该模型禁用 auto（见 auto_model_weight.go）。
//
// 发布是原子的：解析、清洗与校验全部完成之后，一次写锁内整体替换，
// 并发读只能看到完整旧值或完整新值，永远不会看到"清空后还没写回"的空中间态。
var AutoModelCandidates = types.NewRWMap[string, []string]()

const autoModelDefaultName = "auto"

// IsAutoModelName 判断请求模型名是否是分组内 auto 虚拟模型。
func IsAutoModelName(name string) bool {
	return strings.TrimSpace(name) == autoModelDefaultName
}

// GetAutoModelCandidates 返回分组候选白名单的深拷贝。
// map 与每个模型列表都是独立副本，调用方不能通过返回值修改在线配置。
func GetAutoModelCandidates() map[string][]string {
	source := AutoModelCandidates.ReadAll()
	result := make(map[string][]string, len(source))
	for group, names := range source {
		result[group] = append([]string(nil), names...)
	}
	return result
}

// ValidateAutoModelCandidates 校验一个配置值但不改动在线白名单。
// 面板保存前的预检查与 SetAutoModelCandidates 共用同一份解析规则。
func ValidateAutoModelCandidates(value string) error {
	_, err := parseAutoModelCandidates(value)
	return err
}

// parseAutoModelCandidates 完成解析、清洗与全部校验；出错时返回 nil。
// 非法输入绝不会产生半成品配置：调用方只在成功返回后才发布。
func parseAutoModelCandidates(value string) (map[string][]string, error) {
	result := make(map[string][]string)
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		// 空串表示清空（全部模型回退默认），与 "{}" 同义。
		return result, nil
	}
	var parsed map[string][]string
	if err := common.UnmarshalJsonStr(trimmed, &parsed); err != nil {
		return nil, err
	}
	if parsed == nil {
		return result, nil
	}
	for group, names := range parsed {
		group = strings.TrimSpace(group)
		if group == "" {
			continue
		}
		seen := make(map[string]bool, len(names))
		// 保留显式空列表：它是"该分组禁用 auto 候选"的语义，不能被丢弃。
		cleaned := make([]string, 0, len(names))
		for _, name := range names {
			name = strings.TrimSpace(name)
			if name != "" && !seen[name] {
				seen[name] = true
				cleaned = append(cleaned, name)
			}
		}
		result[group] = cleaned
	}
	return result, nil
}

// SetAutoModelCandidates 覆盖分组候选白名单；空串表示清空（全部模型回退默认）。
// 校验通过之后一次性原子替换，失败时在线配置保持不变。
func SetAutoModelCandidates(value string) error {
	parsed, err := parseAutoModelCandidates(value)
	if err != nil {
		return err
	}
	AutoModelCandidates.Replace(parsed)
	return nil
}

// AutoModelCandidatesToJSONString 导出当前白名单。
func AutoModelCandidatesToJSONString() string {
	data, err := common.Marshal(GetAutoModelCandidates())
	if err != nil {
		return "{}"
	}
	return string(data)
}
