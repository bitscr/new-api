# Auto reasoning effort compatibility

This policy applies only to requests using the virtual `auto` model. It adapts the caller's effort after a concrete upstream model has been selected.

* OpenAI models: use the exact capability table in `pkg/openaimodel`; an unsupported value is raised to the highest supported value. `none` is always kept as thinking-off.
* DeepSeek: fixed `none`/`low`/`high`/`max`; anything else is raised to `max`. `none` is the official thinking-off value and is kept as-is.
* Qwen3.8: fixed `low`/`medium`/`xhigh`; anything else (including `high` and `max`) is raised to `xhigh`. Older Qwen models use fixed `low`/`medium`/`high`/`xhigh`.
* Grok: fixed `low`/`medium`/`high`/`xhigh`; `max` is raised to `xhigh`.
* Claude: fixed `low`/`medium`/`high`/`max`.
* Gemini: 3.6 Flash uses fixed `minimal`/`low`/`medium`/`high`; 3.1 Pro uses `low`/`high`; other Gemini models use `low`/`medium`/`high`; `max` is raised to `high`.
* GLM 5.1: fixed `none`/`minimal`/`high`/`xhigh`; anything else is raised to `xhigh`.
* GLM 5.2: fixed `none`/`minimal`/`high`/`max`; anything else is raised to `max`.
* GLM 5.3 (and unknown GLM versions): fixed `low`/`high`/`max`; anything else is raised to `max`.
* Kimi K3: fixed `low`/`high`/`max`; Kimi K2 and earlier omit the effort parameter.
* MiniMax, Mistral, Llama, Cohere/Command, Ernie, Hunyuan, Doubao, Step, Yi and InternLM: omit the effort parameter. Other thinking controls are not disabled or removed.
* Unknown models: fixed fallback `low`/`high`/`max`; an unmatched value is raised to `max`.
* General rule: keep an incoming value that is in the model's fixed list; otherwise use the highest value in that list. `none` always means thinking-off and is never raised; `minimal` is raised like any other unmatched value. There is no alias mapping. An omitted effort stays omitted. Models that do not accept an effort parameter still omit it rather than receiving `none`.

The policy covers Chat `reasoning_effort`, nested `reasoning.effort`, Responses `reasoning.effort`, Claude `output_config.effort` and existing Gemini thinking-level fields. Existing provider conversions retain their own wire format. This is not a new thinking-enable or token-budget policy.

Model matching is case-insensitive and tolerates provider namespaces, colon-separated channel labels and leading bracket tags, for example `nim/nvidia/glm-5.3-flash`, `[次]deepseek-v4.1-flash`, `intern/inkstone/Qwen3.8-Flash` and `deepseek-v4-flash:0731`. The model identifier itself is not changed. Trailing numeric/date and `preview` revision tags retain the preceding model family, including stacked tags. Unknown terminal model identifiers still override family-looking channel labels. The actual mapped upstream name takes priority. If that name is an opaque/unknown alias, it uses the `low`/`high`/`max` fallback rather than guessing its family from a conflicting client alias.

Each retry starts from the caller's original effort. A Gemini attempt receiving `high`, or a Kimi attempt omitting effort, must not change a subsequent GPT/DeepSeek attempt's original `max`.

Named third-party requests retain their prior passthrough behavior. Official OpenAI named requests now clamp an unsupported effort to the model's highest supported value instead of returning a local 400. Existing model-suffix rules and explicit channel parameter overrides retain their prior precedence. Auto also adapts the effort on raw passthrough paths without changing the stored original request; on those paths it uses the model actually sent, not a model mapping that passthrough bypasses. Native Gemini is URL-selected, so its effective URL/mapped model remains authoritative even when a conflicting `model` member is present in the raw body. This ownership follows the selected upstream adaptor, not the client ingress format. Usage logs continue to report the final sent effort, not the caller's pre-adaptation value.

## Verification

```sh
go test ./setting/reasoning ./relay/common ./relay -count=1
unshare --net -- sh -c 'ip link set dev lo up && exec python3 scripts/test-auto-effort-routing.py "$@"' sh /absolute/path/to/new-api
```

The end-to-end harness uses a real gateway binary, fresh fixture databases and synthetic loopback providers. It must never be pointed at a production database or public bootstrap port.
