package operation_setting

import (
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
)

// auto 的选路质量设置。
//
// 与"失败处理"(auto_model_failure_setting.go)刻意分开:那边回答的是"这次请求
// 失败了怎么办",这里回答的是"下一次请求该先打谁"。调参时的心智完全不同,
// 混在一个文件里会让只想调选路的人先翻过一堆冷却阶梯。

// auto 选路默认参数。路由顺序为:渠道 priority 降序(硬边界)→ 可靠性准入
// → 成功延迟接近的候选池 → 池内渠道/模型权重随机 → 少量真实业务探索。
const (
	autoModelDefaultReliabilityThreshold  = 0.95
	autoModelDefaultMinObservations       = 5
	autoModelDefaultLatencyToleranceRatio = 0.20
	autoModelDefaultLatencyToleranceMS    = int64(250)
	autoModelDefaultExploreRatio          = 0.05
	autoModelDefaultExploreMaxRatio       = 0.10
)

// AutoModelMaxMinObservations 是面板/环境变量允许的最低业务观测数上限。
//
// 有效证据总数被封顶在 100,而读取时在新鲜度窗口边缘最多只剩 75%(窗口是
// 衰减窗口的 1/4)。留出累计衰减与取整余量后取 70:任何被接受的阈值都能由
// 真实业务流量达到,不会出现“设置成功但永远进不了主池”的配置。
const AutoModelMaxMinObservations = 70

// AutoModelRoutingSettings 是一次路由规划使用的配置快照。规划器只读这一份
// 值,避免多个 getter 之间被并发更新插入,得到半新半旧的组合。
type AutoModelRoutingSettings struct {
	ReliabilityThreshold  float64
	MinObservations       int
	LatencyToleranceRatio float64
	LatencyToleranceMs    int64
	ExploreRatio          float64
	ExploreMaxRatio       float64
	ScoreDecayMinutes     int
}

var (
	autoModelRoutingSettingsMutex sync.RWMutex

	autoModelReliabilityThreshold  = autoModelDefaultReliabilityThreshold
	autoModelMinObservations       = autoModelDefaultMinObservations
	autoModelLatencyToleranceRatio = autoModelDefaultLatencyToleranceRatio
	autoModelLatencyToleranceMS    = autoModelDefaultLatencyToleranceMS
	autoModelExploreRatio          = autoModelDefaultExploreRatio
	autoModelExploreMaxRatio       = autoModelDefaultExploreMaxRatio
)

// AutoModelRoutingSettingsSnapshot returns all routing parameters under one
// read lock. A plan must not observe a partially applied configuration.
func AutoModelRoutingSettingsSnapshot() AutoModelRoutingSettings {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return AutoModelRoutingSettings{
		ReliabilityThreshold:  autoModelReliabilityThreshold,
		MinObservations:       autoModelMinObservations,
		LatencyToleranceRatio: autoModelLatencyToleranceRatio,
		LatencyToleranceMs:    autoModelLatencyToleranceMS,
		ExploreRatio:          autoModelExploreRatio,
		ExploreMaxRatio:       autoModelExploreMaxRatio,
		ScoreDecayMinutes:     AutoModelScoreDecayMinutes(),
	}
}

// AutoModelReliabilityThreshold 是主流量准入的最低近期成功率。
func AutoModelReliabilityThreshold() float64 {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return autoModelReliabilityThreshold
}

// AutoModelMinObservations 是主流量准入所需的最低有效业务观测数。
func AutoModelMinObservations() int {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return autoModelMinObservations
}

// AutoModelLatencyToleranceRatio 是速度接近候选池的相对容差。
func AutoModelLatencyToleranceRatio() float64 {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return autoModelLatencyToleranceRatio
}

// AutoModelLatencyToleranceMs 是速度接近候选池的绝对容差(毫秒)。
func AutoModelLatencyToleranceMs() int64 {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return autoModelLatencyToleranceMS
}

