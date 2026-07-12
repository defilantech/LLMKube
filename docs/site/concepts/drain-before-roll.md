---
title: Drain Before Roll
description: Defer pod-template rollouts until all inference replicas are idle, avoiding dropped in-flight generations. Supports llama.cpp, vLLM, TGI, SGLang, and custom runtimes via annotation.
---

# Drain Before Roll

When the controller detects a pod-template change for an `InferenceService`, it normally applies the update immediately. If a replica is mid-generation, that request is dropped. The `waitForIdle` policy gates the rollout behind a per-replica idle check: the controller probes every ready endpoint, and only proceeds when all replicas report idle — or the timeout expires.

The idle check is **fail-closed**. Any ambiguity (unreachable pod, parse error, unsupported runtime) defers the rollout rather than risking in-flight work.

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
| `sglang` | `GET /metrics` — Prometheus gauge `sglang:num_requests_running` | sum across all label sets equals 0 |
| `generic` | Annotation-driven custom endpoint (see below) | HTTP 2xx response |
| `personaplex` | — | Not supported; proceeds immediately with `IdleCheckUnsupported` |

For vLLM and SGLang, the gauge is summed across all label combinations because both runtimes emit one series per model or request class. An absent metric is treated as **busy** (fail-closed), not idle.

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

The controller appends the annotation value to each replica's base URL. A 2xx response means idle; anything else (non-2xx, network error, timeout) is treated as busy or fail-closed. Without this annotation, the generic runtime is unsupported and the rollout proceeds immediately with `ReasonIdleCheckUnsupported`.

## Multi-replica behavior

The controller does not probe the load-balanced `Service` URL. Instead, it lists the service's `EndpointSlice` objects and probes **each ready endpoint address individually**. This ensures a busy replica behind a healthy Service doesn't cause the idle check to pass incorrectly.

The rollout waits until **every** ready replica reports idle. If even one is busy or unreachable, the entire rollout defers. When no EndpointSlices exist yet (freshly created Deployment), the controller falls back to a single Service URL probe for backward compatibility.

## Fail-closed semantics

The `RolloutDeferred` condition communicates why a rollout is held:

| Condition state | Reason | Meaning |
|---|---|---|
| `True` | `PodsBusy` | One or more replicas are actively processing requests |
| `True` | `IdleCheckFailed` | The controller could not reach or parse the idle signal (network error, non-200 response) |
| `True` | `IdleCheckUnsupported` | The selected runtime does not implement idle detection, or `generic` without `idle-endpoint` annotation |
| `False` | `IdleTimeoutExceeded` | The `idleTimeoutSeconds` budget expired; rollout proceeded despite busy pods |

In all cases the rollout eventually proceeds: either when idle is confirmed, or when the timeout expires. The timeout is a safety valve — it prevents an endlessly stuck rollout if a runtime hangs in a permanently busy state.

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
