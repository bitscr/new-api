# Auto routing, scoring and scoped retries

This policy applies to the virtual `auto` model. Named-model selection and ordinary
retry behavior are unchanged. Effort adaptation is described separately in
[auto-reasoning-effort.md](auto-reasoning-effort.md).

## Selection and budget

The plan is built as: group order, then eligibility filters, then channel
priority in descending order as a hard boundary. Inside one priority tier the
planner separates reliability from speed:

1. Reliability admission uses decayed recent business success/failure counts.
   A route needs at least `AutoModelMinObservations` (default 5, allowed range
   1–70), a recent reliability estimate of at least
   `AutoModelReliabilityThreshold` (default 0.95), a fresh mode-specific
   successful-latency summary with at least three samples, and no
   pending-recovery flag. Only these routes enter main traffic. The 70 ceiling
   leaves headroom below the 100-count evidence cap after freshness-window
   decay, so every accepted threshold is reachable by real traffic.
2. Main candidates are grouped into success-latency pools. The first pool is
   anchored at the lowest latency `L`; routes with `latency <= L + max(
   AutoModelLatencyToleranceMs, AutoModelLatencyToleranceRatio * L)` share it.
   Remaining reliable routes form progressively slower backup pools.
3. Inside a pool, the existing two-level weighted sampling is unchanged: pick a
   channel by channel weight, then a model by model weight. Ten models on one
   channel do not multiply that channel's share.
4. A small, request-scoped exploration draw may prepend one candidate from
   outside the main pool (unverified/stale trial or a slower successful route).
   Known failures and pending-recovery routes do not participate in this draw.
   The base ratio is `AutoModelExploreRatio` (default 0.05), rising toward
   `AutoModelExploreMaxRatio` (default 0.10) with the share of candidates that
   still need evidence. The two values are validated and saved as one pair;
   every plan reads them from one configuration snapshot so it cannot observe a
   half-applied pair. Exploration uses the current user request itself; it
   never sends probes, shadow requests, parallel races or extra samples.
5. Routes in the trial range remain eligible for normal degraded fallback when
   no reliable route exists. Each trial route has at most one in-flight real
   business request per process; planning and page queries never consume it.
   Within each priority tier, a trial with fresh successful latency in the
   request's mode, a recent success ratio meeting the reliability threshold,
   and no pending-recovery flag is preferred over unverified trials. These
   successful trials are ordered into latency pools after the admitted main
   and backup pools and remain labeled `successful_trial`, without bypassing
   the main-pool sample thresholds. When no admitted route exists, normal
   traffic uses the fastest successful trial pool and the configured small
   exploration draw applies to candidates outside it. Fully random cold-start
   fallback is used only while that tier has no qualifying successful trial.
   Within that tier untouched trials precede known failures. A failed route
   remains a last recovery fallback after cooldown expiry, subject to its trial
   slot; neither evidence decay nor a restart turns it into an untouched route.

An unverified exploration attempt with an immediate successful fallback has a
20-second wait limit from actual upstream dispatch, while the response remains
uncommitted. Expiry cancels only that upstream attempt and records pair-scoped
failure before replanning. It does not exclude or disable the channel, which may
also serve the successful fallback. A committed answer stops the timer; late first
output after expiry is discarded. Ordinary manual-priority selection, cold-start
selection and demonstrated slow successful routes keep their existing timeouts.
The limit requires remaining retry budget and the normal request retry guards.

Priority remains a hard boundary: exploration, trial admission or a faster route
never jumps ahead of a higher-priority tier. Every concrete `(group, model,
channel)` route occurs at most once. Token allowlists, billing eligibility,
enabled abilities, local daily/RPM limits and strict pair cooldowns are checked
before dispatch. Reliability and speed evidence are never synthesized from
startup, idle time, page refresh, cooldown expiry or manual channel tests.

