package middleware

import (
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

func TestInitialAutoChannelFallsBackOnlyToAuthorizedCandidates(t *testing.T) {
	for _, failure := range []error{nil, errors.New("selection failed")} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
		common.SetContextKey(c, constant.ContextKeyAutoModelCandidates, []string{"first", "denied", "last"})
		common.SetContextKey(c, constant.ContextKeyAutoModelIndex, 0)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "auto")
		common.SetContextKey(c, constant.ContextKeyTokenModelLimitEnabled, true)
		common.SetContextKey(c, constant.ContextKeyTokenModelLimit, map[string]bool{"first": true, "last": true})
		request := &ModelRequest{Model: "first"}
		var attempts []string
		want := &model.Channel{Id: 42}
		channel, group, err := selectInitialAuthorizedAutoChannel(c, request, func() (*model.Channel, string, error) {
			attempts = append(attempts, request.Model)
			if request.Model == "first" {
				common.SetContextKey(c, constant.ContextKeyAutoGroupIndex, 99)
				common.SetContextKey(c, constant.ContextKeyAutoGroupRetryIndex, 12)
				c.Set("channel_affinity_cache_key", "first")
				return nil, "auto", failure
			}
			if common.GetContextKeyInt(c, constant.ContextKeyAutoGroupIndex) != 0 || common.GetContextKeyInt(c, constant.ContextKeyAutoGroupRetryIndex) != 0 {
				t.Fatal("exhausted group state leaked to next candidate")
			}
			if _, exists := c.Get("channel_affinity_cache_key"); exists {
				t.Fatal("candidate-specific affinity state leaked")
			}
			if common.GetContextKeyString(c, constant.ContextKeyUsingGroup) != "auto" {
				t.Fatal("token group changed during initial fallback")
			}
			return want, "actual-group", nil
		})
		if channel != want || group != "actual-group" || err != nil {
			t.Fatalf("selection = %v, %q, %v", channel, group, err)
		}
		if !reflect.DeepEqual(attempts, []string{"first", "last"}) || common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex) != 2 {
			t.Fatalf("attempts = %v, candidate index = %d", attempts, common.GetContextKeyInt(c, constant.ContextKeyAutoModelIndex))
		}
	}
}

func TestInitialAutoChannelStopsOnSuccessAbortAndNonAuto(t *testing.T) {
	for _, mode := range []string{"success", "abort", "affinity-skip", "non-auto", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			if mode != "non-auto" {
				common.SetContextKey(c, constant.ContextKeyAutoModelClientName, "auto")
			}
			common.SetContextKey(c, constant.ContextKeyAutoModelCandidates, []string{"first", "last"})
			request := &ModelRequest{Model: "first"}
			calls := 0
			_, _, _ = selectInitialAuthorizedAutoChannel(c, request, func() (*model.Channel, string, error) {
				calls++
				if mode == "success" {
					return &model.Channel{Id: 1}, "default", nil
				}
				if mode == "abort" {
					c.Abort()
				}
				if mode == "affinity-skip" {
					c.Set("channel_affinity_skip_retry_on_failure", true)
				}
				return nil, "default", nil
			})
			wantCalls := 1
			if mode == "exhausted" {
				wantCalls = 2
			}
			if calls != wantCalls {
				t.Fatalf("selection calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}
