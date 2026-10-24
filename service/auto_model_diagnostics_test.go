package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAutoModelPlanDiagnosticIncludesConfigEvidenceAndWarnings(t *testing.T) {
	route := AutoModelRoute{Group: "g", ModelName: "m", ChannelID: 1}
	plan := AutoModelRoutePlan{
		Routes: []AutoModelRoute{route},
		Meta: AutoModelRoutePlanMeta{
			Categories:  map[AutoModelRoute]string{route: "trial"},
			Reasons:     map[AutoModelRoute]string{route: "试用：未观察"},
			Evidence:    map[AutoModelRoute]autoModelEvidenceView{route: {}},
			Diagnostics: AutoModelRouteDiagnostics{Trial: 1},
		},
	}

	text := AutoModelPlanDiagnostic(plan, "g")

	require.Contains(t, text, "cfg{")
	require.Contains(t, text, "cat=trial")
	require.Contains(t, text, "rel=0.00?")
	require.Contains(t, text, "main=0")
}

func TestAutoModelPlanDiagnosticReportsAllCooling(t *testing.T) {
	plan := AutoModelRoutePlan{Meta: AutoModelRoutePlanMeta{
		Categories:  map[AutoModelRoute]string{},
		Reasons:     map[AutoModelRoute]string{},
		Evidence:    map[AutoModelRoute]autoModelEvidenceView{},
		Diagnostics: AutoModelRouteDiagnostics{Cooling: 3},
	}}

	text := AutoModelPlanDiagnostic(plan, "g")

	require.Contains(t, text, "all_candidates_cooling(cooling=3)")
}