The request's stream flag is parsed from the actual Chat/Claude/Responses body
(or from Gemini's streaming path) and is used for both the first plan and every
retry. A stream request must never be ranked with non-stream latency evidence.

The first selected channel freezes the retry budget, including its override.
`RetryTimes` counts extra attempts, not channels. Switching model, channel or group
does not reset this budget. An explicit channel lock, cancellation, broken client
delivery, a response already written, and skip-retry errors still block fallback.

The auto path does not share `RetryTimes` with named requests. That value is tuned for
a single named model and is often large, while every auto attempt really does contact
a different upstream combination, so it would multiply one client request into dozens
of upstream calls. The frozen budget therefore comes from `AUTO_MODEL_MAX_ATTEMPTS`
(default 4, including the first dispatch); a channel override is still recorded for
consistent downstream retry numbering but cannot enlarge it. Named-model retry
semantics are untouched.

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
attempt. A local validation error that never enters dispatch cannot create
reliability/latency evidence or an auto cooldown.

This does not relax model parameter validation. Official OpenAI models now clamp an
unsupported effort to the highest supported value instead of returning a local 400
(for example, `gpt-5.4` with `reasoning_effort=max` becomes `xhigh`). Third-party
adaptors may still reject a request before dispatch; a never-contacted upstream must
not be penalized, and keeping an effort in the auto mapper is not a promise that every
downstream validator accepts it.

Feedback is deduplicated within the request by `(group, model, channel)`, keeping the
last observation for each key. Each entry pins its real observation time at enqueue;
a later long backup attempt must not refresh an earlier attempt's freshness or
cooldown start time. At request completion, these final observations are applied in
the order they actually occurred within that request. Replacing an older observation
also moves its position, so unordered map iteration is not a valid application order.
A batch drains only once.

For auto requests, each attempt's successful response is staged above the normal
relay writer until the answer observer confirms text, reasoning or a valid tool
call. A non-stream body is buffered up to a hard cap and committed only after
validation; a stream's initial frames are buffered and released at the first valid
output. Crossing the cap commits the buffered prefix and streams the remaining
bytes directly, so staging never holds more than the cap. If an explicitly invalid
HTTP 200 (empty body, banner-only, or explicit protocol error) arrives before
commit and budget/candidates remain, the staged response is discarded, the pair's
failure is published immediately, and the next replanned route is dispatched. Once
any bytes are committed, or the staging cap is reached, no second candidate is
spliced into the response. A committed response whose bytes the observer cannot
classify (unknown shape, truncated/ambiguous choices, observation limit) is
delivered but recorded as neutral: it is neither a success nor a failure. A candidate
that never wrote a response and cannot be replaced ends as a gateway error instead
of an empty 200. SSE keepalive pings are dropped while a candidate is uncommitted
and serialized with answer writes once it is; a discarded candidate's headers are
restored so the next candidate can set SSE headers again. Named-model requests keep
their previous behavior.

A dispatched trial route keeps its process-local slot until its feedback has been
published. A long fallback attempt therefore cannot let another request re-enter the
same trial route before the first outcome is visible. A pre-dispatch local rejection
releases the slot immediately; every normal exit, failure and cancellation still
releases it before the request ends.

Reliability is estimated from decayed business success and failure counts, not
from a blended score. Successful stream requests record the time from this
attempt's real upstream dispatch to the first actual answer delta (text,
reasoning or a tool call); role frames, empty deltas, heartbeats, usage and
banners do not count. Non-stream requests record completion duration from the
same dispatch point, separately from the full request processing time kept for
logs. The two latency summaries are never mixed. A failed or later-truncated
stream does not submit a success-latency sample. Output detection is
protocol-aware: metadata, echoed input and tool definitions never count as an
answer, while text, reasoning and valid tool calls do. OpenAI Chat, Claude,
Gemini and Responses text channels each accumulate bounded incremental fragments;
a gateway banner split across SSE packets is not credited until text outside the
completed banner appears, and a banner-only native answer is classified as
unusable. Only a channel's first fragment may drop leading whitespace; later
fragments are appended verbatim. Responses full snapshots (`part.done`,
`item.done`, `completed`) replace or merge the delta buffer using one
`output_index`-canonical channel, so repeated end events cannot double-count text
length. Ambiguous or unknown shapes leave latency unknown rather than guessing.

Only the mode that receives a new successful-latency sample folds that mode's
history. Feedback in the other mode updates reliability but leaves the samples
and their observation time untouched; read-time evaluation applies the elapsed
decay once. A delayed, out-of-order observation may add its count but cannot
rewind the recorded observation time. Latency sample counts and reliability
counts decay with `AutoModelScoreDecayMinutes`; the latency value itself is not
decayed toward zero, so an idle route is treated as unknown rather than
increasingly fast. Evidence is pair-specific and process-local; the database
keeps the latest snapshot per key, not a globally merged event history. Cooldown
and evidence caches are process-local as well: feedback is persisted before the
same process plans the next candidate, but other instances are not notified and
may retain their previous in-memory cooldown until they reload/restart. No
cross-instance push, shared Redis dependency or periodic stale-cache overwrite is
introduced.

## Persistent cooldown remains pair-scoped

Observed failures and unusable delivered answers can cool
only the observed `(group, model, channel)`. A slow but successful answer is latency
evidence, not a failure: it stays eligible and is ordered into a slower backup pool,
never cooled. Cooldown is strict and survives a restart. Any successful recovery
clears that pair's cooldown and escalation state, even if timing was unavailable;
another model succeeding does not clear it. The latest recovery time is kept as an
ordering barrier even after the cooldown row is deleted, so a failure observed before
recovery and published after a long backup attempt cannot re-cool the route. The
barrier is advanced by successful observations and by administrator clears, and it
only moves forward: a delayed older success can never move it backwards. No model-empty channel cooldown rows are
introduced, and no historical cooldowns are deleted by this change.

