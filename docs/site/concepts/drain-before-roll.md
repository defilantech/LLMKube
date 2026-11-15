---
title: Drain Before Roll
description: Defer pod-template rollouts until all inference replicas are idle, avoiding dropped in-flight generations, and choose whether a GPU service replaces all replicas at once or one pod at a time. Supports llama.cpp, vLLM, TGI, SGLang, and custom runtimes via annotation.
---

# Drain Before Roll

When the controller detects a pod-template change for an `InferenceService`, it normally applies the update immediately. If a replica is mid-generation, that request is dropped. The `waitForIdle` policy gates the rollout behind a per-replica idle check: the controller probes every endpoint, ready and not-ready, and only proceeds when all reachable replicas report idle, or the timeout expires. A replica whose serving container is not running holds nothing, so it neither gets probed nor blocks the rollout.

The idle check is **fail-closed** for a running replica. Any ambiguity (unreachable pod, parse error, unsupported runtime) defers the rollout rather than risking in-flight work. A pod with `PodReady=False` has been removed from Service endpoints so new requests stop arriving, but generations already accepted keep running, so not-ready replicas with a running container are probed directly rather than inferred idle from their readiness. The one state that needs no probe is a container that is not running: `CrashLoopBackOff`, `ContainerCreating`, or a terminated container cannot be holding a request, so a rollout over pods in that state proceeds instead of waiting out the timeout (#2553).

## Enabling

Set `rolloutPolicy.waitForIdle` on the `InferenceService`:

```yaml
spec:
  rolloutPolicy:
    waitForIdle: true
    idleTimeoutSeconds: 300   # default: 300 (5 minutes)
    force: false              # set true to bypass idle check entirely
```

With this enabled, the controller checks each replica before applying a pod-template update. When every replica is idle, the rollout proceeds normally. When one or more are busy, the controller sets a `RolloutDeferred` status condition and re-checks every 5 seconds until all are idle or the timeout expires.

## Supported runtimes

| Runtime | Signal | Idle when |
|---|---|---|
| `llamacpp` | `GET /slots` — JSON array of `{id, is_processing}` | every slot has `is_processing: false` |
| `vllm` | `GET /metrics` — Prometheus gauge `vllm:num_requests_running` | sum across all label sets equals 0 |
| `tgi` | `GET /metrics` — Prometheus gauge `tgi_batch_current_size` | value equals 0 |
| `sglang` | `GET /metrics` — Prometheus gauge `sglang:num_running_reqs` | sum across all label sets equals 0 |
| `generic` | Annotation-driven custom endpoint (see below) | HTTP 2xx response |
| `personaplex` | — | Not supported; defers the rollout (fail-closed) until `idleTimeoutSeconds` expires |

For vLLM and SGLang, the gauge is summed across all label combinations because both runtimes emit one series per model or request class. An absent metric is treated as **busy** (fail-closed), not idle.

> **Note on queued requests:** The idle check reads the *running* gauge only (`vllm:num_requests_running`, `sglang:num_running_reqs`, `tgi_batch_current_size`), not the *waiting/queued* gauge. A request that has been accepted but is still queued during a momentary `running == 0` window could be dropped on restart. This is a deliberate trade-off for v1: checking the queue gauge would mean the drain never completes while any request is waiting, which could indefinitely block rollouts on busy services. The `idleTimeoutSeconds` safety valve ensures the rollout eventually proceeds. If queue-aware draining is needed, it can be layered on as a future `RolloutPolicySpec` field.

## Generic runtime fallback

Custom runtimes can opt in by annotating the `InferenceService` with an HTTP path that returns 2xx when idle:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: my-custom-runtime
  annotations:
    inference.llmkube.dev/idle-endpoint: "/health/idle"
spec:
  runtime: generic
  rolloutPolicy:
    waitForIdle: true
```

The controller appends the annotation value to each replica's base URL. A 2xx response means idle; anything else (non-2xx, network error, timeout) is treated as busy or fail-closed. Without this annotation, the generic runtime is unsupported and the rollout defers (fail-closed) until `idleTimeoutSeconds` expires.

## Multi-replica behavior

The controller does not probe the load-balanced `Service` URL. Instead, it lists the service's `EndpointSlice` objects and probes **each endpoint address individually — both ready and not-ready**. This ensures a busy replica behind a healthy Service doesn't cause the idle check to pass incorrectly, and that a not-ready replica still processing accepted generations is not silently skipped.

The rollout waits until **every** reachable replica reports idle. If even one running replica is busy or unreachable, the entire rollout defers. When no EndpointSlices exist yet (freshly created Deployment), the controller falls back to a single Service URL probe for backward compatibility. When EndpointSlices exist but contain no reachable addresses at all, the controller reads the pods: if none of them is running its serving container the rollout proceeds, and if one is running it treats the state as **undetermined** and defers (fail-closed) rather than silently proceeding over possibly in-flight work.

## Pod replacement strategy

`waitForIdle` decides *when* a rollout starts. `rolloutPolicy.strategy` decides *how* the pods are replaced, and the two are independent: setting `strategy` alone does not turn on the idle gate. The strategy is not part of the hashed pod template, so changing only `strategy` is not a pod-template change and is not gated on idleness once the live Deployment carries the operator's template-hash annotation. Two inputs still travel with a held update: a Deployment created before that annotation existed (a missing hash reads as changed), and a service already holding a deferred template change.

| Workload | Default | With `strategy: OneAtATime` |
|---|---|---|
| GPU, device plugin | `Recreate` | `RollingUpdate`, `maxSurge: 0`, `maxUnavailable: 1` |
| GPU, DRA resource claims | `Recreate` | `Recreate` (the field is ignored, with a `RolloutStrategyIgnored` warning) |
| Metal (`accelerator: metal`) | no Deployment | unchanged, with a `RolloutStrategyIgnored` warning |
| Non-GPU (device plugin count 0, no claims) | Kubernetes default (25%/25%) | unchanged, with a `RolloutStrategyIgnored` warning |
| `spec.multiNode` | no Deployment | unchanged, with a `RolloutStrategyIgnored` warning |

`Recreate` is the GPU default because a replacement pod cannot schedule while an old pod holds its GPU: a rolling update that needs a spare GPU deadlocks (#192). The cost is that every pod-template change stops every replica before starting any.

`OneAtATime` gets the same guarantee a different way. With `maxSurge: 0` the Deployment never runs more pods than `spec.replicas`, so it scales an old pod down before creating its replacement and never needs a spare GPU. At `replicas >= 2` the other replicas keep serving at reduced capacity while each replacement loads; at `replicas: 1` it is the same one-pod replacement `Recreate` performs, since there is no second replica to keep serving.

```yaml
spec:
  replicas: 2
  rolloutPolicy:
    strategy: OneAtATime   # default: Recreate
```

Before opting in, check four things:

- **Model cache access mode.** A `ReadWriteOnce` cache (the shared cache's default unless the operator is configured for `ReadWriteMany`) cannot be attached by the replacement while the outgoing pod holds it. The replacement stays Pending until the volume is released and the rollout stalls at reduced capacity. Use a `ReadWriteMany` cache with more than one replica. The operator's `perService` cache mode does not help here: its claim is also `ReadWriteOnce`, bound on the node the first pod scheduled to, so a replacement placed on another node stalls the same way.
- **Startup time vs `progressDeadlineSeconds`.** It stays at the Kubernetes default of 600s. A single step longer than that reports `ProgressDeadlineExceeded` even though the Deployment controller keeps making progress; `kubectl rollout status` surfaces it as a failure.
- **Mixed versions during the window.** A rollout that changes the model (a `ModelPool` move, a changed `modelRef`) serves the old and the new version side by side until every replica is replaced. For a template change the old version cannot serve, keep `Recreate`.
- **Replica count.** `Recreate` on a single replica and `OneAtATime` on a single replica both replace the only pod, so the strategy only avoids downtime from `replicas: 2` up.

While a replacement is Pending on `Insufficient nvidia.com/gpu` and at least one replica still serves, the service reports `Progressing`, not `WaitingForGPU`: a serving replica means it is mid-rollout, not queued for GPU. That Pending state is normal during a one-at-a-time rollout. The outgoing pod holds its GPUs until it finishes draining, so the replacement cannot schedule until then; it is not the #192 deadlock, which only appears when the Deployment asks for a pod beyond `spec.replicas`. At `replicas: 1`, or in any window where no replica is Ready, no serving replica masks the probe, so the service can report `WaitingForGPU` until the outgoing pod releases its GPUs.

## Fail-closed semantics

The `RolloutDeferred` condition communicates why a rollout is held:

| Condition state | Reason | Meaning |
|---|---|---|
| `True` | `PodsBusy` | One or more replicas are actively processing requests |
| `True` | `PodsCrashLooping` | Some old-generation pods are running their serving container while others are not; the rollout defers to protect in-flight work on the running pods |
| `True` | `IdleCheckFailed` | The controller could not reach or parse the idle signal (network error, non-200 response) for a replica whose serving container is running |
| `True` | `IdleCheckUnsupported` | The selected runtime does not implement idle detection, or `generic` without `idle-endpoint` annotation, and a replica's serving container is running |
| `False` | `IdleTimeoutExceeded` | The `idleTimeoutSeconds` budget expired; rollout proceeded despite busy pods |

The rollout eventually proceeds in one of four ways: when idle is confirmed, when no old pod's serving container is running, when the `idleTimeoutSeconds` budget expires, or when `force: true` bypasses the check. The timeout is a safety valve — it prevents an endlessly stuck rollout if a runtime hangs in a permanently busy state.

## Observability

Check rollout status with `kubectl`:

```bash
# See RolloutDeferred condition and reason
kubectl get inferenceservice <name> -o=jsonpath='{.status.conditions[?(@.type=="RolloutDeferred")]}' | jq

# Full status
kubectl describe inferenceservice <name>
```

A deferred rollout looks like:

```yaml
conditions:
  - type: RolloutDeferred
    status: "True"
    reason: PodsBusy
    message: "2 of 3 replicas busy, waiting for idle (60s elapsed)"
```

When the timeout expires and the rollout forces through:

```yaml
conditions:
  - type: RolloutDeferred
    status: "False"
    reason: IdleTimeoutExceeded
    message: "idle timeout exceeded after 300s, proceeding with rollout"
```

## Next steps

- Read the [CRD reference](/docs/concepts/crds) for every field on `RolloutPolicySpec`.
- See the [Architecture](/docs/concepts/architecture) page for how the controller manages rollouts.
