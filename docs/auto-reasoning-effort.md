# Auto reasoning effort compatibility

This policy applies only to requests using the virtual `auto` model. It adapts the caller's effort after a concrete upstream model has been selected.

* GPT, DeepSeek and GLM: keep the supplied effort unchanged.
* Qwen3 and Grok: translate `max` to `xhigh`.
* Kimi and MiniMax: omit the effort parameter. Other thinking controls are not disabled or removed.
* Every other model, including Gemini: translate `max` to `high`.
* Otherwise keep non-`max` values unchanged. An omitted effort stays omitted.

The policy covers Chat `reasoning_effort`, nested `reasoning.effort`, Responses `reasoning.effort`, Claude `output_config.effort` and existing Gemini thinking-level fields. Existing provider conversions retain their own wire format. This is not a new thinking-enable or token-budget policy.

Model matching is case-insensitive and tolerates provider namespaces, colon-separated channel labels and leading bracket tags, for example `nim/nvidia/glm-5.3-flash`, `[次]deepseek-v4.1-flash`, `intern/inkstone/Qwen3.8-Flash` and `deepseek-v4-flash:0731`. The model identifier itself is not changed. Trailing numeric/date and `preview` revision tags retain the preceding model family, including stacked tags. Unknown terminal model identifiers still override family-looking channel labels. The actual mapped upstream name takes priority. If that name is an opaque/unknown alias, `max` gets the `high` fallback rather than guessing its family from a conflicting client alias.

Each retry starts from the caller's original effort. A Gemini attempt receiving `high`, or a Kimi attempt omitting effort, must not change a subsequent GPT/DeepSeek attempt's original `max`.

Named-model requests retain their prior behavior. Existing model-suffix rules and explicit channel parameter overrides retain their prior precedence. Auto also adapts the effort on raw passthrough paths without changing the stored original request; on those paths it uses the model actually sent, not a model mapping that passthrough bypasses. Native Gemini is URL-selected, so its effective URL/mapped model remains authoritative even when a conflicting `model` member is present in the raw body. This ownership follows the selected upstream adaptor, not the client ingress format. Usage logs continue to report the final sent effort, not the caller's pre-adaptation value.

## Verification

```sh
go test ./setting/reasoning ./relay/common ./relay -count=1
unshare --net -- sh -c 'ip link set dev lo up && exec python3 scripts/test-auto-effort-routing.py "$@"' sh /absolute/path/to/new-api
```

The end-to-end harness uses a real gateway binary, fresh fixture databases and synthetic loopback providers. It must never be pointed at a production database or public bootstrap port.
