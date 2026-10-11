# HTTP 200 credit-error banners and auto feedback

Some OpenAI-compatible upstreams return a successful HTTP envelope whose content
contains only an error banner. For example, the request model
`moonshotai/kimi-k2.7-code` has been observed returning:

```text
[Upstream Error: 402 "You have no remaining credits. Purchase pre-paid credits to continue using Inference Providers. Alternatively, subscribe to PRO to get monthly included credits."]
```

The actual observed client response was HTTP 200 with SSE `choices[].delta.content`,
not an HTTP 402 response or a top-level JSON `error`. A short first-response time
measures arrival of those bytes, not successful model output. The upstream may use
a differently cased response model name; feedback must still use the requested
concrete route key.

## Observation and existing feedback owners

`relay/common/answer_observation.go` observes delivered response content. A standalone
bracketed `Upstream Error` banner is treated like the existing gateway warning
banners. The complete `Upstream Error:` label is required. Closed banners and
single-line banners with a missing closing bracket are recognized, as with the
existing truncated warning banners. A `length` or `content_filter` finish remains
inconclusive rather than evidence of a failed upstream. Content fragments are
considered together within their own choice, not across unrelated choices. Genuine
prose before or after a banner, useful reasoning,
and tool calls still count as a usable answer. Quoted examples are not rejected
merely for containing the words `Upstream Error` or `credits`.

Only bounded content is retained for this observation. If that bound is exceeded,
the observer conservatively avoids declaring an error from a truncated prefix.
Unknown formats retain their existing conservative behavior. Explicit top-level
protocol errors still take precedence over partial output.

The observer does not rewrite the response, retry after response bytes have been
sent, or alter token accounting, billing, or refunds. The original status and first
response body remain intact. It reuses the existing controller feedback path:

1. A delivered unusable answer is not scored as a successful auto result.
2. Only the observed `(group, model, channel)` combination enters durable cooldown.
3. The existing first cooldown is 15 minutes. Repeated failures after expiry use the
   existing escalating window, capped at 6 hours.
4. Later auto requests skip that combination, including after service restart.
   Success on another combination does not clear it.

This does not introduce a durable channel-wide ban, change configured retry status
codes, or add a special Kimi model rule. Named-model requests preserve their normal
routing semantics and do not acquire virtual-auto cooldown behavior.

When the existing `ERROR_LOG_ENABLED` setting is enabled, unusable HTTP 200 answers
also produce the existing zero-quota diagnostic error row, correlated to request,
model, and channel. Root log views retain details; token log views keep their current
sanitized error fields. Consumption rows are not relabeled or removed.

## Repeatable verification

The fixture never reads production data or credentials. Use the real candidate
binary inside a loopback-only network namespace:

```bash
unshare --net -- sh -c 'ip link set lo up && exec python3 -B scripts/test-auto-credit-errors.py "$@"' sh /absolute/path/to/new-api
python3 -B -m unittest discover -s scripts -p test_auto_credit_errors.py -v
```

The matrix covers both cache modes, the exact banner and requested model key,
character-split content, non-stream content, named requests, actual-answer controls,
independent choices, diagnostic logs, repeated requests and restart persistence.
Reports are appended after each case, with expected IDs, artifact SHA256 and fixture
cleanup evidence. Exit 1 means a behavioral failure; exit 2 means fixture failure.
Use `--baseline-results /absolute/prior/results.json` to require unchanged first
response status/body and first-request billing against a prior binary. A named live
probe identifies the response shape; it is not evidence of virtual-auto cooldown.
