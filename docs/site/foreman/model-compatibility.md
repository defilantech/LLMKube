# Foreman model compatibility

The Foreman v0.1 native agent loop assumes the inference endpoint
behind every Agent speaks **OpenAI-style function calling**: it
emits structured `tool_calls` in chat-completions responses, the
loop parses them, dispatches the named tool, and feeds the result
back as a tool-role message keyed by `tool_call_id`. That assumption
is true for most modern open-weights instruct models served via
llama.cpp / `llama-server` / vLLM / mlx-server, but it isn't
universal.

This page is the calibrated table of what we've empirically
validated. If a model isn't here, that doesn't mean it doesn't
work. It means we haven't run it. Pull requests adding entries
welcome.

## How to read this table

- **Role:** the Agent role we tested the model in (coder, reviewer,
  verifier).
- **Tool protocol:** whether the model emits OAI-shaped
  `tool_calls` in llama.cpp / mlx-server / vLLM. ✓ means yes; ✗
  means no.
- **Confabulation rate:** subjective rating of how often the
  model's terminal `submit_result.extra` fields contained text
  that wasn't grounded in its own earlier tool calls. The harness
  reconciles known confabulation surfaces server-side (see
  [#582](https://github.com/defilantech/LLMKube/issues/582) and the
  `reconcileReviewer*` helpers in
  `pkg/foreman/agent/executor_native.go`), so even a high-confab
  model is usable; the rate just describes how much work the
  reconciler does.
- **Notes:** observed quirks worth documenting.

## Tested matrix (v0.4 reviewer release)

| Model | Quant | Host | Role | Tool protocol | Confab | Notes |
|---|---|---|---|---|---|---|
| Qwen3.6-35B-A3B (Carnice MoE) | Q8_0 | M5 Max 128GB | coder | ✓ | low | Reference coder. Verified end-to-end on real LLMKube issues. |
| Qwen3.6-35B-A3B | Q8_0 | M5 Max 128GB | reviewer | ✓ | low | Same model serves as same-family reviewer. Catches Section G (godoc/code consistency) reliably. |
| Devstral-Small-2 24B-Instruct-2512 | Q6_K | Mac Studio 36GB | reviewer | ✓ | high | Tools all dispatch correctly; terminal `submit_result.extra.issueAsk` and `filesTouched` frequently confabulated on multi-file diffs. Harness reconciles both server-side. |
| Gemma 3 27B-it | Q6_K | Mac Studio 36GB | reviewer | ✗ | n/a | **Does not currently work.** Emits tool invocations as Google's native markdown `\`\`\`tool_code` blocks rather than OAI `tool_calls`. The loop sees zero tool_calls on turn 1 and force-terminates as `ModelMisunderstood`. Tracked as [#589](https://github.com/defilantech/LLMKube/issues/589); fixed when the `toolProtocol` adapter work lands in v0.5. |
| Mistral-Small-3.2 24B-Instruct-2506 | Q6_K | Mac Studio 36GB | reviewer | ⚠️ | n/a | **Under investigation.** First chat-completions request hangs indefinitely (llama-server health endpoint stays OK, CPU drops to 1.3%, no client-side timeout fires). May be a Metal-perf path issue specific to this model or an HTTP-streaming-shape issue. Tracked as [#590](https://github.com/defilantech/LLMKube/issues/590). |

## How the harness handles confabulation

For reviewers whose tool protocol works but whose terminal payload
is unreliable, the executor reconciles two fields server-side
before the result is stored:

- **`filesTouched`** is rewritten to the output of
  `git diff --name-only main...HEAD` in the workspace. The model's
  original claim lands at `filesTouchedClaimed` for archaeology.
  Shipped in
  [#584](https://github.com/defilantech/LLMKube/pull/584).
- **`issueAsk`** is checked against the body the model fetched via
  the `fetch_issue` tool. If the claim is a literal substring of
  the body it's marked verified; otherwise it's archived at
  `issueAskClaimed` and rewritten with the first useful paragraph
  of the body. Shipped in
  [#587](https://github.com/defilantech/LLMKube/pull/587).

A new boolean field `issueAskVerified` signals to downstream
consumers whether the stored value came from the model
verbatim or from the harness rewrite.

Since [#645](https://github.com/defilantech/LLMKube/pull/645) the
verification result is *enforced*, not just recorded:

- An unverified `issueAsk` on a **GO** verdict demotes the verdict
  to **NO-GO**. A reviewer that cannot prove it read the issue
  cannot approve a branch. Because escalation reviewers are emitted
  on base NO-GO, the branch is automatically re-reviewed by the
  escalation model instead of being green-lit.
- An unverified `issueAsk` on any other verdict keeps the verdict
  but marks it untrusted.
- In both cases the result extra carries `verdictDemoted: true`,
  `verdictClaimed` (the model's original verdict), and a
  `demotionReason`, mirroring the `issueAskClaimed` convention.
- If `issueAskVerified` is absent entirely (no `fetch_issue` body
  in the transcript, a harness-side gap rather than model
  dishonesty), enforcement does not fire.

[#647](https://github.com/defilantech/LLMKube/issues/647) adds a
second, fully computable check: when the issue body names concrete
files (`config/rbac/role.yaml`, `AGENTS.md`) and the ground-truth
diff touches none of them, the executor flags scope drift
deterministically (`scopeRefs`, `scopeMatched`,
`scopeDriftDetected` in the result extra) and demotes a GO the
same way. No model judgment is involved; an issue that names no
files keeps the check observe-only.

The *anchor fields* downstream tools pivot on (which files did the
diff touch? what does the issue actually ask for?) remain
harness-authoritative, and the verdict now inherits that property:
a verdict that contradicts the harness's evidence check cannot
drive the cascade rule on its own.

## Hybrid-thinking models

Since [#651](https://github.com/defilantech/LLMKube/pull/651) the
loop understands `reasoning_content`: a turn a thinking model spends
reasoning without emitting a tool call gets a continuation nudge
(bounded by `MaxReasoningOnlyRetries`, default 4) instead of the
prose corrective, the reasoning is preserved in the transcript
ConfigMap, and it is stripped from the wire so past thinking never
re-enters the context budget. Before this, thinking models (North
Mini Code, Qwen-family with reasoning enabled, Mellum2-Thinking)
either death-spiraled in no-tool-call nudges or had to run with
reasoning disabled via
`InferenceService.spec.extraArgs: ["--reasoning-budget", "0"]`,
which degrades models trained to reason before acting.

## What v0.5 changes

The current `Agent` CRD shape doesn't carry an explicit tool
protocol field. That makes the Gemma 3 finding above a footgun: a
user can apply an Agent CR pointing at a Gemma 3 InferenceService
and watch every AgenticTask fail as `ModelMisunderstood` without
the operator catching the misconfiguration ahead of time.

The v0.5 plan ([#589](https://github.com/defilantech/LLMKube/issues/589))
adds:

- `Agent.spec.toolProtocol`: an enum (`oai-function-calling`,
  `google-tool-code-blocks`, `anthropic-xml`, `text-marker`) that
  declares which protocol shape the executor should expect.
- Adapters in `pkg/foreman/agent/oai/` that translate non-OAI
  protocols into the loop's internal `tool_calls` shape.
- A pre-flight validation on `Agent` reconcile that probes the
  referenced InferenceService and flags a misconfigured
  `toolProtocol` before any AgenticTask binds to it.

Until that lands, the practical advice is: stick to models in the
"tested ✓" rows above for v0.4. The Qwen and Mistral families
broadly work in llama.cpp's OAI tool-calls implementation; the
Gemma and (currently) Mistral-Small-3.2 paths don't.

## Contributing entries

If you run Foreman against a model not in this table, please file
an issue or PR with:

- Model + quantization + host hardware
- Role you tested it in
- Whether the loop reached `submit_result` (tool protocol ✓ / ✗)
- A subjective confabulation rate if it did
- Any reproducing notes for the failure modes you saw

The table grows the same way LLMKube's hardware matrix does:
people running real workloads on real hardware reporting what
they actually saw.

## Context strategy: window vs session

Foreman builds each turn's request from the running transcript using one of
two strategies, set per Agent via `spec.contextStrategy`.

**`window` (default).** Observation masking: tool results older than
`observationWindowTurns` are masked to a header, bounding the payload. Correct
for small-context models. Because masking rewrites the older part of the
prompt every turn, it defeats prompt caching on runtimes that support it.

**`session`.** A stable, append-only prefix. Nothing is masked, so a caching
runtime (for example llama.cpp's prompt cache) reuses the prefix and only
prefills the new tokens each turn. When the payload approaches
`contextWindowTokens`, Foreman compacts by dropping the oldest middle turns,
always keeping the system prompt, the original task, and the most recent turn.

Use `session` for large-context models on caching runtimes. Two settings
matter:

- Set `stuckLoopDetection.contextHardCap` at or above the server's context
  size (`n_ctx`) and `contextSoftCap` proportionally, so a healthy deep
  session is not aborted before it reaches the ceiling.
- Tradeoff: `session` trades an occasional cold re-prefill (one per compaction
  event, rare when the ceiling is high) for cheap, cache-hit steady-state
  turns. `window` makes the opposite trade.
