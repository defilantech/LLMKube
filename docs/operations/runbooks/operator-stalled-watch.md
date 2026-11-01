# Operator looks healthy but nothing reconciles (stalled watch)

The LLMKube controller-manager pod is `Running`, its liveness and readiness
probes pass, and its admission webhooks answer, but the controllers have
stopped reconciling: a GitOps apply lands, the CR shows the new spec, and the
workload keeps running the old one. `metadata.generation` runs ahead of
`status.observedGeneration` on operator-owned objects with no error anywhere
in the operator log.

This is the #2069 failure class: the operator's informers have silently
stopped receiving events. The operator detects this itself with a watch
canary and restarts on its own; this runbook covers the alert, the
manual detection, and what to do when the automatic recovery has not cleared
it.

## Trigger

One or more of:

- Alert: `LLMKubeControllerWatchStalled` (critical) or
  `LLMKubeControllerWatchCanaryDown` (warning), shipped by the chart's
  PrometheusRule when the watch canary is enabled.
- Alert: `LLMKubeUnreconciledGeneration` (warning): one kind's spec changes
  are not being reconciled while the canary may still be green. See
  [the section for it](#alert-llmkubeunreconciledgeneration-one-kind-stopped-reconciling).
- Kubelet restart events on the controller-manager pod. The probe response
  withholds the reason by design (the healthz endpoint is public), so the
  kubelet event reads:

  ```
  Liveness probe failed: [-]watch-canary failed: reason withheld
  ```

  The real reason is in the operator's own log, both as the healthz handler's
  failure line and as the canary's own transition log:

  ```
  watch canary failed the liveness check; the kubelet restarts this pod once
    the probe starts failing  {"error": "watch canary write not observed for
    5m0s (fail-after 5m0s): the manager's watch pipeline is stalled"}
  ```

- Operator notices: an InferenceService whose `status.observedGeneration`
  lags `metadata.generation` minutes after a spec change, while the operator
  pod is green:

  ```bash
  kubectl --context <ctx> -n <ns> get isvc <name> \
    -o jsonpath='{.metadata.generation}{" / "}{.status.observedGeneration}{"\n"}'
  ```

  `2 / 1` means the operator has not seen generation 2.

## Background: what the canary does

Every `--watch-canary-interval` (default 60s) the operator renews the Lease
`llmkube-controller-manager-watch-canary` in its own namespace and requires
its own informer to deliver the write back. When a write goes unobserved for
`--watch-canary-fail-after` (default 5m), the `watch-canary` liveness check
fails and the kubelet restarts the pod. The gauge
`llmkube_watch_canary_last_observed_timestamp_seconds` and the counter
`llmkube_watch_canary_writes_total{result=...}` expose the same signal to
Prometheus.

## Diagnose

1. **Confirm the stall, do not trust the probes.**

   ```bash
   kubectl --context <ctx> -n llmkube-system get pod \
     -l control-plane=controller-manager
   kubectl --context <ctx> -n llmkube-system get isvc -A \
     -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,GEN:.metadata.generation,OBSERVED:.status.observedGeneration'
   ```

   Any row where OBSERVED lags GEN on a recently changed object is the stall
   (or a slower problem: a stuck reconcile, an outage in a dependency).

2. **Read the canary metrics.** If you scrape the operator:

   ```
   llmkube_watch_canary_up
   llmkube_watch_canary_last_observed_timestamp_seconds
   llmkube_watch_canary_writes_total{result="error"}
   ```

   - `up == 1` with a growing `time() - last_observed`: the watch pipeline is
     stalled; the pod should have restarted itself. If it did not, check the
     probe configuration (the `watch-canary` check must be wired into
     `/healthz`, which the chart does by default).
   - `up == 0` and `writes_total{result="error"}` climbing: the operator
     cannot write its lease at all. That is RBAC or namespace
     misconfiguration, not a stalled watch. See step 4.
   - `up == 1` and a fresh timestamp: the watch pipeline is fine; the failure
     is elsewhere (look at the controller logs and events instead).

3. **Check the operator log around the restart, if the kubelet restarted it.**

   ```bash
   kubectl --context <ctx> -n llmkube-system logs deploy/llmkube-controller-manager \
     --previous | grep -i "watch canary"
   ```

   A `watch canary failed the liveness check` line followed by a restart that
   cleared the lag is the system working as designed; note it and move to
   Verify.

4. **If the canary keeps failing across restarts**, the operator cannot
   renew its lease:

   ```bash
   kubectl --context <ctx> auth can-i update leases.coordination.k8s.io \
     -n llmkube-system --as=system:serviceaccount:llmkube-system:controller-manager
   kubectl --context <ctx> -n llmkube-system logs deploy/llmkube-controller-manager \
     | grep -m3 "watch canary write failed"
   ```

   A `forbidden` error means the ServiceAccount's Role is missing the lease
   rules. Installs managed by `rbac.create=true` ship them; externally
   managed RBAC must add `get/list/watch/create/update` on
   `coordination.k8s.io/leases` in the operator namespace (see the chart's
   `leader-election-role`). Until the rule lands, disable the canary rather
   than restart-looping (step Mitigate).

## Mitigate

Restart the operator; this is exactly what the canary does automatically, and
it clears a stalled watch:

```bash
kubectl --context <ctx> -n llmkube-system rollout restart deploy/llmkube-controller-manager
```

If the canary is restart-looping on missing RBAC and you need the operator up
now, disable the canary (it resumes on the next rollout):

```yaml
controllerManager:
  watchCanary:
    interval: "0s"
```

or `--watch-canary-interval=0` on a bare binary. Fix the RBAC, then re-enable.

## Resolve

- Chart installs: keep `controllerManager.watchCanary.interval` enabled
  (default) and the PrometheusRule `rules.watchCanary` enabled so the alert
  pages when restarts do not clear the stall.
- Externally managed RBAC: grant the lease verbs named in Diagnose step 4.
- If stalls recur, capture `kubectl get --raw /debug/pprof/goroutine?debug=2`
  from the operator before it restarts and open an issue referencing #2069
  and upstream kubernetes-sigs/controller-runtime#3428: the trigger is
  upstream and not yet diagnosed.

## Verify

After the restart, make a harmless change to a test InferenceService (or
watch an object the GitOps tool just applied) and confirm the generation lag
closes within a minute:

```bash
kubectl --context <ctx> -n <ns> get isvc <name> \
  -o jsonpath='{.metadata.generation}{" / "}{.status.observedGeneration}{"\n"}'
```

The two numbers must converge. The canary's own health is visible in
`/healthz?verbose`:

```bash
kubectl --context <ctx> -n llmkube-system port-forward deploy/llmkube-controller-manager 8081:8081
curl -s localhost:8081/healthz?verbose
```

`[+]watch-canary ok` must appear in the output.

## Alert: LLMKubeUnreconciledGeneration (one kind stopped reconciling)

The watch canary proves the operator's Lease informer delivers events.
Informers are per resource type, so the InferenceService or Model informer
can stop alone while the canary stays green
(kubernetes-sigs/controller-runtime#3428 shows that shape). For that case the
leader lists InferenceServices and Models through the API server every
`--unreconciled-generation-interval` (default 60s) and exports, per kind:

```
llmkube_unreconciled_generation_objects{kind="InferenceService|Model"}
llmkube_unreconciled_generation_oldest_seconds{kind="InferenceService|Model"}
```

An object lags when its `metadata.generation` is ahead of the highest
`observedGeneration` across its `status.conditions` (neither kind has a
top-level `status.observedGeneration`). `objects` counts the ones lagging
longer than `--unreconciled-generation-threshold` (chart:
`controllerManager.unreconciledGeneration.thresholdSeconds`, default 1200s,
above the 15m `--reconcile-timeout`); `oldest_seconds` is the age of the
oldest lag of that kind. `LLMKubeUnreconciledGeneration` (warning) fires when
`oldest_seconds` passes the same threshold.

This alert does **not** restart the operator. A stuck object is not always a
dead watch: a reconcile error the controller retries forever looks the same,
and restarting on it would restart-loop the pod.

What to check:

1. **Which kind and which object.** The alert's `kind` label names the kind.
   The operator logs the object once it crosses the threshold:

   ```bash
   kubectl --context <ctx> -n llmkube-system logs deploy/llmkube-controller-manager \
     | grep "spec change unreconciled past the threshold"
   ```

   Or list generation against the conditions' observedGeneration:

   ```bash
   kubectl --context <ctx> get isvc -A \
     -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,GEN:.metadata.generation,OBSERVED:.status.conditions[*].observedGeneration'
   kubectl --context <ctx> get models -A \
     -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,GEN:.metadata.generation,OBSERVED:.status.conditions[*].observedGeneration'
   ```

   A row whose GEN is above every OBSERVED value is lagging.

2. **Is that kind's controller running at all?** Look for its reconcile logs
   after the spec change (`"controller": "inferenceservice"` or
   `"controller": "model"`):

   ```bash
   kubectl --context <ctx> -n llmkube-system logs deploy/llmkube-controller-manager \
     --since=30m | grep '"controller":"model"' | tail
   ```

   - No reconcile lines for the lagging object, and other objects of the same
     kind are lagging too: that kind's informer has stopped delivering. Restart
     the operator (see Mitigate); a restart rebuilds every informer.
   - Reconcile lines with an error for the object, repeating: the controller
     sees the change and fails on it. This is not a watch problem. Fix the
     cause the error names; the alert clears on the next successful pass.

3. **Clear it.** The gauge drops as soon as a reconcile stamps the current
   generation on any condition. After a restart the clock starts again, so
   the alert resolves and fires again only if the lag outlasts the threshold
   once more.

Not counted, by design:

- A spec change the admission webhook rejects never bumps
  `metadata.generation`, so there is nothing to lag.
- An object with a `deletionTimestamp` (being finalized) is skipped.
- Kinds other than InferenceService and Model, and built-in resources
  (kube-state-metrics' `KubeDeploymentGenerationMismatch` covers
  Deployments).

To turn the check off, set `controllerManager.unreconciledGeneration.interval`
to `"0s"` (or `--unreconciled-generation-interval=0`); to keep the gauges but
drop the alert, set
`prometheus.prometheusRule.rules.unreconciledGeneration.enabled: false`.

## Related

- Issue #2069 (the stall signature and the AKS / MicroK8s timelines).
- Upstream: kubernetes-sigs/controller-runtime#3428, the open report of the
  same silent-reflector-stall class.
- [`upgrade-rollback.md`](./upgrade-rollback.md) for the restart procedure on
  an install managed by Helm.