Jitter failures such as rate limits, timeouts and server errors keep the existing
ladder: 15 minutes, doubling to a configurable cap. There is no deterministic
cooldown tier any more: a failure matching an administrator-configured
deterministic keyword retires the `(group, model, channel)` pair immediately —
the model is removed from that channel's `models` list and the abilities are
rebuilt, an emptied channel is auto-disabled — because retrying a deterministic
failure cannot succeed and a cooldown would only postpone it. A generic bad
request, a context-length error, or unrecognised text is treated as jitter: an
unknown failure is retried rather than removing a model that may only be
temporarily unavailable.

Deterministic keywords are administrator-configured in the panel and stored in
the database (`AutoModelPermanentKeywords`); nothing is built into the source, so
an unconfigured deployment has no deterministic criteria at all. A keyword match
has exactly one effect: the pair is retired immediately. The channel itself is
kept — credentials and configuration are reused — and no probe is ever sent.
If the retirement transaction fails, the pair is cooled and marked pending
recovery immediately, so database failure cannot leave it repeatedly selectable.
Manual channel-test retirement failures do not create business evidence.
Transient keywords (`AutoModelTransientKeywords`) outrank deterministic ones and
pin a self-healing error to the jitter ladder; context overflow is a protected
transient failure that no keyword can override.

Keyword writes are validated before they reach the database, because a keyword is a
high-consequence criterion: a match removes the pair immediately. Entries shorter than
two characters, entries that only describe a protected transient failure (which the
protected predicate outranks, so they could never take effect), and bare catch-all
words such as `error` are refused with an explanatory message. Specific upstream
error strings are always accepted — that is the whole point of the feature. The
refusal arrives as HTTP 200 with `success:false`, so a client that only checks the
transport status would report a rejected keyword as saved; the admin panel reads the
body's `success` and surfaces the server's message.

This is the reversible reading of "remove the model": the pair is gone from auto
routing because the model no longer exists in the channel's `models` list, not
because of a timer. Re-adding the model to the channel restores selection, and the
admin panel exposes the live keyword lists. That visibility is not optional: a
keyword match takes effect immediately and lasts until an administrator acts, so
the panel must show exactly which criteria are configured.

## Evidence window and cooldown window

The evidence decay window (`AutoModelScoreDecayMinutes`, default 360 minutes)
must stay longer than the early tiers of the transient cooldown ladder (15
minutes, doubling). At a shorter window, success/failure counts and latency
sample counts would already be gone when the cooldown expired, leaving only
the cooldown's failure marker. Freshness is derived as one
quarter of this window. Latency values remain the last observed values; they are
not decayed toward zero, so stale evidence becomes unknown instead of appearing
faster. Expired cooldown rows retain failure memory and their escalation level
until business success or an administrator clear. They no longer block dispatch,
but remain a source of pending-recovery classification if score evidence was
pruned or lost before a restart. A restored success from a strictly later second
reconciles an older cooldown left by a failed deletion; ambiguous same-second
events remain conservative because cooldown timestamps have second precision.

## Clearing evidence

An administrator clear removes online evidence immediately and writes a version-3
tombstone through the same per-key ordering guard. A snapshot captured before the
clear cannot be inserted afterwards; a restart restores the tombstone as a marker,
never as confidence or latency. A real post-clear observation is newer and replaces
the marker normally. If either the tombstone write or the cooldown-row deletion fails, the online
clear still takes effect and the API reports the persistence failure; the
background save cycle retries both. Tombstone and cooldown retries share the
worker's single I/O budget, use context-bound SQL, and are confirmed per key; a
busy cooldown lock only skips that background round. Synchronous cooldown loads,
legacy/expired-row cleanup, failure/cooldown writes and administrator clears also
use one bounded context per operation (shared between the cooldown DELETE and the
score tombstone for a clear). A timeout releases the cooldown lock, keeps the
pending state, and lets the background worker finish it later.

Feedback publication, deterministic retirement and administrator clear share a
per-route publication lock (taken before the score/cooldown locks). A retirement
holds it through its database transaction, so a clear that returns success cannot
be followed by a pre-clear retirement committing model/ability deletion or channel
disablement. The retirement transaction, ability rebuild, CAS retries and their
backoff share one bounded context (the same two-second I/O budget); a timeout
releases the route lock and lets feedback and administrator clear proceed instead
of waiting on a blocked row lock.
The clear time is ordering metadata only and never becomes a route's last observation
time.

