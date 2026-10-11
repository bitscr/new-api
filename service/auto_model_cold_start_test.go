package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 冷启动置信度是否需要额外调参:先量一遍现有行为,再决定要不要加旋钮。
//
// 计划里原本打算"让新组合初始分低于中性,好让坏组合更快被换掉"。但一个组合只要
// 失败一次就会 (a) 掉分,并且 (b) 进冷却直接从候选池消失。所以真正要证明的是:
// 首次失败之后,那个组合是否已经排在"还没试过"的组合后面。如果已经排在后面,
// 再加一个冷启动旋钮就只是多一个没人需要的配置项。
func TestFreshCandidateOutranksJustFailedCandidate(t *testing.T) {
	now := time.Unix(1700000000, 0)

	// 秒拒:200ms 就被上游拒掉(配额/鉴权类)。
	failedFast := updateAutoModelOutcome(autoModelOutcome{}, false, false, 200, now)

	// 还没试过的组合:中性 0.5,零观测。
	fresh := autoModelOutcome{}

	failedScore := effectiveScore(failedFast, now)
	freshScore := effectiveScore(fresh, now)

	require.Less(t, failedScore, freshScore,
		"试过一次就失败的组合必须排在还没试过的组合后面,否则坏组合会被反复命中")

	// 反例方向也要成立:一次成功就该把组合抬到中性以上。
	// 注意签名是 (current, exists, success, latencyMs, now)。
	succeeded := updateAutoModelOutcome(autoModelOutcome{}, false, true, 300, now)
	require.Greater(t, effectiveScore(succeeded, now), freshScore)

	// 卡满 45 秒才失败的组合,必须比秒拒的那个更差——这正是失败也要记耗时的意义。
	failedSlow := updateAutoModelOutcome(autoModelOutcome{}, false, false, 45000, now)
	require.Less(t, effectiveScore(failedSlow, now), failedScore,
		"卡死才失败的组合必须比秒拒的组合更差,否则耗着不说话的反而被当成响应快")
}