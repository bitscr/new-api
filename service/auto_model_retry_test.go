package service_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
)

func setAutoModelRetryTestStatusCodes(t *testing.T, codes string) {
	t.Helper()
	previous := operation_setting.AutomaticRetryStatusCodeRanges
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = previous })
	if err := operation_setting.AutomaticRetryStatusCodesFromString(codes); err != nil {
		t.Fatal(err)
	}
}

func autoModelRetryTestError(status int, code any, param, message string) *types.NewAPIError {
	return types.WithOpenAIError(types.OpenAIError{
		Type: "invalid_request_error", Code: code, Param: param, Message: message,
	}, status)
}

func TestClassifyAutoModelRetryConditionalAccountLimit(t *testing.T) {
	setAutoModelRetryTestStatusCodes(t, "429")
	message := "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct."
	err := autoModelRetryTestError(429, "rate_limit_exceeded", "", message)
	if got := service.ClassifyAutoModelRetry(err); got != service.AutoModelRetryNextChannel {
		t.Fatalf("an incomplete account limit cannot establish model scope: message=%q scope=%v want=%v", message, got, service.AutoModelRetryNextChannel)
	}
}

func TestClassifyAutoModelRetryAccountRateLimitForms(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{"failure-before-for", "Your account rate limit exceeded for model 'gpt-4o'."},
		{"failure-before-on", "Your account rate limit exceeded on model 'gpt-4o'."},
		{"reached-before-on", "The organization's rate limit reached on this model."},
		{"for-before-failure", "Your account rate limit for model 'gpt-4o' exceeded."},
		{"on-before-failure", "Your account rate limit on model 'gpt-4o' exceeded."},
		{"for-before-has-been", "Organization rate limit for model 'gpt-4o' has been exceeded."},
		{"on-before-has-been", "Your project's rate limit on the requested model has been reached."},
		{"possessive-reversed", "Your account's rate limit for model 'gpt-4o' reached."},
		{"organisation-alias", "The organisation's rate limit on selected model has been exceeded."},
		{"unquoted-model", "Project rate limit on model gpt-4o reached."},
		{"reversed-only", "Your account rate limit for model 'gpt-4o' exceeded only."},
		{"on-with-available-siblings", "Your account rate limit exceeded on model 'gpt-4o'; other models remain available."},
		{"reversed-with-available-siblings", "Your account rate limit for model 'gpt-4o' has been exceeded; other models remain available."},
		{"forward-is", "Your account rate limit is exceeded for model 'gpt-4o'."},
		{"forward-was", "Your account rate limit was reached for model 'gpt-4o'."},
		{"forward-has-been", "Your account rate limit has been exceeded for model 'gpt-4o'."},
	} {
		for _, enabled := range []bool{false, true} {
			t.Run(tc.name+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "503", service.AutoModelRetryStop
				if enabled {
					codes, want = "429", service.AutoModelRetryNextModel
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				err := autoModelRetryTestError(429, "rate_limit_exceeded", "", tc.message)
				if got := service.ClassifyAutoModelRetry(err); got != want {
					t.Fatalf("affirmative account/model limits must retain sibling fallback only when enabled: message=%q scope=%v want=%v", tc.message, got, want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryAccountContinuationScope(t *testing.T) {
	for _, tc := range []struct {
		name, continuation string
		want               service.AutoModelRetryScope
	}{
		{"forward-for", "Organization rate limit exceeded for model 'gpt-4o'.", service.AutoModelRetryNextChannel},
		{"forward-on", "Organization rate limit reached on model 'gpt-4o'.", service.AutoModelRetryNextChannel},
		{"reversed-for", "Organization rate limit for model 'gpt-4o' has been exceeded.", service.AutoModelRetryNextChannel},
		{"reversed-on", "Your project's rate limit on the requested model reached.", service.AutoModelRetryNextChannel},
		{"conditional-forward", "Organization rate limit exceeded for model 'gpt-4o'; only if the estimate is correct.", service.AutoModelRetryNextModel},
		{"conditional-reversed", "Organization rate limit on model 'gpt-4o' has been exceeded; only if the estimate is correct.", service.AutoModelRetryNextModel},
		{"unknown-reversed", "Organization rate limit for model 'gpt-4o' reached; further scope details are unavailable.", service.AutoModelRetryNextModel},
		{"model-only-continuation", "Organization rate limit exceeded for model 'gpt-4o' only.", service.AutoModelRetryNextModel},
		{"auxiliary-continuation", "Organization rate limit is exceeded for model 'gpt-4o'.", service.AutoModelRetryNextModel},
		{"quota-continuation", "Organization quota is exhausted only for model 'gpt-4o'.", service.AutoModelRetryNextModel},
	} {
		for _, separator := range []string{",", ";", ":"} {
			t.Run(tc.name+"/"+separator, func(t *testing.T) {
				setAutoModelRetryTestStatusCodes(t, "429")
				message := "Your account's quota is exhausted" + separator + " " + tc.continuation
				err := autoModelRetryTestError(429, "model_rate_limit_exceeded", "model", message)
				if got := service.ClassifyAutoModelRetry(err); got != tc.want {
					t.Fatalf("an account continuation must be complete under its own scope owner before completing the preceding claim: message=%q scope=%v want=%v", message, got, tc.want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryUncertainAccountRateLimitControls(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{"forward-for", "Your account rate limit exceeded for model 'gpt-4o'; only if the estimate is correct."},
		{"forward-on", "Your account rate limit exceeded on model 'gpt-4o'; only if the estimate is correct."},
		{"reversed-for", "Your account rate limit for model 'gpt-4o' exceeded; only if the estimate is correct."},
		{"reversed-on", "Your account's rate limit on the requested model has been reached; only if the estimate is correct."},
		{"forward-auxiliary", "Your account rate limit is exceeded for model 'gpt-4o'; only if the estimate is correct."},
	} {
		for _, evidence := range []struct {
			name, code, param, later string
			want                     service.AutoModelRetryScope
		}{
			{"text-only", "rate_limit_exceeded", "", "", service.AutoModelRetryNextChannel},
			{"model-code", "model_rate_limit_exceeded", "", "", service.AutoModelRetryNextModel},
			{"model-param", "rate_limit_exceeded", "model", "", service.AutoModelRetryNextModel},
			{"later-model", "rate_limit_exceeded", "", " This model is overloaded.", service.AutoModelRetryNextModel},
			{"later-model-rate-limit", "rate_limit_exceeded", "", " Rate limit exceeded on model 'gpt-4o'.", service.AutoModelRetryNextModel},
		} {
			for _, policy := range []struct {
				name, codes string
				status      int
				enabled     bool
			}{
				{"enabled", "429", 429, true},
				{"disabled", "503", 429, false},
				{"no-compatibility-exception", "429", 400, false},
			} {
				t.Run(tc.name+"/"+evidence.name+"/"+policy.name, func(t *testing.T) {
					setAutoModelRetryTestStatusCodes(t, policy.codes)
					want := service.AutoModelRetryStop
					if policy.enabled {
						want = evidence.want
					}
					message := tc.message + evidence.later
					err := autoModelRetryTestError(policy.status, evidence.code, evidence.param, message)
					if got := service.ClassifyAutoModelRetry(err); got != want {
						t.Fatalf("uncertain account text cannot invent model scope or veto independent model evidence: message=%q scope=%v want=%v", message, got, want)
					}
				})
			}
		}
	}
}

func TestClassifyAutoModelRetryAccountMessagePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, codes, code, param, message string
		status                            int
		want                              service.AutoModelRetryScope
	}{
		{"account-wide", "429", "rate_limit_exceeded", "", "Account-wide rate limit exceeded for model 'gpt-4o'.", 429, service.AutoModelRetryNextChannel},
		{"organization-wide", "403", "model_access_denied", "model", "Organization-wide quota exhausted. Access to model 'gpt-4o' is denied.", 403, service.AutoModelRetryNextChannel},
		{"account-quota", "429", "rate_limit_exceeded", "", "Your account's quota is exhausted; rate limit exceeded for model 'gpt-4o'.", 429, service.AutoModelRetryNextChannel},
		{"revoked-key", "403", "access_denied", "", "API key is revoked. Access to model 'gpt-4o' is denied.", 403, service.AutoModelRetryNextChannel},
		{"authentication", "503", "server_error", "", "Authentication failed: model 'gpt-4o' is overloaded.", 503, service.AutoModelRetryNextChannel},
		{"invalid-key-no-compatibility", "429", "unknown_error", "", "Invalid API key. Model 'gpt-4o' not found.", 404, service.AutoModelRetryStop},
		{"disabled-account-status", "500-599", "rate_limit_exceeded", "", "Account-wide rate limit exceeded for model 'gpt-4o'.", 429, service.AutoModelRetryStop},
		{"account-model-limit", "429", "rate_limit_exceeded", "", "Your account: rate limit exceeded for model 'gpt-4o'.", 429, service.AutoModelRetryNextModel},
		{"key-model-access", "403", "access_denied", "", "Your API key: access to model 'gpt-4o' is denied.", 403, service.AutoModelRetryNextModel},
		{"organization-model-limit", "429", "rate_limit_exceeded", "", "Organization rate limit for model 'gpt-4o' has been exceeded.", 429, service.AutoModelRetryNextModel},
		{"negated-account-limit", "503", "server_error", "", "Account-wide rate limit is not exceeded. Model 'gpt-4o' is overloaded.", 503, service.AutoModelRetryNextModel},
		{"model-auth-control", "403", "model_access_denied", "model", "This account cannot access the requested model.", 403, service.AutoModelRetryNextModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setAutoModelRetryTestStatusCodes(t, tc.codes)
			err := autoModelRetryTestError(tc.status, tc.code, tc.param, tc.message)
			if got := service.ClassifyAutoModelRetry(err); got != tc.want {
				t.Fatalf("account message precedence must not confuse whole-account and per-model failures: got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestClassifyAutoModelRetryAccountClauseBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		want          service.AutoModelRetryScope
	}{
		{"only-model", "Your account rate limit is exceeded only for model 'gpt-4o'; other models remain available.", service.AutoModelRetryNextModel},
		{"for-model", "Your account rate limit is exceeded for model 'gpt-4o'.", service.AutoModelRetryNextModel},
		{"quota-for-model", "Your account's quota is exhausted for the requested model.", service.AutoModelRetryNextModel},
		{"organization-model-only", "Organization rate limit has been reached for model 'gpt-4o' only.", service.AutoModelRetryNextModel},
		{"wide-but-model-only", "Account-wide rate limit exceeded only for model 'gpt-4o'; other models remain available.", service.AutoModelRetryNextModel},
		{"conditional-exhaustion", "Your account rate limit is exceeded if this model is used; other models remain available.", service.AutoModelRetryNextModel},
		{"negated-account-subject", "No account rate limit is exceeded. Rate limit exceeded for model 'gpt-4o'.", service.AutoModelRetryNextModel},
		{"negated-account-predicate", "Your account rate limit is not exceeded. Rate limit exceeded for model 'gpt-4o'.", service.AutoModelRetryNextModel},
		{"wide-with-model-context", "Account-wide rate limit exceeded for model 'gpt-4o'.", service.AutoModelRetryNextChannel},
		{"whole-account", "Your account rate limit is exceeded.", service.AutoModelRetryNextChannel},
		{"whole-account-clause", "Your account's quota is exhausted; rate limit exceeded for model 'gpt-4o'.", service.AutoModelRetryNextChannel},
		{"all-models", "Your account quota is exhausted across all models.", service.AutoModelRetryNextChannel},
		{"organization-all-models", "Organization rate limit has been reached for all models.", service.AutoModelRetryNextChannel},
		{"revoked-key", "API key is revoked. Rate limit exceeded for model 'gpt-4o'.", service.AutoModelRetryNextChannel},
	} {
		for _, enabled := range []bool{false, true} {
			t.Run(tc.name+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "503", service.AutoModelRetryStop
				if enabled {
					codes, want = "429", tc.want
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				err := autoModelRetryTestError(429, "model_rate_limit_exceeded", "model", tc.message)
				if got := service.ClassifyAutoModelRetry(err); got != want {
					t.Fatalf("only affirmative whole-account clauses may override structured model scope: message=%q scope=%v want=%v", tc.message, got, want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryAccountPunctuationScope(t *testing.T) {
	model, channel := service.AutoModelRetryNextModel, service.AutoModelRetryNextChannel
	for _, tc := range []struct {
		name, message string
		want          service.AutoModelRetryScope
		wantTextOnly  service.AutoModelRetryScope
	}{
		{"only-for-model", "Your account rate limit is exceeded{separator} only for model 'gpt-4o'; other models remain available.", model, model},
		{"for-model-only", "Your account rate limit is exceeded{separator} for model 'gpt-4o' only.", model, model},
		{"for-model", "Your account rate limit is exceeded{separator} for model 'gpt-4o'.", model, model},
		{"organization-model-only", "Organization quota is exhausted{separator} only for the requested model; other models remain available.", model, model},
		{"wide-but-only-model", "Account-wide rate limit exceeded{separator} only for model 'gpt-4o'; other models remain available.", model, model},
		{"wide-for-model-only", "Account-wide rate limit exceeded{separator} for model 'gpt-4o' only.", model, model},
		{"wide-with-model-context", "Account-wide rate limit exceeded{separator} for model 'gpt-4o'.", channel, channel},
		{"wide-with-available-siblings", "Account-wide rate limit exceeded{separator} for model 'gpt-4o'; other models remain available.", model, model},
		{"conflicting-all-models-scope", "Your account rate limit is exceeded for all models{separator} only for model 'gpt-4o'; other models remain available.", model, channel},
		{"negated-narrowing", "Your account rate limit is exceeded{separator} not only for model 'gpt-4o'.", model, channel},
		{"for-all-models", "Your account rate limit is exceeded{separator} for all models.", channel, channel},
		{"across-all-models", "Your account quota is exhausted{separator} across all models.", channel, channel},
		{"incidental-model", "Your account rate limit is exceeded{separator} requested model: gpt-4o.", channel, channel},
		{"independent-model-limit", "Your account's quota is exhausted{separator} rate limit exceeded for model 'gpt-4o'.", channel, channel},
		{"all-models-with-context", "Organization rate limit is exceeded for all models{separator} requested model: gpt-4o.", channel, channel},
		{"conditional-scope", "Your account rate limit is exceeded{separator} if this model is used; other models remain available.", model, channel},
		{"qualified-model-scope", "Your account rate limit is exceeded{separator} only for model 'gpt-4o' if the estimate is correct.", model, channel},
		{"unknown-scope", "Your account rate limit is exceeded{separator} further scope details are unavailable.", model, channel},
		{"contradictory-global-scope", "Your account rate limit is exceeded{separator} not necessarily for all models.", model, channel},
		{"revoked-key", "API key is revoked{separator} rate limit exceeded for model 'gpt-4o'.", channel, channel},
		{"authentication", "Authentication failed{separator} requested model: gpt-4o.", channel, channel},
		{"model-limit-then-auth", "Your account rate limit is exceeded{separator} only for model 'gpt-4o'; API key is revoked.", channel, channel},
	} {
		for _, separator := range []string{":", ",", ";"} {
			for _, evidence := range []struct{ name, code, param string }{
				{"model-code", "model_rate_limit_exceeded", "model"},
				{"model-param", "rate_limit_exceeded", "model"},
				{"text-only", "rate_limit_exceeded", ""},
			} {
				for _, policy := range []struct {
					name, codes string
					status      int
					enabled     bool
				}{
					{"enabled", "429", 429, true},
					{"disabled", "503", 429, false},
					{"no-compatibility-exception", "429", 400, false},
				} {
					t.Run(tc.name+"/"+separator+"/"+evidence.name+"/"+policy.name, func(t *testing.T) {
						setAutoModelRetryTestStatusCodes(t, policy.codes)
						want := service.AutoModelRetryStop
						if policy.enabled {
							want = tc.want
							if evidence.name == "text-only" {
								want = tc.wantTextOnly
							}
						}
						message := strings.ReplaceAll(tc.message, "{separator}", separator)
						err := autoModelRetryTestError(policy.status, evidence.code, evidence.param, message)
						if got := service.ClassifyAutoModelRetry(err); got != want {
							t.Fatalf("explanatory punctuation does not complete account scope: message=%q scope=%v want=%v", message, got, want)
						}
					})
				}
			}
		}
	}
}

func TestClassifyAutoModelRetryCompatibilityCodes(t *testing.T) {
	for _, code := range []string{"model_not_found", "context_length_exceeded", "unsupported_model_feature"} {
		for _, status := range []int{400, 404, 405, 422, 501} {
			for _, enabled := range []bool{false, true} {
				t.Run(code+"/"+strconv.Itoa(status)+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
					codes := ""
					if enabled {
						codes = strconv.Itoa(status)
					}
					setAutoModelRetryTestStatusCodes(t, codes)
					err := autoModelRetryTestError(status, code, "", "")
					if got := service.ClassifyAutoModelRetry(err); got != service.AutoModelRetryNextModel {
						t.Fatalf("explicit model incompatibility must try next model; got %v", got)
					}
				})
			}
		}
	}
}

func TestClassifyAutoModelRetryModelScopeStatusPolicy(t *testing.T) {
	cases := []struct {
		status int
		code   string
	}{
		{403, "model_access_denied"},
		{429, "model_rate_limit_exceeded"},
		{503, "model_overloaded"},
		{500, "model_overloaded"},
		{403, "model_not_found"},
		{408, "model_not_found"},
		{429, "model_not_found"},
		{503, "unsupported_model_feature"},
	}
	for _, tc := range cases {
		for _, enabled := range []bool{false, true} {
			t.Run(tc.code+"/"+strconv.Itoa(tc.status)+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "", service.AutoModelRetryStop
				if enabled {
					codes, want = strconv.Itoa(tc.status), service.AutoModelRetryNextModel
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				err := autoModelRetryTestError(tc.status, tc.code, "", "")
				if got := service.ClassifyAutoModelRetry(err); got != want {
					t.Fatalf("model-specific errors outside compatibility statuses still obey configuration: scope=%v, want=%v", got, want)
				}
			})
		}
	}
	for _, code := range []string{"model_access_denied", "model_rate_limit_exceeded", "model_overloaded"} {
		for _, status := range []int{400, 404, 405, 422, 501} {
			t.Run(code+"/not-compatibility/"+strconv.Itoa(status), func(t *testing.T) {
				setAutoModelRetryTestStatusCodes(t, "")
				if got := service.ClassifyAutoModelRetry(autoModelRetryTestError(status, code, "", "")); got != service.AutoModelRetryStop {
					t.Fatalf("model scope alone is not a compatibility exception; got %v", got)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryStructuredEvidence(t *testing.T) {
	cases := []struct {
		name string
		err  *types.NewAPIError
		want service.AutoModelRetryScope
	}{
		{"unsupported-model-code", autoModelRetryTestError(400, "unsupported_model", "", ""), service.AutoModelRetryNextModel},
		{"model-not-supported-code", autoModelRetryTestError(400, "model_not_supported", "", ""), service.AutoModelRetryNextModel},
		{"typed-code", autoModelRetryTestError(404, types.ErrorCodeModelNotFound, "", ""), service.AutoModelRetryNextModel},
		{"type-without-code", types.WithOpenAIError(types.OpenAIError{Type: "model_not_found"}, 404), service.AutoModelRetryNextModel},
		{"claude-type", types.WithClaudeError(types.ClaudeError{Type: "model_not_found"}, 404), service.AutoModelRetryNextModel},
		{"model-param-unsupported-value", autoModelRetryTestError(400, "unsupported_value", "model", ""), service.AutoModelRetryNextModel},
		{"model-param-invalid-value", autoModelRetryTestError(400, "invalid_value", "model", ""), service.AutoModelRetryNextModel},
		{"model-param-not-found", autoModelRetryTestError(404, "not_found", "model", ""), service.AutoModelRetryNextModel},
		{"model-param-unsupported-parameter", autoModelRetryTestError(400, "unsupported_parameter", "model", ""), service.AutoModelRetryNextModel},
		{"generic-type", autoModelRetryTestError(400, nil, "", "Invalid request"), service.AutoModelRetryStop},
		{"model-param-alone", autoModelRetryTestError(400, "invalid_request_error", "model", "Invalid request"), service.AutoModelRetryStop},
		{"missing-model-param", autoModelRetryTestError(400, "missing_required_parameter", "model", ""), service.AutoModelRetryStop},
		{"unsupported-other-param", autoModelRetryTestError(400, "unsupported_parameter", "temperature", ""), service.AutoModelRetryStop},
		{"invalid-other-param", autoModelRetryTestError(400, "invalid_value", "messages", ""), service.AutoModelRetryStop},
		{"numeric-code", autoModelRetryTestError(404, 404, "", "Endpoint not found"), service.AutoModelRetryStop},
		{"code-substring", autoModelRetryTestError(404, "trace_model_not_found_detail", "", ""), service.AutoModelRetryStop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setAutoModelRetryTestStatusCodes(t, "")
			if got := service.ClassifyAutoModelRetry(tc.err); got != tc.want {
				t.Fatalf("structured evidence: scope=%v, want=%v", got, tc.want)
			}
		})
	}
}

func TestClassifyAutoModelRetryModelMessages(t *testing.T) {
	stop, nextModel, nextChannel := service.AutoModelRetryStop, service.AutoModelRetryNextModel, service.AutoModelRetryNextChannel
	cases := []struct {
		name         string
		status       int
		message      string
		wantExcluded service.AutoModelRetryScope
		wantEnabled  service.AutoModelRetryScope
	}{
		{"quoted-not-found", 404, "The model `gpt-4o` does not exist or you do not have access to it.", nextModel, nextModel},
		{"requested-not-found", 404, "Requested model not found.", nextModel, nextModel},
		{"unsupported-endpoint", 405, "This model does not support the requested endpoint.", nextModel, nextModel},
		{"unsupported-feature", 422, "This model does not support the requested feature.", nextModel, nextModel},
		{"unsupported-parameter", 400, "Unsupported parameter: 'temperature' is not supported with this model.", nextModel, nextModel},
		{"unsupported-value", 400, "Unsupported value: 'reasoning_effort' does not support 'max' with this model.", nextModel, nextModel},
		{"unsupported-decimal-value", 400, "Unsupported value: 'temperature' does not support 0.7 with this model. Only the default (1) value is supported.", nextModel, nextModel},
		{"unrelated-sentences", 400, "This endpoint does not support streaming. A validation error occurred with this model.", stop, nextChannel},
		{"unimplemented-model-feature", 501, "The requested operation is not implemented for this model.", nextModel, nextModel},
		{"named-model-not-supported", 400, "The model gpt-4o is not supported on this endpoint.", nextModel, nextModel},
		{"gemini-model-resource", 404, "models/gemini-2.5-pro is not found for API version v1beta.", nextModel, nextModel},
		{"model-context-length", 400, "This model's maximum context length is 8192 tokens; you requested 9000 tokens.", nextModel, nextModel},
		{"model-access", 403, "Access to this model is denied.", stop, nextModel},
		{"named-model-access", 403, "You do not have access to model 'gpt-4o'.", stop, nextModel},
		{"model-permission", 403, "Permission denied for this model.", stop, nextModel},
		{"model-rate-limit", 429, "Rate limit exceeded for this model.", stop, nextModel},
		{"named-model-rate-limit", 429, "Rate limit for model 'gpt-4o' exceeded.", stop, nextModel},
		{"model-rate-limited", 429, "This model is rate limited.", stop, nextModel},
		{"model-overload", 503, "This model is overloaded.", stop, nextModel},
		{"named-model-overload", 503, "Model gpt-4o is currently overloaded.", stop, nextModel},
		{"model-capacity", 503, "This model is at capacity.", stop, nextModel},
		{"generic-model-mention", 400, "Invalid request field independent of model.", stop, nextChannel},
		{"invalid-model-field", 400, "Invalid model parameter format.", stop, nextChannel},
		{"unsupported-parameter-only", 400, "Unsupported parameter: 'temperature'.", stop, nextChannel},
		{"unsupported-model-parameter", 400, "Unsupported model parameter: temperature.", stop, nextChannel},
		{"model-metadata-endpoint", 404, "Model metadata endpoint not found.", stop, nextChannel},
		{"missing-model-field", 400, "The model field is required.", stop, nextChannel},
		{"invalid-request-type", 400, "Request has invalid_request_error; model=gpt-4o.", stop, nextChannel},
		{"model-request-json", 422, "Invalid JSON for model request.", stop, nextChannel},
		{"model-gateway-endpoint", 501, "Model gateway endpoint is not implemented.", stop, nextChannel},
		{"account-rate-limit", 429, "Account rate limit exceeded; requested model: gpt-4o.", stop, nextChannel},
		{"model-routing-rate-limit", 429, "Too many requests for model routing.", stop, nextChannel},
		{"model-gateway-overload", 503, "Model gateway is overloaded.", stop, nextChannel},
		{"unavailable-model-mention", 503, "Upstream unavailable while serving model gpt-4o.", stop, nextChannel},
		{"overload-model-suggestion", 503, "Server overloaded; retry with another model.", stop, nextChannel},
	}
	for _, tc := range cases {
		for _, enabled := range []bool{false, true} {
			t.Run(tc.name+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "", tc.wantExcluded
				if enabled {
					codes, want = strconv.Itoa(tc.status), tc.wantEnabled
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				if got := service.ClassifyAutoModelRetry(autoModelRetryTestError(tc.status, nil, "", tc.message)); got != want {
					t.Fatalf("message %q: scope=%v, want=%v", tc.message, got, want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryContextOverflowEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, code, message string
		incompatible        bool
	}{
		{"capacity-only", "invalid_request", "This model's maximum context length is 8192 tokens.", false},
		{"capacity-and-malformed-json", "invalid_request", "This model's maximum context length is 8192 tokens; the request JSON is malformed.", false},
		{"malformed-json-then-capacity", "invalid_request", "The request JSON is malformed. This model's maximum context length is 8192 tokens.", false},
		{"within-limit", "invalid_request", "This model's maximum context length is 8192 tokens; you requested 8000 tokens.", false},
		{"at-limit", "invalid_request", "This model's maximum context length is 8192 tokens; you requested 8192 tokens.", false},
		{"within-limit-and-malformed-json", "invalid_request", "This model's maximum context length is 8192 tokens; you requested 8000 tokens. The request JSON is malformed.", false},
		{"requested-count-only", "invalid_request", "You requested 9000 tokens; the request JSON is malformed.", false},
		{"unrelated-count", "invalid_request", "This model's maximum context length is 8192 tokens; another request used 9000 tokens. The request JSON is malformed.", false},
		{"conditional-count", "invalid_request", "This model's maximum context length is 8192 tokens; if you requested 9000 tokens, it would be rejected.", false},
		{"hypothetical-count", "invalid_request", "This model's maximum context length is 8192 tokens; you would have requested 9000 tokens.", false},
		{"negated-count", "invalid_request", "This model's maximum context length is 8192 tokens; you did not request 9000 tokens.", false},
		{"qualified-count", "invalid_request", "This model's maximum context length is 8192 tokens; you requested 9000 tokens if the estimate is correct.", false},
		{"unparseable-count", "invalid_request", "This model's maximum context length is 8192 tokens; you requested 900000000000000000000 tokens.", false},
		{"unparseable-capacity", "invalid_request", "This model's maximum context length is 900000000000000000000 tokens; you requested 9000 tokens.", false},
		{"capacity-denies-overflow", "invalid_request", "This model's maximum context length is 8192 tokens; this limit was not exceeded.", false},
		{"negated-overflow", "invalid_request", "This model's maximum context length has not been exceeded.", false},
		{"conditional-overflow", "invalid_request", "This model's maximum context length is exceeded only if the request is too long.", false},
		{"conditional-overflow-colon", "invalid_request", "This model's maximum context length is exceeded: only if the request is too long.", false},
		{"conditional-overflow-semicolon", "invalid_request", "This model's maximum context length is exceeded; only if the request is too long.", false},
		{"hypothetical-overflow-continuation", "invalid_request", "This model's maximum context length is exceeded; that is only a hypothetical example.", false},
		{"conditional-count-colon", "invalid_request", "This model's maximum context length is 8192 tokens; you requested 9000 tokens: if the estimate is correct.", false},
		{"conditional-count-semicolon", "invalid_request", "This model's maximum context length is 8192 tokens; you requested 9000 tokens; if the estimate is correct.", false},
		{"numeric-overflow", "invalid_request", "This model's maximum context length is 8192 tokens; you requested 9000 tokens.", true},
		{"numeric-overflow-after-period", "invalid_request", "This model's maximum context length is 8192 tokens. However, you requested 9000 tokens.", true},
		{"named-model-numeric-overflow", "invalid_request", "The model 'gpt-4o' maximum context length is 8192 tokens, you requested 9000 tokens.", true},
		{"affirmative-overflow", "invalid_request", "This model's maximum context length has been exceeded.", true},
		{"affirmative-overflow-with-capacity", "invalid_request", "This model's maximum context length of 8192 tokens is exceeded.", true},
		{"structured-context-overflow", "context_length_exceeded", "This model's maximum context length is 8192 tokens.", true},
	} {
		for _, enabled := range []bool{false, true} {
			t.Run(tc.name+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "429", service.AutoModelRetryStop
				if enabled {
					codes, want = "400", service.AutoModelRetryNextChannel
				}
				if tc.incompatible {
					want = service.AutoModelRetryNextModel
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				err := autoModelRetryTestError(400, tc.code, "", tc.message)
				if got := service.ClassifyAutoModelRetry(err); got != want {
					t.Fatalf("context capacity is not rejection evidence without affirmative overflow: message=%q scope=%v want=%v", tc.message, got, want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryModelClauseBoundary(t *testing.T) {
	stop, nextModel, nextChannel := service.AutoModelRetryStop, service.AutoModelRetryNextModel, service.AutoModelRetryNextChannel
	for _, tc := range []struct {
		name, codes, code, message string
		status                     int
		want                       service.AutoModelRetryScope
	}{
		{"no-model-unsupported", "429", "invalid_request", "No model is unsupported. The request JSON is malformed.", 400, stop},
		{"no-model-unsupported-enabled", "400", "invalid_request", "No model is unsupported. The request JSON is malformed.", 400, nextChannel},
		{"neither-model-unsupported", "429", "invalid_request", "Neither model is unsupported. The request JSON is malformed.", 400, stop},
		{"conditional-model-unsupported", "429", "invalid_request", "If this model is unsupported, select another model. The request JSON is malformed.", 400, stop},
		{"denied-model-assertion", "429", "invalid_request", "It is not true that model 'gpt-4o' is unsupported. The request JSON is malformed.", 400, stop},
		{"negated-model-predicate", "429", "invalid_request", "This model is not unsupported. The request JSON is malformed.", 400, stop},
		{"no-unsupported-parameter", "429", "invalid_request", "No parameter is unsupported with this model. The request JSON is malformed.", 400, stop},
		{"negated-unsupported-operation", "429", "invalid_request", "The requested operation is not unsupported for this model. The request JSON is malformed.", 400, stop},
		{"denied-resource-assertion", "429", "invalid_request", "It is not true that models/gemini-2.5-pro is not found. The request JSON is malformed.", 400, stop},
		{"denied-context-assertion", "429", "invalid_request", "It is not true that this model's maximum context length is 8192 tokens. The request JSON is malformed.", 400, stop},
		{"no-model-overloaded", "503", "invalid_request", "No model is overloaded; upstream maintenance is in progress.", 503, nextChannel},
		{"no-model-overloaded-disabled", "429", "invalid_request", "No model is overloaded; upstream maintenance is in progress.", 503, stop},
		{"no-named-model-overloaded", "503", "invalid_request", "No model 'gpt-4o' is overloaded; upstream maintenance is in progress.", 503, nextChannel},
		{"conditional-model-overload", "503", "invalid_request", "If this model is overloaded, try again later; upstream maintenance is in progress.", 503, nextChannel},
		{"negated-model-overload", "503", "invalid_request", "This model is not overloaded; upstream maintenance is in progress.", 503, nextChannel},
		{"no-access-denied", "403", "access_denied", "No access to this model is denied. The credential has another restriction.", 403, nextChannel},
		{"no-permission-denied", "403", "permission_denied", "No permission is denied for this model. The credential has another restriction.", 403, nextChannel},
		{"no-rate-limit", "429", "rate_limit_exceeded", "No rate limit exceeded for this model. The account has another restriction.", 429, nextChannel},
		{"affirmative-model-subject", "429", "invalid_request", "This model is unsupported on this endpoint.", 400, nextModel},
		{"affirmative-after-colon", "429", "invalid_request", "Invalid request: the requested model is unsupported on this endpoint.", 400, nextModel},
		{"affirmative-after-period", "429", "invalid_request", "The request was rejected. The model 'gpt-4o' is unsupported on this endpoint.", 400, nextModel},
		{"affirmative-after-semicolon", "503", "invalid_request", "Upstream rejected the request; this model is overloaded.", 503, nextModel},
		{"negative-then-affirmative-overload", "503", "invalid_request", "No model is unsupported. This model is overloaded.", 503, nextModel},
		{"negative-then-affirmative-compatibility", "429", "invalid_request", "No model is overloaded; this model does not support the requested endpoint.", 400, nextModel},
		{"structured-compatibility-precedence", "429", "model_not_found", "No model is unsupported. The request JSON is malformed.", 400, nextModel},
		{"structured-overload-precedence", "503", "model_overloaded", "No model is overloaded; upstream maintenance is in progress.", 503, nextModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setAutoModelRetryTestStatusCodes(t, tc.codes)
			err := autoModelRetryTestError(tc.status, tc.code, "", tc.message)
			if got := service.ClassifyAutoModelRetry(err); got != tc.want {
				t.Fatalf("message evidence must start with an affirmative clause subject: message=%q scope=%v want=%v", tc.message, got, tc.want)
			}
		})
	}
}

func TestClassifyAutoModelRetryMessageEvidenceSource(t *testing.T) {
	for _, tc := range []struct {
		name, message, display string
		want                   service.AutoModelRetryScope
	}{
		{"negative-source-positive-display", "No model is unsupported. The request JSON is malformed.", "This model is unsupported.", service.AutoModelRetryStop},
		{"positive-source-negative-display", "This model is unsupported.", "No model is unsupported. The request JSON is malformed.", service.AutoModelRetryNextModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setAutoModelRetryTestStatusCodes(t, "429")
			err := autoModelRetryTestError(400, "invalid_request", "", tc.message)
			err.SetMessage(tc.display)
			if got := service.ClassifyAutoModelRetry(err); got != tc.want {
				t.Fatalf("scope must follow the source OpenAI error, not a rewritten display message: got=%v want=%v", got, tc.want)
			}
		})
	}
	for _, code := range []types.ErrorCode{types.ErrorCodeInvalidRequest, types.ErrorCodeConvertRequestFailed, types.ErrorCodeBadResponseBody} {
		t.Run("local-or-unparsed/"+string(code), func(t *testing.T) {
			setAutoModelRetryTestStatusCodes(t, "100-599")
			err := types.NewErrorWithStatusCode(errors.New("This model is unsupported."), code, 400)
			if got := service.ClassifyAutoModelRetry(err); got != service.AutoModelRetryStop {
				t.Fatalf("local or unparsed failures must not gain an upstream compatibility exception: got=%v", got)
			}
		})
	}
}

func TestClassifyAutoModelRetryAccountEvidenceWins(t *testing.T) {
	cases := []struct {
		name string
		err  *types.NewAPIError
	}{
		{"api-key-with-compatibility-text", autoModelRetryTestError(400, "invalid_api_key", "model", "This model does not support the requested feature")},
		{"account-quota-with-missing-model-text", autoModelRetryTestError(404, "insufficient_quota", "", "Requested model not found")},
		{"account-quota-with-model-rate-text", autoModelRetryTestError(429, "insufficient_quota", "model", "Rate limit exceeded for this model")},
		{"account-disabled-with-model-overload", autoModelRetryTestError(503, "account_deactivated", "", "This model is overloaded")},
		{"auth-type-with-model-code", types.WithOpenAIError(types.OpenAIError{Type: "authentication_error", Code: "model_not_found"}, 404)},
		{"auth-code-with-model-type", types.WithOpenAIError(types.OpenAIError{Type: "model_not_found", Code: "invalid_api_key"}, 404)},
	}
	for _, tc := range cases {
		for _, enabled := range []bool{false, true} {
			t.Run(tc.name+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "", service.AutoModelRetryStop
				if enabled {
					codes, want = strconv.Itoa(tc.err.StatusCode), service.AutoModelRetryNextChannel
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				if got := service.ClassifyAutoModelRetry(tc.err); got != want {
					t.Fatalf("explicit account/auth evidence must win over incidental model evidence: scope=%v, want=%v", got, want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryModelParamScope(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{403, "access_denied"},
		{403, "permission_denied"},
		{429, "rate_limit_exceeded"},
		{503, "overloaded_error"},
	} {
		for _, enabled := range []bool{false, true} {
			t.Run(tc.code+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "", service.AutoModelRetryStop
				if enabled {
					codes, want = strconv.Itoa(tc.status), service.AutoModelRetryNextModel
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				if got := service.ClassifyAutoModelRetry(autoModelRetryTestError(tc.status, tc.code, "model", "")); got != want {
					t.Fatalf("explicit model param scope: scope=%v, want=%v", got, want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryAlwaysSkippedStatuses(t *testing.T) {
	found := false
	for status := 100; status <= 599; status++ {
		if !operation_setting.IsAlwaysSkipRetryStatusCode(status) {
			continue
		}
		found = true
		for _, tc := range []struct {
			name string
			err  *types.NewAPIError
		}{
			{"generic", autoModelRetryTestError(status, "server_error", "", "Upstream unavailable")},
			{"compatibility-code", autoModelRetryTestError(status, "model_not_found", "", "")},
			{"compatibility-message", autoModelRetryTestError(status, nil, "", "This model does not support the requested feature")},
			{"channel-configuration", types.NewErrorWithStatusCode(errors.New("Missing channel key"), types.ErrorCodeChannelNoAvailableKey, status)},
		} {
			t.Run(strconv.Itoa(status)+"/"+tc.name, func(t *testing.T) {
				setAutoModelRetryTestStatusCodes(t, strconv.Itoa(status))
				if got := service.ClassifyAutoModelRetry(tc.err); got != service.AutoModelRetryStop {
					t.Fatalf("hard status blacklist must dominate configuration and all exceptions; got %v", got)
				}
			})
		}
	}
	if !found {
		t.Skip("hard status blacklist is currently empty; the overlay regression enables 404 without changing repository settings")
	}
}

func TestClassifyAutoModelRetryGuards(t *testing.T) {
	local := func(code types.ErrorCode, status int, options ...types.NewAPIErrorOptions) *types.NewAPIError {
		return types.NewErrorWithStatusCode(errors.New("This model does not support the requested feature"), code, status, options...)
	}
	cases := []struct {
		name  string
		err   *types.NewAPIError
		codes string
		want  service.AutoModelRetryScope
	}{
		{"nil", nil, "100-599", service.AutoModelRetryStop},
		{"empty", &types.NewAPIError{}, "100-599", service.AutoModelRetryStop},
		{"skip-upstream", autoModelRetryTestError(503, "server_error", "", "Unavailable"), "100-599", service.AutoModelRetryStop},
		{"skip-compatibility", autoModelRetryTestError(404, "model_not_found", "", "Requested model not found"), "100-599", service.AutoModelRetryStop},
		{"always-skip-code", local(types.ErrorCodeBadResponseBody, 400), "100-599", service.AutoModelRetryStop},
		{"local-validation-400", local(types.ErrorCodeInvalidRequest, 400), "100-599", service.AutoModelRetryStop},
		{"local-validation-500", local(types.ErrorCodeInvalidRequest, 500), "100-599", service.AutoModelRetryStop},
		{"local-conversion", local(types.ErrorCodeConvertRequestFailed, 500), "100-599", service.AutoModelRetryStop},
		{"local-read-body", local(types.ErrorCodeReadRequestBodyFailed, 400), "100-599", service.AutoModelRetryStop},
		{"local-invalid-body", local(types.ErrorCodeBadRequestBody, 400), "100-599", service.AutoModelRetryStop},
		{"explicit-channel-configuration", local(types.ErrorCodeChannelNoAvailableKey, 400), "", service.AutoModelRetryNextChannel},
		{"explicit-channel-mapping", local(types.ErrorCodeChannelModelMappedError, 500), "", service.AutoModelRetryNextChannel},
		{"skip-channel-configuration", local(types.ErrorCodeChannelModelMappedError, 400, types.ErrOptionWithSkipRetry()), "100-599", service.AutoModelRetryStop},
		{"channel-invalid-status", local(types.ErrorCodeChannelInvalidKey, 0), "100-599", service.AutoModelRetryStop},
		{"channel-success-status", local(types.ErrorCodeChannelInvalidKey, 200), "100-599", service.AutoModelRetryStop},
		{"transport-enabled", local(types.ErrorCodeDoRequestFailed, 500), "500", service.AutoModelRetryNextChannel},
		{"transport-excluded", local(types.ErrorCodeDoRequestFailed, 500), "", service.AutoModelRetryStop},
	}
	// The same skip option must dominate both configured and compatibility paths.
	types.ErrOptionWithSkipRetry()(cases[2].err)
	types.ErrOptionWithSkipRetry()(cases[3].err)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setAutoModelRetryTestStatusCodes(t, tc.codes)
			if got := service.ClassifyAutoModelRetry(tc.err); got != tc.want {
				t.Fatalf("scope=%v, want=%v (stop=0, next-model=1, next-channel=2)", got, tc.want)
			}
		})
	}
	for _, status := range []int{-1, 0, 99, 200, 204, 299, 600, 999} {
		t.Run("invalid-or-success-status/"+strconv.Itoa(status), func(t *testing.T) {
			setAutoModelRetryTestStatusCodes(t, "100-599")
			err := autoModelRetryTestError(status, "model_not_found", "model", "Requested model not found")
			if got := service.ClassifyAutoModelRetry(err); got != service.AutoModelRetryStop {
				t.Fatalf("status %d must stop even with model evidence and all status codes enabled; got %v", status, got)
			}
		})
	}
}

func TestClassifyAutoModelRetryNonConfigurationChannelStatusPolicy(t *testing.T) {
	for _, code := range []types.ErrorCode{types.ErrorCodeChannelResponseTimeExceeded, "channel:unknown_failure"} {
		for _, enabled := range []bool{false, true} {
			t.Run(string(code)+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "", service.AutoModelRetryStop
				if enabled {
					codes, want = "408", service.AutoModelRetryNextChannel
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				err := types.NewOpenAIError(errors.New("Channel request timed out"), code, 408)
				if got := service.ClassifyAutoModelRetry(err); got != want {
					t.Fatalf("non-configuration channel errors must not bypass the status list: scope=%v, want=%v", got, want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryGenericStatusPolicy(t *testing.T) {
	// These are upstream errors. Local dispatch/response/budget guards belong
	// to the auto controller, not the status configuration or named-model path.
	cases := []struct {
		status  int
		code    string
		message string
	}{
		{400, "invalid_request", "Invalid request field independent of model"},
		{401, "invalid_api_key", "Invalid API key"},
		{402, "insufficient_quota", "Account balance exhausted"},
		{403, "access_denied", "Credential access forbidden"},
		{404, "route_not_found", "Endpoint not found"},
		{405, "method_not_allowed", "HTTP method not allowed"},
		{408, "request_timeout", "Upstream request timeout"},
		{413, "payload_too_large", "Upstream body size limit exceeded"},
		{422, "invalid_request", "Invalid request structure"},
		{429, "rate_limit_exceeded", "Too many requests"},
		{500, "server_error", "Upstream unavailable"},
		{501, "not_implemented", "Endpoint is not implemented"},
		{503, "server_error", "Upstream unavailable"},
		{504, "gateway_timeout", "Upstream gateway timeout"},
		{524, "upstream_timeout", "Upstream connection timed out"},
	}
	for _, tc := range cases {
		for _, enabled := range []bool{false, true} {
			t.Run(strconv.Itoa(tc.status)+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
				codes, want := "", service.AutoModelRetryStop
				if enabled {
					codes, want = strconv.Itoa(tc.status), service.AutoModelRetryNextChannel
				}
				setAutoModelRetryTestStatusCodes(t, codes)
				err := autoModelRetryTestError(tc.status, tc.code, "", tc.message)
				if got := service.ClassifyAutoModelRetry(err); got != want {
					t.Fatalf("generic upstream status %d (enabled=%t): scope=%v, want=%v (stop=0, next-model=1, next-channel=2)", tc.status, enabled, got, want)
				}
			})
		}
	}
}

func TestClassifyAutoModelRetryAccountClauseChains(t *testing.T) {
	for _, count := range []int{1, 3, 8} {
		for _, separator := range []string{", ", "; ", ": "} {
			clause := "Your account rate limit exceeded for model 'gpt-4o'"
			chain := strings.Repeat(clause+separator, count-1) + clause
			conditional := "; only if the estimate is correct."
			for _, tc := range []struct {
				name, prefix, suffix, code, param string
				want                              service.AutoModelRetryScope
			}{
				{"complete", "", ".", "", "", service.AutoModelRetryNextModel},
				{"conditional", "", conditional, "", "", service.AutoModelRetryNextChannel},
				{"conditional-model-code", "", conditional, "model_rate_limit_exceeded", "", service.AutoModelRetryNextModel},
				{"conditional-model-param", "", conditional, "", "model", service.AutoModelRetryNextModel},
				{"conditional-later-model", "", conditional + " This model is overloaded.", "", "", service.AutoModelRetryNextModel},
				{"conditional-later-compatibility", "", conditional + " This model is unsupported.", "", "", service.AutoModelRetryNextModel},
				{"wide-head", "Account-wide rate limit exceeded for model 'gpt-4o'; ", ".", "model_rate_limit_exceeded", "model", service.AutoModelRetryNextChannel},
				{"wide-tail", "", "; Account-wide rate limit exceeded for model 'gpt-4o'.", "model_rate_limit_exceeded", "model", service.AutoModelRetryNextChannel},
				{"wide-model-only-tail", "", "; Account-wide rate limit exceeded only for model 'gpt-4o'.", "", "", service.AutoModelRetryNextModel},
				{"wide-available-siblings-tail", "", "; Account-wide rate limit exceeded for model 'gpt-4o'; other models remain available.", "", "", service.AutoModelRetryNextModel},
				{"wide-conditional-tail", "", "; Account-wide rate limit exceeded for model 'gpt-4o'" + conditional, "model_rate_limit_exceeded", "model", service.AutoModelRetryNextModel},
				{"global-head", "Your account's quota is exhausted; ", ".", "model_rate_limit_exceeded", "model", service.AutoModelRetryNextChannel},
				{"global-tail", "", "; Organization quota exhausted across all models.", "model_rate_limit_exceeded", "model", service.AutoModelRetryNextChannel},
				{"conditional-global-head", "Your account's quota is exhausted; ", conditional, "model_rate_limit_exceeded", "model", service.AutoModelRetryNextModel},
				{"independent-model-completion", "", "; Rate limit exceeded on model 'gpt-4o'.", "", "", service.AutoModelRetryNextModel},
				{"compatibility-completion", "", "; This model is not supported.", "", "", service.AutoModelRetryNextModel},
				{"metadata-completion", "", "; requested model: gpt-4o.", "", "", service.AutoModelRetryNextModel},
			} {
				t.Run(strconv.Itoa(count)+"/"+separator+"/"+tc.name, func(t *testing.T) {
					setAutoModelRetryTestStatusCodes(t, "429")
					code := tc.code
					if code == "" {
						code = "rate_limit_exceeded"
					}
					message := tc.prefix + chain + tc.suffix
					if got := service.ClassifyAutoModelRetry(autoModelRetryTestError(429, code, tc.param, message)); got != tc.want {
						t.Fatalf("account chains must preserve completion and wide/independent evidence: message=%q scope=%v want=%v", message, got, tc.want)
					}
				})
			}
		}
	}
}

func TestClassifyAutoModelRetryAccountChainMessageIsolation(t *testing.T) {
	setAutoModelRetryTestStatusCodes(t, "429")
	clause := "Your account rate limit exceeded for model 'gpt-4o'"
	chain := strings.Repeat(clause+"; ", 7) + clause
	// Reuse the same suffix lengths in different messages and scope contexts.
	// A cached completion must never outlive the one message being classified.
	for _, tc := range []struct {
		message string
		want    service.AutoModelRetryScope
	}{
		{chain + ".", service.AutoModelRetryNextModel},
		{chain + "#", service.AutoModelRetryNextChannel},
		{chain + ".", service.AutoModelRetryNextModel},
		{chain + "; only if the estimate is correct.", service.AutoModelRetryNextChannel},
		{strings.ReplaceAll(chain, "Your account", "Account-wide") + ".", service.AutoModelRetryNextChannel},
		{chain + ".", service.AutoModelRetryNextModel},
		{chain + "; only if the estimate is correct.", service.AutoModelRetryNextChannel},
	} {
		if got := service.ClassifyAutoModelRetry(autoModelRetryTestError(429, "rate_limit_exceeded", "", tc.message)); got != tc.want {
			t.Fatalf("account completion leaked between messages: message=%q scope=%v want=%v", tc.message, got, tc.want)
		}
	}
}

// Keep scaling measurable without a machine-dependent timing assertion in tests.
// Run with -bench=BenchmarkClassifyAutoModelRetryAccountClauseChain -benchmem.
func BenchmarkClassifyAutoModelRetryAccountClauseChain(b *testing.B) {
	previous := operation_setting.AutomaticRetryStatusCodeRanges
	b.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = previous })
	if err := operation_setting.AutomaticRetryStatusCodesFromString("429"); err != nil {
		b.Fatal(err)
	}
	for _, count := range []int{1, 8, 32, 128, 512} {
		b.Run("clauses="+strconv.Itoa(count), func(b *testing.B) {
			clause := "Your account rate limit exceeded for model 'gpt-4o'"
			message := strings.Repeat(clause+"; ", count-1) + clause + "."
			err := autoModelRetryTestError(429, "rate_limit_exceeded", "", message)
			b.ReportAllocs()
			b.SetBytes(int64(len(message)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if got := service.ClassifyAutoModelRetry(err); got != service.AutoModelRetryNextModel {
					b.Fatalf("affirmative account chain changed scope: got=%v", got)
				}
			}
		})
	}
}

func TestClassifyAutoModelRetryCompatibilityValueClauseBoundary(t *testing.T) {
	for _, delimiter := range []struct{ name, text string }{
		{"semicolon", ";"},
		{"colon", ":"},
		{"period", "."},
		{"exclamation", "!"},
		{"question", "?"},
	} {
		for _, tc := range []struct {
			name, continuation string
			incompatible       bool
		}{
			{"unrelated-model-mention", "a validation error occurred with this model.", false},
			{"later-model-incompatibility", "this model does not support the requested feature.", true},
			{"later-value-incompatibility", "'reasoning_effort' does not support 'max' with this model.", true},
			{"later-decimal-value", "'temperature' does not support 0.7 with this model.", true},
		} {
			for _, enabled := range []bool{false, true} {
				t.Run(delimiter.name+"/"+tc.name+"/enabled="+strconv.FormatBool(enabled), func(t *testing.T) {
					codes, want := "429", service.AutoModelRetryStop
					if enabled {
						codes, want = "400", service.AutoModelRetryNextChannel
					}
					if tc.incompatible {
						want = service.AutoModelRetryNextModel
					}
					setAutoModelRetryTestStatusCodes(t, codes)
					message := "This endpoint does not support streaming" + delimiter.text + " " + tc.continuation
					err := autoModelRetryTestError(400, "invalid_request", "", message)
					if got := service.ClassifyAutoModelRetry(err); got != want {
						t.Fatalf("unsupported values cannot cross clauses, but later affirmative model clauses remain evidence: message=%q scope=%v want=%v", message, got, want)
					}
				})
			}
		}
	}
}