The status page uses the same targets, evidence, time and one configuration snapshot
as the planner. It shows decayed (effective) sample counts and freshness for both
modes, so a row cannot appear sample-sufficient while classification says otherwise.
A failed target/evidence read returns an explicit error instead of an empty table.
The frontend discards late responses when switching between stream and non-stream
previews.

## Diagnostics

Auto routing emits stable `auto route diag:` events for the initial plan (`plan`),
candidate selection (`attempt stage=selected`), confirmed upstream dispatch
(`attempt stage=dispatched`), each completed dispatched attempt (`result`),
feedback publication (`feedback`), each successful reroute (`switch`), blocked reroutes
(`no_switch`: response committed, budget exhausted, client canceled, channel lock,
staging unavailable, plan empty, rebuild failed, or retry classification), an
unclassifiable committed answer (`unknown_answer`), and a final `stop` summary.
Plan, plan-failed and switch events include candidate counts grouped as route
excluded, cooling, channel excluded, model excluded, unauthorized, whitelist
excluded, no targets, zero weight and billing/endpoint excluded, plus main/backup/
trial split. Logs contain route metadata and error codes only, never keys, prompts
or response bodies.

All events use the existing request logger, including its severity, timestamp and
request ID, and the same console/daily `oneapi-YYYYMMDD.log` output. There is no
separate diagnostic file. New free-text diagnostic fields collapse line breaks and
tabs and have length limits so one event remains one line.

Plans include the planning time, request mode, actual exploration draw/probability,
captured routing settings and runtime retry/cooldown controls. Each previewed route
shows its actual priority and channel/model weights, effective business and latency
sample counts, evidence ages, and missing main-admission gates. `priority=hard`
records the manual-priority rule: successful routes are preferred within the same
group and priority tier. The preview is capped at ten routes; category totals cover
the entire plan, and selected/result events carry the selected route's evidence
even beyond that preview. Cooling entries show their real group, escalation level,
remaining duration and whether a database write is pending. Failed plan reads are
marked `plan_complete=false`; counts then describe only the part already read.

`result` separates full attempt processing time from actual upstream dispatch time
and reports first valid output when known. Its `attempt` matches the selected
attempt index, while `real_dispatched` counts only actual upstream dispatches;
local rejections cannot make these counters look like spent dispatch budget.
`exploration_guard` and
`exploration_timeout` record the bounded wait and immediate fallback. `stop` retains
the existing last-attempt `elapsed_ms`, and adds `request_elapsed_ms` from the
existing request start timestamp plus `last_attempt_elapsed_ms`. The server can
report a request-context deadline when one exists; `client_deadline_remaining_ms=?`
means the external client's timeout is unknown, rather than unlimited. Feedback
events show the in-memory evidence and cooldown after publication. This state can
include another request's newer feedback; it is not confirmation that the
asynchronous score snapshot has reached the database. `persist_pending` refers to
the cooldown write/delete only.

Warnings and advice are conditional on the captured facts: exhausted dispatch
budgets point to `AutoModelMaxAttempts`; an excluded retry status points to
`AutomaticRetryStatusCodes`; insufficient successful business samples point to
`AutoModelMinObservations`; a freshness window shorter than the first cooldown
points to `AutoModelScoreDecayMinutes`. Repeated failed exploration can justify
lowering `AutoModelExploreRatio`/`AutoModelExploreMaxRatio`. Local request errors,
fixed skip rules, missing channel metadata and client cancellation are reported
separately because increasing budgets or retry status ranges cannot fix them.
Successful slow routes prompt a comparison with client waiting time. Advice does
not modify configuration or recommend bypassing manual priority, cooling a slow
success, or treating an expired failure cooldown as proof of recovery.

## Verification

`service/auto_model_plan_test.go` covers reliability admission, latency pools,
successful-trial reuse until admission, bounded cold-start exploration, and
fresh mode-specific success requirements. It also covers
exploration prepending and the process-local trial slot.
`service/auto_model_routing_test.go` covers evidence decay, per-mode folding and
out-of-order observations. `service/auto_model_score_clear_test.go` covers clear
tombstones, stale-snapshot rejection, restore and persistence retry.
`service/auto_model_score_status_test.go` covers effective samples and explicit read
failures. `relay/common/first_valid_output_test.go` covers protocol-aware
first-valid-output detection and per-attempt reset; `relay/common/auto_attempt_response_test.go`
covers banner discard, the hard staging cap, keepalive handling and concurrent
writers; `relay/helper/auto_attempt_headers_test.go` covers SSE header reuse after a
discarded candidate. `controller/auto_model_relay_chain_test.go` exercises a real
HTTP upstream through the adaptor, observer, staging writer and feedback store
(banner-only, oversized unclassifiable and untouched-writer cases); other controller
latency/feedback tests cover dispatch-based timing, stream/non-stream evidence and
trial-slot release.

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
