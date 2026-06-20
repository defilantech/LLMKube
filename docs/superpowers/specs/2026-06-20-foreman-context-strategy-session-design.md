# Foreman `contextStrategy`: session mode design

**Issue:** [#756](https://github.com/defilantech/LLMKube/issues/756)
**Status:** Design approved, pending implementation plan.

## Summary

Add a per-Agent `contextStrategy` field that selects how Foreman builds the
wire payload across an agentic loop:

- `window` (default, current behavior): observation masking. Older tool
  results are masked each turn, bounded by `observationWindowTurns`. Correct
  for small-context local models.
- `session`: a stable, append-only prompt prefix with no per-turn masking, so
  a caching runtime (llama.cpp prompt cache) reuses the prefix and only
  prefills the new delta each turn. Compaction (drop oldest middle turns)
  happens only when context approaches `contextWindowTokens`.

## Problem

Foreman's only context strategy today is observation masking, and
`observationWindowTurns` is CRD-capped at 50. Masking rewrites the older
portion of the prompt every turn, which defeats prompt caching on runtimes
that support it.

Measured on a dense 27B coder (AMD Strix, llama.cpp Vulkan, 256k ctx):

- While masking was effectively inactive (turns < 50), the runtime prompt
  cache hit and per-turn prefill was ~31 tokens at 56k context. The run
  climbed stably to ~76k context.
- The moment the run crossed the 50-turn cap, masking resumed, the prefix was
  rewritten, the cache broke, and every turn became a full cold re-prefill of
  the ~75k context (~17 min/turn). The run churned and could not converge.

The point at which a long task most needs sustained context is exactly where
the current cap forces masking back on and collapses caching.

## Approach

Separate function + branch at the single callsite.

`maskTranscriptForWire` (`pkg/foreman/agent/loop.go:881`) is the one place the
transcript is transformed into the wire payload, called once at
`loop.go:744`. Add a sibling `compactTranscriptForWire`; branch on the
strategy at the callsite. The existing masking function is left untouched
(zero regression risk for window-mode agents) and each function is
independently testable.

Rejected alternatives:
- One function with a strategy parameter branching internally: mixes two
  unrelated policies in one body, muddier and riskier to the existing path.
- A strategy interface with two implementations: over-engineered for two
  cases (YAGNI).

## API change

`api/foreman/v1alpha1/agent_types.go`, on `AgentSpec`:

```go
// ContextStrategy selects how the loop builds the wire payload.
// "window" (default) applies observation masking bounded by
// ObservationWindowTurns. "session" keeps a stable append-only prefix
// (cache-friendly) and compacts only at the ContextWindowTokens ceiling.
// +kubebuilder:validation:Enum=window;session
// +kubebuilder:default=window
// +optional
ContextStrategy string `json:"contextStrategy,omitempty"`
```

- `window`: exact current behavior. Default, so existing Agents are byte-for-byte
  unchanged.
- `session`: new behavior (below).
- `observationWindowTurns` applies only to `window`. `contextWindowTokens` is
  the ceiling/budget in both strategies.

Threaded through `LoopConfig` (`pkg/foreman/agent/loop.go`) and the executor
mapping (`pkg/foreman/agent/executor_native.go`, next to the existing
`ObservationWindowTurns` map at ~`:382`). Regenerate CRDs:
`make manifests generate foreman-chart-crds`.

## Core logic: `compactTranscriptForWire(transcript []oai.Message, ctxBudget int) []oai.Message`

1. If `ctxBudget <= 0` OR `approxTokens(transcript) <= ctxBudget`: return the
   transcript **as-is** (append-only -> prompt-cache hits). This is the common
   path and the entire point of the strategy.
2. Over budget: drop the oldest whole **turn-groups** from the *middle*,
   preserving:
   - the **head**: the leading system message(s) and the original user task
     message (the issue). The agent must never lose its instructions or its
     task.
   - the **most recent turn-group** (always kept).
   Drop in turn-group units so a `tool_call_id` is never orphaned. A
   turn-group is an assistant message plus the tool result messages that
   answer its tool calls. OAI validity requires an assistant tool_call and its
   tool results travel together.
3. Degenerate case (head + newest group alone exceeds budget): return that
   minimal set and log a warning. Rare; the ceiling is normally far above one
   turn.

The function is non-mutating: it returns the same message values when under
budget (cache stability) and a new slice when it drops.

### Turn-group identification

Walk the transcript after the pinned head. A new group starts at each
assistant message; the tool messages immediately following it belong to that
group. (User messages mid-run, if any, start their own group.) Index 0..k is
the pinned head (system messages + first user/task message); the remaining
groups are drop candidates oldest-first, excluding the final group.

## Interactions

- Stuck-loop `contextSoftCap`/`contextHardCap` (`progress.go`, default
  90k/140k) stay operator-tunable and keep working as a backstop. **Document**:
  for session mode set `contextHardCap` >= the server `n_ctx` (and
  `contextSoftCap` proportionally) so a healthy deep session is not aborted
  prematurely. This is exactly the manual tuning validated during the Strix
  run.
- Compaction tradeoff to **document**: session mode trades occasional cold
  re-prefills (one per compaction event, rare when the ceiling is high) for
  cheap, cache-hit steady-state turns. window mode trades the opposite.

## Out of scope (tracked as #756 follow-ups)

- Summary-based compaction (an extra model call to summarize dropped turns) ->
  v2. v1 is drop-oldest only.
- The agent streaming-idle cancel (~30s) can cancel-churn a long cold prefill;
  session mode wants it raised. Tracked separately.
- Large-context AMD node prerequisites docs (`amdgpu.lockup_timeout` above the
  2s default; size the runtime prompt cache to hold the session).

## Testing

Mirror `pkg/foreman/agent/loop_masking_test.go` fixtures
(`transcriptFixture`):

- **Cache-stability invariant**: under-budget session input returns a wire
  payload byte-identical to the input (no message content changed). This is
  the core guarantee.
- **Compaction**: over-budget input drops the oldest middle turn-groups; the
  head (system + task) and the most recent group are kept; no orphaned
  `tool_call_id`; message structure stays OAI-valid.
- **Degenerate**: head + newest group over budget returns the minimal set
  without panic.
- **Regression**: `window` strategy output is unchanged (existing masking
  tests continue to pass; add a case asserting the callsite routes `window`
  to `maskTranscriptForWire`).

## Files

- `api/foreman/v1alpha1/agent_types.go` — add `ContextStrategy`.
- `pkg/foreman/agent/loop.go` — `LoopConfig.ContextStrategy`; branch at
  `:744`; new `compactTranscriptForWire`.
- `pkg/foreman/agent/executor_native.go` — map `agent.Spec.ContextStrategy`
  into `LoopConfig`.
- `pkg/foreman/agent/loop_session_test.go` — new test file.
- `config/crd/bases/...agents.yaml` + `charts/foreman/templates/crds/...` —
  regenerated.
- Docs: the two documented notes above (interactions + tradeoff).
