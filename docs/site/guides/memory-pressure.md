---
title: Memory-pressure protection
description: How the LLMKube Metal agent keeps a Mac from running out of memory. It checks each model against a memory budget before it starts, and a watchdog reports pressure on every InferenceService and evicts only when LLMKube is the cause.
---

# Memory-pressure protection

A Mac serving models has no Kubernetes memory accounting: llama-server runs
as a native process, outside any pod limit. Without protection, an oversized
model or a runaway warmup can swap-thrash the whole machine and take
unrelated programs down with it. The Metal agent guards against that in two
places:

1. **Before a start**, a pre-flight check charges each model's weights, KV
   cache and overhead against a memory budget and refuses a model that does
   not fit.
2. **While models run**, a watchdog samples host memory, reports a
   `MemoryPressure` condition on every InferenceService it manages, and,
   when you opt in, evicts the lowest-priority one before the kernel does.

The recording below shows three llama.cpp services on a kind cluster, one
with `evictionProtection: true`. The watchdog reaches Critical, reports the
condition on every service, and holds fire.

<AsciinemaCast
  src="/casts/memory-pressure.cast"
  title="Real-time recording on a kind cluster, idle waits compressed."
/>

## Before a start: the memory budget

The agent estimates a model's memory cost (weights, KV cache and overhead)
before it starts `llama-server`. A model that does not fit in what the
running models have left is refused: the InferenceService gets
`status.schedulingStatus: InsufficientMemory` and one Warning Event, and the
agent re-checks it on a backoff of up to a minute, so it starts once memory
frees up.

The budget is a fraction of the Mac's memory: 67% on a Mac with 36 GB or
less, and 75% above that, changed with the agent flag `--memory-fraction`. When
the agent cannot size a model at all, it fails closed with
`MemoryCheckFailed` instead of starting it blind, because Metal wires model
memory so the kernel cannot page it out. `--memory-check-mode warn` restores
the old log-and-start behavior.

How the KV cache is sized, the budget table, and the exact refusal rules are
in [Memory budgets](/docs/guides/macos-metal#memory-budgets) in the macOS
guide.

## While models run: the watchdog

Every `--memory-watchdog-interval` (10 seconds by default; `0` turns it off)
the watchdog samples host memory and each managed process's resident set
size (RSS):

- **Pressure level.** When available memory falls below
  `--memory-pressure-warning` (default `0.20` of total) the level is Warning;
  below `--memory-pressure-critical` (default `0.10`) it is Critical. The
  level is the `llmkube_metal_agent_memory_pressure_level` gauge.
- **Visibility on the workload.** Every managed InferenceService gets a
  `MemoryPressure` status condition, including services that opted out of
  eviction, and an Event on each change of level, so `kubectl describe isvc`
  shows it without Prometheus.
- **Friendly-fire guard.** Even at Critical, the watchdog does not evict
  unless LLMKube's processes hold more than 50% of the system's RSS. If
  something else is eating the memory, stopping a model would not fix it.
  The refusal is counted in
  `llmkube_metal_agent_evictions_skipped_total{reason="below_guard"}`, so
  you can alert on "the Mac is in distress, but not because of us".

### Which service is evicted

Eviction is off by default. With `--eviction-enabled`, at Critical and past
the guard, the watchdog picks a target by these rules, in order:

1. **Safety floor.** With one managed process or none, nothing is evicted:
   a single-tenant Mac should not be emptied by its own watchdog.
2. **Only llama-server.** oMLX and Ollama run as shared daemons that the
   agent does not own, so stopping them this way frees no per-model memory.
3. **Opt-outs.** Services with `spec.evictionProtection: true` are skipped.
4. **Lowest `spec.priority`.** The same scale used for GPU scheduling, from
   lowest to highest: `batch`, `low`, `normal`, `high`, `critical`.
5. **Ties.** The largest RSS, then the oldest start.

An evicted service stays stopped until pressure returns to Normal, so the
controller cannot start it again into the same pressure. Once pressure is
Normal and available memory has room for it, the agent starts it again.

## Turn on eviction

These are flags on the agent on the Mac, not Helm values. Give them to
`llmkube metal join` with `--agent-arg`:

```bash
llmkube metal join llmkube://... \
  --agent-arg eviction-enabled=true \
  --agent-arg memory-pressure-critical=0.10 \
  --agent-arg memory-pressure-warning=0.20
```

On a Mac that is already joined, add them to the agent's LaunchAgent plist
and reload it, as in
[Changing the agent's flags](/docs/guides/macos-metal#changing-the-agents-flags).
A later `join` keeps them.

Mark a service that must never be evicted:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: chat-prod
spec:
  modelRef: qwen3-8b
  priority: critical
  evictionProtection: true
```

Adding `evictionProtection` or changing `priority` on a running service
takes effect without restarting its engine: the agent picks the edit up on
its next poll of the InferenceService (every 5 seconds), and the watchdog
ticks after that use it.

## Metrics worth alerting on

The agent serves these on `127.0.0.1:9090/metrics` on the Mac. The
[metrics reference](/docs/reference/metrics) lists the rest.

| Metric | Type | Meaning |
| --- | --- | --- |
| `llmkube_metal_agent_memory_pressure_level` | gauge | 0 Normal, 1 Warning, 2 Critical. Page on Critical: host memory is under stress. |
| `llmkube_metal_agent_evictions_total` | counter | Services stopped by the watchdog. A non-zero rate means it is pruning under pressure. |
| `llmkube_metal_agent_evictions_skipped_total{reason}` | counter | Why an eviction did not happen: `disabled`, `below_guard`, `empty`, `floor`, `all_protected` or `runtime_ineligible`. |
| `llmkube_metal_agent_process_rss_bytes` | gauge | Resident set size per managed process, for capacity planning and spotting the model that is growing. |
