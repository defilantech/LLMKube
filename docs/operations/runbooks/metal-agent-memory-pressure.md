# Metal-agent reports memory pressure (Apple Silicon)

The LLMKube metal-agent watchdog is reporting Warning or Critical memory pressure on a managed host (Apple Silicon, Mac Mini / Mac Studio / MacBook Pro running the metal-agent as a launchd process). Under sustained pressure, the watchdog may evict the lowest-priority managed inference process to prevent the host from swapping or OOM-killing.

## Trigger

One or more of:

- Kubernetes event on a managed `InferenceService` with reason `MemoryPressureLevelChanged`, type `Warning`, message containing `transitioned from normal to warning` or `transitioned from normal to critical`.

  ```bash
  kubectl get events -A --field-selector reason=MemoryPressureLevelChanged
  ```

- Status condition on the `InferenceService`: `MemoryPressure=True` with reason `Warning`, `Critical`, or `Evicted`.

  ```bash
  kubectl get inferenceservice <name> -o jsonpath='{.status.conditions[?(@.type=="MemoryPressure")]}{"\n"}'
  ```

- Metric: `llmkube_metal_agent_evictions_skipped_total` increments (visible in the metal-agent's `/metrics` endpoint). Its companions are `llmkube_metal_agent_evictions_total` and the `llmkube_metal_agent_memory_pressure_level` gauge (0 Normal, 1 Warning, 2 Critical).
- Operator notices: free memory on the host drops, or applications connected to the inference endpoint report sudden hangs or 5xx responses.

## Diagnose

1. **Determine current pressure level and which process the watchdog is tracking.**

   Check the most recent event:

   ```bash
   kubectl get events -A --field-selector reason=MemoryPressureLevelChanged \
     --sort-by=.lastTimestamp | tail -5
   ```

   For the full picture, read the agent's own log. At Warning or above it
   logs `memory pressure detected` with `level`, `available`, `total`,
   `wired`, and `totalRSS` fields:

   ```bash
   grep 'memory pressure detected' ~/Library/Logs/llmkube/metal-agent.log | tail -5
   ```

   The level is computed from **available** memory as a fraction of total,
   not from RSS. Lower is worse:

   | Level | Condition | Default |
   |---|---|---|
   | Normal | `available / total` at or above the warning threshold | `>= 0.20` |
   | Warning | below warning, at or above critical | `0.10` to `0.20` |
   | Critical | below the critical threshold | `< 0.10` |

   Both thresholds are agent flags: `--memory-pressure-warning` (default
   `0.20`) and `--memory-pressure-critical` (default `0.10`). `totalRSS`
   appears in the messages because it feeds the friendly-fire guard in step
   4, not because it sets the level.

2. **List managed processes and their priorities.**

   ```bash
   kubectl get inferenceservices -A \
     -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,PRIORITY:.spec.priority,PROTECTED:.spec.evictionProtection,PHASE:.status.phase'
   ```

   Lower-priority + non-protected services are eviction candidates. The metal-agent's watchdog will pick the lowest-priority unprotected one when it decides to evict.

3. **Check whether eviction is enabled.**

   The metal-agent only evicts when started with `--eviction-enabled`. If it is not, you will see `EvictionSkipped` events with `skip-reason=disabled`:

   ```bash
   kubectl get events -A --field-selector reason=EvictionSkipped \
     --sort-by=.lastTimestamp | tail -10
   ```

4. **Verify the friendly-fire guard is not the blocker.** When `totalRSS` from managed processes is < 50% of system total RSS, the watchdog refuses to evict (the pressure is from somewhere else, not us). You will see `EvictionSkipped` with `skip-reason=below_guard`. This is the correct behavior; do not disable the guard.

5. **Read the skip reason.** `disabled` and `below_guard` are not the only ones. When the watchdog is Critical, eviction is enabled, and the guard passes, it can still find no eligible target:

   | `skip-reason` | Meaning | What to do |
   |---|---|---|
   | `disabled` | `--eviction-enabled` is off | expected unless you opted in |
   | `below_guard` | managed processes hold under 50% of system RSS | correct refusal, look elsewhere for the hog |
   | `empty` | no managed processes at all | nothing for the agent to evict |
   | `floor` | refused to evict the last managed process | single-tenant host by design |
   | `all_protected` | every candidate has `evictionProtection: true` | review your protection settings |
   | `runtime_ineligible` | every managed process is on a shared daemon (oMLX / Ollama) the agent does not own at the OS level | killing them here would not free memory |

## Mitigate (immediate)

Pick the path that matches the level.

### Warning level

The watchdog will not evict at Warning. The condition is informational. If the host is meaningfully degrading, manually choose a path:

- **Reduce a service's footprint.** Edit an InferenceService with a smaller `contextSize` (the agent passes it to llama-server as `--ctx-size` and to vllm-swift as `--max-model-len`), lower `parallelSlots`, or lower `cacheTypeK/V` precision (e.g., `q4_0` instead of `f16`). The metal-agent will respawn the process with the new settings.
- **Scale a non-essential service down to 0.**

  ```bash
  kubectl patch inferenceservice <low-priority-name> --type=merge \
    -p '{"spec":{"replicas":0}}'
  ```

- **Set `evictionProtection: true` on services that must not be evicted** even if the watchdog escalates to Critical. Use sparingly; protected services do NOT get protection from being the cause of pressure.

### Critical level (and eviction enabled, above 50% RSS guard)

The watchdog will evict the lowest-priority unprotected process. This is the intended behavior. Verify the eviction was the right call:

```bash
kubectl get events -A --field-selector reason=Evicted \
  --sort-by=.lastTimestamp | tail -5
```

If the eviction was correct, no manual mitigation is needed; the freed memory should drop the level back to Warning or Normal within seconds. If the wrong service was evicted (e.g., the production-critical one), set `priority: critical` or `evictionProtection: true` on it for next time.

### Critical level with eviction disabled

The watchdog cannot act. You will see `EvictionSkipped` with `skip-reason=disabled` repeating. Either:
- Manually scale down a low-priority service (see Warning section above).
- Restart the metal-agent with `--eviction-enabled` if you accept automatic eviction policy. Running engines are adopted by the restarted agent, not reloaded.

## Resolve (structural)

If memory pressure is recurring on this host:

1. **Right-size workloads.** Check the actual RSS of each managed process:

   ```bash
   ps -o pid,rss,command | grep -E 'llama-server|vllm|ollama' | awk '{print $1, $2/1024/1024" GB", $3}'
   ```

   Compare each to the model's expected memory footprint. If a process is dramatically over its expected footprint, that is a separate bug in the runtime; file a focused issue.

2. **Lower `--memory-fraction` on the metal-agent.** The flag defaults to `0`, which means auto-detect, and auto is tiered by host RAM: `0.67` at or below 36 GiB, `0.75` above it. A 128 GB Studio therefore budgets ~96 GB, not ~86 GB. If you are trying to leave more headroom for non-LLMKube workloads, set it explicitly to `0.5`.

3. **Add a memory budget to specific models.** The budget lives on the
   **Model**, under `spec.hardware`, not on the InferenceService:

   ```yaml
   apiVersion: inference.llmkube.dev/v1alpha1
   kind: Model
   spec:
     hardware:
       memoryBudget: "16Gi"     # absolute
       # or, instead:
       # memoryFraction: 0.4    # fraction of this host's total RAM
   ```

   The precedence chain is `spec.hardware.memoryBudget`, then
   `spec.hardware.memoryFraction`, then the agent's `--memory-fraction`.
   A Model budget can only lower the agent's limit for that model: a value
   above the agent's budget is clamped to it with a `MemoryBudgetClamped`
   Warning event, unless the agent runs with
   `--allow-model-memory-budget-override`. The metal-agent enforces the
   resolved budget at spawn time and refuses to start a process whose
   estimate exceeds it, or whose estimate does not fit next to the services
   already running on the Mac (the agent's budget is shared), which heads off
   the failure mode upstream of the watchdog. If the pre-flight estimate
   cannot be computed at all, `--memory-check-mode` decides what happens:
   `enforce` (the default) fails closed, `warn` starts the process anyway.

4. **Move heavy models to a host with more memory.** If the same M2 Pro keeps hitting Critical with a 30B model, that is a sizing problem, not a runbook problem.

## Verify

1. **No new `MemoryPressureLevelChanged` events going up** in the last 5 minutes.

   ```bash
   kubectl get events -A --field-selector reason=MemoryPressureLevelChanged \
     --sort-by=.lastTimestamp \
     --output=custom-columns='TIME:.lastTimestamp,REASON:.reason,MSG:.message' | tail -5
   ```

   The recovery itself shows up as one type `Normal` event per managed
   service, with a message containing `to normal`, and the agent logs
   `memory pressure returned to normal`. The agent reports that only after
   three consecutive Normal watchdog samples (30 seconds at the default
   `--memory-watchdog-interval` of 10s), so a level hovering around the
   Warning threshold does not flap; Warning and Critical are reported at
   once. `llmkube_metal_agent_memory_pressure_level` follows every sample,
   so it can read `0` up to that long before the event.

2. **The managed services that should be running, are running.**

   ```bash
   kubectl get inferenceservice -A
   ```

   An eviction does **not** change the InferenceService spec. The agent kills
   the local process and sets the `MemoryPressure` condition with reason
   `Evicted`; `spec.replicas` is left exactly as you set it, so while
   pressure is above Normal a service showing `replicas: 1` with no running
   process is the expected shape after an eviction, not a stuck reconcile.
   The agent keeps that key blocked in-memory so a controller event cannot
   respawn it while pressure stays high.

   Once pressure is reported back at Normal, the agent restarts an evicted
   service only if there is room for it. At eviction it records the
   process's resident memory (RSS). A service is restarted when the host's
   available memory, minus that RSS, stays at or above the Warning
   threshold (`--memory-pressure-warning`, default `0.20` of total memory).
   Services restarted together count their RSS together, and a restart that
   is queued but not yet running keeps its RSS counted. A service that does
   not fit stays blocked, keeps `MemoryPressure=True` with reason `Evicted`,
   and is checked again on every later watchdog sample while pressure stays
   Normal; no other change to the InferenceService starts it in the
   meantime. Without this, a service evicted for pressure that has not gone
   away would come straight back and be evicted again: the pre-start
   admission check compares the model's estimate, plus what the services
   still running have committed, with a fraction of total memory and does
   not look at available memory.

   A service evicted again within 10 minutes of being restarted this way
   waits before its next restart: 1 minute, then twice as long on each
   further quick re-eviction, up to 30 minutes. A service that stays up
   for 10 minutes starts from no wait again.

   Services that fit are queued on the agent's restart queue, the same one
   health restarts use, and started one after another. Each start still goes
   through every pre-start check, and counts in
   `llmkube_metal_agent_process_restarts_total`. A service deleted, scaled to
   zero or suspended in the meantime is not started. If pressure has left
   Normal by the time a queued service's turn comes, it is blocked again
   instead of started. A queued start that fails for another reason is
   retried on the restart queue's backoff (10s, doubling, up to 5 minutes),
   with a `RestartFailed` event each time. Its `MemoryPressure` condition goes
   back to `False` with reason `Normal` once its engine is running.

   A service still down after pressure has been Normal for a while either
   still has no room or is still waiting out its backoff (the agent logs
   `restarting evicted services that fit the headroom` with `stillBlocked`
   and `available`, at info on the return to Normal and at debug on each
   later check), or its start keeps failing: check its
   `status.schedulingStatus`, its `RestartFailed` events, and the agent log
   for `restart failed; retrying`.

3. **`llmkube_metal_agent_evictions_skipped_total` is not climbing** unless eviction is intentionally disabled. The agent serves `/metrics` on `127.0.0.1:9090` only, so run this on the Mac itself (or over `ssh <metal-agent-host>`):

   ```bash
   curl -s http://127.0.0.1:9090/metrics \
     | grep -E 'llmkube_metal_agent_(evictions|memory_pressure_level)'
   ```

   `llmkube_metal_agent_memory_pressure_level` should read `0`.

## Related

- Issue: [#390](https://github.com/defilantech/LLMKube/issues/390) (the K8s events that surface this signal)
- Fix: [PR #411](https://github.com/defilantech/LLMKube/pull/411) (events emission)
- Earlier work: PRs #382 + #386 (the watchdog itself, shipped in 0.7.6)
- Field reference: `spec.hardware.memoryBudget` and `spec.hardware.memoryFraction` on `Model`, for enforcement upstream of the watchdog
- Agent setup and log locations: [macOS Metal agent guide](../../site/guides/macos-metal.md)
