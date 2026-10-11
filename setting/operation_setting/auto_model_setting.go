package operation_setting

import (
	"encoding/json"
	"strings"

	"github.com/QuantumNous/new-api/types"
)

// AutoModelEnabled 控制分组内虚拟模型 auto 是否启用。
// 启用后，OpenAI 兼容 chat 请求中 model=auto 时按被动健康度自动路由，
// 不发送任何主动探测量。
var AutoModelEnabled = true

// AutoModelCandidates 按分组配置 auto 候选模型白名单。
// key 为分组名；value 为该分组允许被 auto 路由的模型列表。
// 未配置的分组默认纳入该分组全部启用模型（仍受计费配置过滤）。
var AutoModelCandidates = types.NewRWMap[string, []string]()

const autoModelDefaultName = "auto"

// IsAutoModelName 判断请求模型名是否是分组内 auto 虚拟模型。
func IsAutoModelName(name string) bool {
	return strings.TrimSpace(name) == autoModelDefaultName
}

// GetAutoModelCandidates 返回分组候选白名单副本。
func GetAutoModelCandidates() map[string][]string {
	return AutoModelCandidates.ReadAll()
}

// SetAutoModelCandidates 覆盖分组候选白名单；空串表示清空（全部模型回退默认）。
func SetAutoModelCandidates(value string) error {
	result := make(map[string][]string)
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		var parsed map[string][]string
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return err
		}
		for group, names := range parsed {
			group = strings.TrimSpace(group)
			if group == "" {
				continue
			}
			seen := make(map[string]bool)
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
	}
	AutoModelCandidates.Clear()
	AutoModelCandidates.AddAll(result)
	return nil
}

// AutoModelCandidatesToJSONString 导出当前白名单。
func AutoModelCandidatesToJSONString() string {
	data, err := json.Marshal(GetAutoModelCandidates())
	if err != nil {
		return "{}"
	}
	return string(data)
}
