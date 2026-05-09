# Metal-agent reports memory pressure (Apple Silicon)

The LLMKube metal-agent watchdog is reporting Warning or Critical memory pressure on a managed host (Apple Silicon, Mac Mini / Mac Studio / MacBook Pro running the metal-agent as a launchd process). Under sustained pressure, the watchdog may evict the lowest-priority managed inference process to prevent the host from swapping or OOM-killing.

## Trigger

One or more of:

- Kubernetes event on a managed `InferenceService` with reason `MemoryPressureLevelChanged`, type `Warning`, message containing `transitioned from Normal to Warning` or `transitioned from Normal to Critical`.

  ```bash
  kubectl get events -A --field-selector reason=MemoryPressureLevelChanged
  ```

- Status condition on the `InferenceService`: `MemoryPressure=True` with reason `Warning`, `Critical`, or `Evicted`.

  ```bash
  kubectl get inferenceservice <name> -o jsonpath='{.status.conditions[?(@.type=="MemoryPressure")]}{"\n"}'
  ```

- Metric: `evictions_skipped_total` increments (visible in the metal-agent's `/metrics` endpoint).
- Operator notices: free memory on the host drops, or applications connected to the inference endpoint report sudden hangs or 5xx responses.

## Diagnose

1. **Determine current pressure level and which process the watchdog is tracking.**

   The metal-agent emits `[longctx]`-style telemetry lines in its log including memory state. Check the most recent event:

   ```bash
   kubectl get events -A --field-selector reason=MemoryPressureLevelChanged \
     --sort-by=.lastTimestamp | tail -5
   ```

   The event message includes `totalRSS=XX.X GB of YY.Y GB`. Determine the level:
   - **Normal**: `totalRSS / totalMemory < warningThreshold` (default `< 0.20` of available)
   - **Warning**: between thresholds (default `0.20` to `0.10` available)
   - **Critical**: `totalRSS / totalMemory > 1 - criticalThreshold` (default `> 0.90` of total RSS)

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

## Mitigate (immediate)

Pick the path that matches the level.

### Warning level

The watchdog will not evict at Warning. The condition is informational. If the host is meaningfully degrading, manually choose a path:

- **Reduce a service's footprint.** Edit an InferenceService with a smaller `--max-model-len`, lower `parallelSlots`, or lower `cacheTypeK/V` precision (e.g., `q4_0` instead of `f16`). The metal-agent will respawn the process with the new settings.
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
- Restart the metal-agent with `--eviction-enabled` if you accept automatic eviction policy.

## Resolve (structural)

If memory pressure is recurring on this host:

1. **Right-size workloads.** Check the actual RSS of each managed process:

   ```bash
   ps -o pid,rss,command | grep -E 'llama-server|vllm|ollama' | awk '{print $1, $2/1024/1024" GB", $3}'
   ```

   Compare each to the model's expected memory footprint. If a process is dramatically over its expected footprint, that is a separate bug in the runtime; file a focused issue.

2. **Lower `--memory-fraction` on the metal-agent.** Defaults to `0.67` on most hosts (auto-detected). If you are trying to leave more headroom for non-LLMKube workloads, drop it to `0.5`.

3. **Add memory budget to specific InferenceServices.**

   ```yaml
   spec:
     memoryBudget: "16Gi"
   ```

   The metal-agent enforces this at spawn time and refuses to start a process that would exceed it. Prevents the failure mode upstream of the watchdog.

4. **Move heavy models to a host with more memory.** If the same M2 Pro keeps hitting Critical with a 30B model, that is a sizing problem, not a runbook problem.

## Verify

1. **No new `MemoryPressureLevelChanged` events going up** in the last 5 minutes.

   ```bash
   kubectl get events -A --field-selector reason=MemoryPressureLevelChanged \
     --sort-by=.lastTimestamp \
     --output=custom-columns='TIME:.lastTimestamp,REASON:.reason,MSG:.message' | tail -5
   ```

2. **The managed services that should be running, are running.**

   ```bash
   kubectl get inferenceservice -A
   ```

   If a service was evicted, its replicas are 0 and the MemoryPressure condition has reason `Evicted`. The watchdog will allow respawn once the pressure drops back to Normal (the agent emits a level-changed event when it returns to Normal).

3. **`evictions_skipped_total` is not climbing** unless eviction is intentionally disabled:

   ```bash
   curl -s http://<metal-agent-host>:9090/metrics | grep evictions_skipped
   ```

## Related

- Issue: [#390](https://github.com/defilantech/LLMKube/issues/390) (the K8s events that surface this signal)
- Fix: [PR #411](https://github.com/defilantech/LLMKube/pull/411) (events emission)
- Earlier work: PRs #382 + #386 (the watchdog itself, shipped in 0.7.6)
- Companion runbook: `metal-agent-respawn-blocked.md` (when an evicted service refuses to come back even after pressure clears)
- Documentation: `Memory.Budget` field on `InferenceService` for upstream enforcement
