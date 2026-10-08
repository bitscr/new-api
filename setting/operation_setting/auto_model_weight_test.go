package operation_setting

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func preserveAutoModelWeights(t *testing.T) {
	t.Helper()
	before := AutoModelWeightsToJSONString()
	t.Cleanup(func() { require.NoError(t, SetAutoModelWeights(before)) })
}

func TestAutoModelWeightsDefaultAndExplicitZero(t *testing.T) {
	preserveAutoModelWeights(t)
	require.NoError(t, SetAutoModelWeights(`{" default ":{" fast ":3.5,"disabled":0},"vip":{"fast":2}}`))
	require.Equal(t, 3.5, GetAutoModelWeight("default", "fast"))
	require.Equal(t, 0.0, GetAutoModelWeight("default", "disabled"))
	require.Equal(t, 2.0, GetAutoModelWeight("vip", "fast"))
	require.Equal(t, DefaultAutoModelWeight, GetAutoModelWeight("default", "missing"))
	require.Equal(t, DefaultAutoModelWeight, GetAutoModelWeight("other", "fast"))
	require.JSONEq(t, `{"default":{"fast":3.5,"disabled":0},"vip":{"fast":2}}`, AutoModelWeightsToJSONString())
	for _, empty := range []string{"", "  ", "{}"} {
		require.NoError(t, SetAutoModelWeights(empty))
		require.Empty(t, GetAutoModelWeights())
		require.Equal(t, DefaultAutoModelWeight, GetAutoModelWeight("default", "disabled"))
	}
}

func TestAutoModelWeightsRejectInvalidUpdatesWithoutMutation(t *testing.T) {
	preserveAutoModelWeights(t)
	const original = `{"g":{"m":2}}`
	require.NoError(t, SetAutoModelWeights(original))
	for _, value := range []string{
		"not-json", "[]", "null", `{"g":[]}`, `{"g":null}`,
		`{"g":{"m":null}}`, `{"g":{"m":-1}}`, `{"g":{"m":"2"}}`,
		`{"g":{"m":true}}`, `{"g":{"m":NaN}}`, `{"g":{"m":Infinity}}`,
		`{"g":{"m":1e309}}`, `{" ":{"m":1}}`, `{"g":{" ":1}}`,
		`{"g":{"m":1}," g ":{"m":2}}`, `{"g":{"m":1," m ":2}}`,
	} {
		t.Run(value, func(t *testing.T) {
			require.Error(t, ValidateAutoModelWeights(value))
			require.JSONEq(t, original, AutoModelWeightsToJSONString())
			require.Error(t, SetAutoModelWeights(value))
			require.JSONEq(t, original, AutoModelWeightsToJSONString())
		})
	}
	require.NoError(t, ValidateAutoModelWeights(`{"other":{"m":0,"large":1e300}}`))
	require.JSONEq(t, original, AutoModelWeightsToJSONString(), "validating a valid option is also side-effect free")
}

func TestAutoModelWeightsSnapshotOwnership(t *testing.T) {
	preserveAutoModelWeights(t)
	require.NoError(t, SetAutoModelWeights(`{"g":{"a":1,"b":2}}`))
	snapshot := GetAutoModelWeights()
	snapshot["g"]["a"] = 900
	delete(snapshot["g"], "b")
	snapshot["other"] = map[string]float64{"a":0}
	require.Equal(t, 1.0, GetAutoModelWeight("g", "a"))
	require.Equal(t, 2.0, GetAutoModelWeight("g", "b"))
	require.Len(t, GetAutoModelWeights(), 1)
	require.NoError(t, SetAutoModelWeights(`{"g":{"a":3}}`))
	require.Equal(t, 900.0, snapshot["g"]["a"], "publishing another config cannot mutate an existing request snapshot")
}

func TestAutoModelWeightsPublishWholeConfiguration(t *testing.T) {
	preserveAutoModelWeights(t)
	const first = `{"first":{"a":1,"b":2}}`
	const second = `{"second":{"a":3,"b":4}}`
	require.NoError(t, SetAutoModelWeights(first))
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			for _, value := range []string{second, first} {
				if err := SetAutoModelWeights(value); err != nil {
					failures <- err
					return
				}
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			snapshot := GetAutoModelWeights()
			if len(snapshot) != 1 {
				failures <- fmt.Errorf("partial configuration: %v", snapshot)
				return
			}
			for group, weights := range snapshot {
				valid := group == "first" && weights["a"] == 1 && weights["b"] == 2
				valid = valid || group == "second" && weights["a"] == 3 && weights["b"] == 4
				if !valid || len(weights) != 2 {
					failures <- fmt.Errorf("mixed configuration: %v", snapshot)
					return
				}
			}
		}
	}()
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
}
