# Auto routing, scoring and scoped retries

This policy applies to the virtual `auto` model. Named-model selection and ordinary
retry behavior are unchanged. Effort adaptation is described separately in
[auto-reasoning-effort.md](auto-reasoning-effort.md).

## Selection and budget

The initial plan remains channel-first: group order, channel priority, channel
health, channel weight, then model health and model weight within the channel.
Every concrete `(group, model, channel)` route occurs at most once. Token model
allowlists, billing eligibility, enabled abilities, local daily/RPM limits and
strict pair cooldowns are checked before dispatch.

The first selected channel freezes the retry budget, including its override.
`RetryTimes` counts extra attempts, not channels. Switching model, channel or group
does not reset this budget. An explicit channel lock, cancellation, broken client
delivery, a response already written, and skip-retry errors still block fallback.

## Three retry outcomes

A retry decision has three possible results:

* Stop and return the current error.
* Try the next model on this channel, for explicitly model-scoped failures.
* Skip the remaining models on this channel for the current request.

Ordinary retry eligibility follows `AutomaticRetryStatusCodes`. Generic allowed
errors, such as authentication/account failures, an unspecified or account-scoped
429, transport failures and generic server errors, advance to the next channel.
An explicitly model-scoped permission, rate-limit or overload error can try another
model on the same channel, provided its status is enabled for ordinary retries.

There is one compatibility exception. At status 400, 404, 405, 422 or 501, a
positively identified model incompatibility can still try another model, even if
that status is absent from the ordinary retry list. Examples include a structured
`model_not_found`, `unsupported_model_feature` or `context_length_exceeded` error.
A generic bad request, missing endpoint, method error, or mere mention of a model
is not enough to activate this exception. Clear account/authentication evidence
must not become model-scoped merely because the message also mentions a model.
Account-limit text has one scope owner: a conditional or unexplained continuation
rejected there cannot be accepted again by a broader account-prefix fallback.
Affirmative model-specific limits retain sibling fallback, including the existing
failure-before-target and target-before-failure forms. Undecidable account text
alone does not override structured model evidence or a separate affirmative model
clause.

Thus the previous unconditional status-only compatibility shortcut is retired,
without deleting useful model compatibility fallback. A generic 400 excluded from
the configured list stops instead of trying every model. Status 408 is not forced
on: include it in the configured list if upstream request timeouts should fail over.
Existing explicitly typed channel-configuration errors retain their channel-failure
handling; they are not treated as successful upstream observations.

A channel-scoped exclusion lasts only for this incoming request. It is not a local
RPM/day-limit event, does not disable the channel, and does not create a persistent
channel-wide cooldown. An account/default 429, including one carrying `Retry-After`,
will not immediately fan out across that channel's other models in the same request.
No sleep or cross-request global Retry-After timer is introduced. Explicit model-only
limits remain model-only. If response bytes have already been written, including a
streaming HTTP-200 protocol error, no replacement answer is spliced into that stream.

## Feedback attribution and order

Each relay attempt resets its upstream-dispatch marker. The common request guard
sets it only after local conversion/validation and RPM admission, immediately before
invoking the upstream request callback. A refused connection still counts as an
attempt. A local validation error that never enters dispatch cannot lower health
scores or create an auto cooldown.

This does not relax model parameter validation. For example, if the local capability
table rejects `gpt-5.4` with `reasoning_effort=max`, the request can still return 400,
but a never-contacted upstream must not be penalized. Keeping max in the auto effort
mapper is not a promise that every downstream capability validator accepts it.

Feedback is deduplicated within the request by `(group, model, channel)`, keeping the
last observation for each key. At request completion, these final observations are
applied in the order they actually occurred within that request. Replacing an older
observation also moves its position. Channel EWMA spans model keys, so unordered map
iteration is not a valid application order. A batch drains only once.

The existing success/latency formula is unchanged. Scores describe observed
availability and responsiveness, not model reasoning quality. Model scores are
pair-specific, while a group/channel score also reflects that channel's other models.
Score state is process-local, decays toward neutral over idle time, and is not a
persistent reputation record.

## Persistent cooldown remains pair-scoped

Observed failures, unusable delivered answers and qualifying slow outcomes can cool
only the observed `(group, model, channel)`. The existing timing rule and ladder are
unchanged: 15 minutes, doubling to a six-hour cap. Cooldown is strict and survives a
restart. Fast successful recovery clears that pair's cooldown and escalation state;
another model succeeding does not clear it. No model-empty channel cooldown rows are
introduced, and no historical cooldowns are deleted by this change.

## Verification

`controller/auto_model_feedback_order_test.go` exercises the real feedback queue,
service score updates and route planner against a neutral channel, including replaced
keys. Attribution tests capture real upstream hits and refused connections.

`scripts/test-auto-routing-policy.py` runs a declared synthetic policy matrix against
a supplied binary. Run both `--cache false` and `--cache true`. It checks exact route
sequences, local-validation controls, budgets, exact failed-pair keys before and after
restart, request-only channel exclusions, and process cleanup. Refusal tests reserve
a bound non-listening endpoint and correlate transport diagnostics by request ID. It requires a loopback-only
network namespace before the gateway's wildcard setup listener is started.

The effort and SSE harnesses remain separate controls. A generic upstream 400 outside
the retry list intentionally no longer produces an immediate successful fallback;
record this expected SSE-harness difference instead of claiming all response bytes
are unchanged. Synthetic fixture results are not measurements of real providers.
