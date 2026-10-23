package service

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/require"
)

// 手动测试不能伪造新路由的业务证据：成功不清冷却、不增加可靠性/延迟样本；
// 非确定性失败也不进入真实业务冷却。只有确定性失败仍按既有策略摘除组合。
func TestRecordAutoModelTestOutcomeDoesNotForgeBusinessEvidence(t *testing.T) {
	setupAutoModelCooldownTestDB(t)
	operation_setting.AutoModelEnabled = true
	t.Cleanup(func() {
		operation_setting.AutoModelEnabled = true
		operation_setting.SetAutoModelPermanentKeywords("")
	})

	channel := &model.Channel{Id: 7, Group: "g1,g2", Models: "m1"}
	RecordAutoModelTestOutcome(channel, "m1", true, 800, nil)
	RecordAutoModelTestOutcome(channel, "m1", false, 500, nil)
	require.Zero(t, autoModelHealth.Len(), "manual tests must not create reliability/latency evidence")
	require.Empty(t, ListAutoModelCooldowns(time.Now()), "manual tests must not create business cooldowns")

	RecordAutoModelTestOutcome(channel, "auto", false, 100, nil)
	require.Zero(t, autoModelHealth.Len())

	operation_setting.AutoModelEnabled = false
	RecordAutoModelTestOutcome(channel, "m1", false, 100, nil)
	require.Zero(t, autoModelHealth.Len())
	operation_setting.AutoModelEnabled = true

	// 确定性失败仍尝试摘除组合；测试库没有 channels 表，摘除失败也不得退回冷却。
	operation_setting.SetAutoModelPermanentKeywords("status code 524")
	permErr := types.NewErrorWithStatusCode(errors.New("bad response status code 524"),
		types.ErrorCode("bad_response_status_code"), http.StatusBadGateway)
	RecordAutoModelTestOutcome(channel, "m1", false, 500, permErr)
	require.Zero(t, autoModelHealth.Len(), "permanent retirement must not create a business sample")
	require.Empty(t, ListAutoModelCooldowns(time.Now()))
}