// AutoModelExploreRatio / AutoModelExploreMaxRatio 是定向探索的基础/最高比例。
func AutoModelExploreRatio() float64 {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return autoModelExploreRatio
}

func AutoModelExploreMaxRatio() float64 {
	autoModelRoutingSettingsMutex.RLock()
	defer autoModelRoutingSettingsMutex.RUnlock()
	return autoModelExploreMaxRatio
}

func SetAutoModelReliabilityThreshold(value float64) {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	if ValidateAutoModelReliabilityThreshold(value) != "" {
		common.SysError("invalid AutoModelReliabilityThreshold; using default")
		value = autoModelDefaultReliabilityThreshold
	}
	autoModelReliabilityThreshold = value
}

func SetAutoModelMinObservations(value int) {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	if ValidateAutoModelMinObservations(value) != "" {
		common.SysError("invalid AutoModelMinObservations; using default")
		value = autoModelDefaultMinObservations
	}
	autoModelMinObservations = value
}

func SetAutoModelLatencyToleranceRatio(value float64) {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	if ValidateAutoModelLatencyToleranceRatio(value) != "" {
		common.SysError("invalid AutoModelLatencyToleranceRatio; using default")
		value = autoModelDefaultLatencyToleranceRatio
	}
	autoModelLatencyToleranceRatio = value
}

func SetAutoModelLatencyToleranceMs(value int64) {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	if ValidateAutoModelLatencyToleranceMs(value) != "" {
		common.SysError("invalid AutoModelLatencyToleranceMs; using default")
		value = autoModelDefaultLatencyToleranceMS
	}
	autoModelLatencyToleranceMS = value
}

func SetAutoModelExploreRatio(value float64) {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	if ValidateAutoModelExploreRatios(value, autoModelExploreMaxRatio) != "" {
		common.SysError("invalid AutoModelExploreRatio; using default")
		value = autoModelDefaultExploreRatio
	}
	autoModelExploreRatio = value
}

func SetAutoModelExploreMaxRatio(value float64) {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	if ValidateAutoModelExploreRatios(autoModelExploreRatio, value) != "" {
		common.SysError("invalid AutoModelExploreMaxRatio; using default")
		value = autoModelDefaultExploreMaxRatio
	}
	autoModelExploreMaxRatio = value
}

// SetAutoModelExploreRatios validates and publishes the pair atomically. The
// two values constrain each other, so one-key validation against whichever
// value happens to be current can reject a legitimate new pair or leave the
// runtime split between an old and a new value.
func SetAutoModelExploreRatios(base, maximum float64) string {
	if message := ValidateAutoModelExploreRatios(base, maximum); message != "" {
		return message
	}
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	autoModelExploreRatio = base
	autoModelExploreMaxRatio = maximum
	return ""
}

func ValidateAutoModelReliabilityThreshold(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		return "可靠性门槛必须在 0～1 之间"
	}
	return ""
}

func ValidateAutoModelMinObservations(value int) string {
	if value < 1 || value > AutoModelMaxMinObservations {
		return "最低业务观测数必须是 1～" + strconv.Itoa(AutoModelMaxMinObservations) + " 的整数"
	}
	return ""
}

func ValidateAutoModelLatencyToleranceRatio(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return "延迟相对容差必须是非负有限数字"
	}
	return ""
}

func ValidateAutoModelLatencyToleranceMs(value int64) string {
	if value < 0 {
		return "延迟绝对容差不能为负数"
	}
	return ""
}

func ValidateAutoModelExploreRatios(base, maximum float64) string {
	if math.IsNaN(base) || math.IsInf(base, 0) || math.IsNaN(maximum) || math.IsInf(maximum, 0) ||
		base < 0 || maximum < 0 || base > maximum || maximum > 0.10 {
		return "探索比例必须满足 0 ≤ 基础比例 ≤ 最高比例 ≤ 0.10"
	}
	return ""
}

// InitAutoModelRoutingSettingsFromEnv seeds the routing parameters from
// environment variables. Database options loaded later override these values.
func InitAutoModelRoutingSettingsFromEnv() {
	autoModelRoutingSettingsMutex.Lock()
	defer autoModelRoutingSettingsMutex.Unlock()
	if raw := strings.TrimSpace(common.GetEnvOrDefaultString("AUTO_MODEL_RELIABILITY_THRESHOLD", "")); raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil && ValidateAutoModelReliabilityThreshold(value) == "" {
			autoModelReliabilityThreshold = value
		} else {
			common.SysError("failed to parse AUTO_MODEL_RELIABILITY_THRESHOLD")
		}
	}
	if raw := strings.TrimSpace(common.GetEnvOrDefaultString("AUTO_MODEL_MIN_OBSERVATIONS", "")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && ValidateAutoModelMinObservations(value) == "" {
			autoModelMinObservations = value
		} else {
			common.SysError("failed to parse AUTO_MODEL_MIN_OBSERVATIONS")
		}
	}
	if raw := strings.TrimSpace(common.GetEnvOrDefaultString("AUTO_MODEL_LATENCY_TOLERANCE_RATIO", "")); raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil && ValidateAutoModelLatencyToleranceRatio(value) == "" {
			autoModelLatencyToleranceRatio = value
		} else {
			common.SysError("failed to parse AUTO_MODEL_LATENCY_TOLERANCE_RATIO")
		}
	}
	if raw := strings.TrimSpace(common.GetEnvOrDefaultString("AUTO_MODEL_LATENCY_TOLERANCE_MS", "")); raw != "" {
		if value, err := strconv.ParseInt(raw, 10, 64); err == nil && ValidateAutoModelLatencyToleranceMs(value) == "" {
			autoModelLatencyToleranceMS = value
		} else {
			common.SysError("failed to parse AUTO_MODEL_LATENCY_TOLERANCE_MS")
		}
	}
	// 两个探索比例相互约束，必须成对读取、成对校验、成对发布。逐项应用时，
	// 先到的一项可能拿旧值做校验，结果被拒绝或与另一项拆开。
	baseRaw := strings.TrimSpace(common.GetEnvOrDefaultString("AUTO_MODEL_EXPLORE_RATIO", ""))
	maxRaw := strings.TrimSpace(common.GetEnvOrDefaultString("AUTO_MODEL_EXPLORE_MAX_RATIO", ""))
	base, baseErr := strconv.ParseFloat(baseRaw, 64)
	maximum, maxErr := strconv.ParseFloat(maxRaw, 64)
	basePresent, maxPresent := baseRaw != "" && baseErr == nil, maxRaw != "" && maxErr == nil
	if baseRaw != "" && baseErr != nil {
		common.SysError("failed to parse AUTO_MODEL_EXPLORE_RATIO")
	}
	if maxRaw != "" && maxErr != nil {
		common.SysError("failed to parse AUTO_MODEL_EXPLORE_MAX_RATIO")
	}
	switch {
	case basePresent && maxPresent:
		if ValidateAutoModelExploreRatios(base, maximum) == "" {
			autoModelExploreRatio, autoModelExploreMaxRatio = base, maximum
		} else {
			common.SysError("invalid AUTO_MODEL_EXPLORE_RATIO / AUTO_MODEL_EXPLORE_MAX_RATIO pair; keeping previous values")
		}
	case basePresent:
		if ValidateAutoModelExploreRatios(base, autoModelExploreMaxRatio) == "" {
			autoModelExploreRatio = base
		} else {
			common.SysError("invalid AUTO_MODEL_EXPLORE_RATIO")
		}
	case maxPresent:
		if ValidateAutoModelExploreRatios(autoModelExploreRatio, maximum) == "" {
			autoModelExploreMaxRatio = maximum
		} else {
			common.SysError("invalid AUTO_MODEL_EXPLORE_MAX_RATIO")
		}
	}
}
