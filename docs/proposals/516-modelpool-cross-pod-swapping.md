# LLMKube: `ModelPool` cross-pod sticky model swapping on a shared GPU slot

> **Local scratchpad only, not a repo deliverable.** Per the maintainer's
> decision on #516, `ModelPool` gets its **own issue**: #516 stays the
> in-process `runtime: llamacpp-router`, and the cross-pod `ModelPool` shape is
> tracked separately (Defilan is opening it and cross-linking). This doc is the
> working design/scratchpad; PRs reference the new ModelPool issue (`Refs #516`
> for context, not `Fixes #516`). Keep the DCO sign-off intact.

## Decision: separate ModelPool issue, complementary to #516
- The maintainer approved the cross-pod direction and asked for `ModelPool` to
  live in its **own issue**, distinct from #516 (the in-process runtime), so the
  two shapes are not conflated.
- **#516 as filed (`runtime: llamacpp-router`)** is *in-process* swap: one
  `llama-server` Pod, N models, swap inside the process. Good when models share
  a runtime/Pod and you accept llama.cpp owning the swap scheduler.
- **`ModelPool` (this design)** is *cross-pod* swap: N independent single-model
  `InferenceService`s sharing one exclusive GPU slot, fronted by `ModelRouter`,
  with VRAM-gated scale-to-zero. This is the shape where **LLMKube owns the
  queue and the swap policy in Go**, so the anti-thrash behavior is expressible
  without any llama.cpp change. It also allows per-member runtime divergence
  (different images, context sizes, vLLM vs llama.cpp) that in-process mode
  cannot. Complementary to the in-process runtime.

They compose: a `ModelRouter` can front both in-process router-mode services and
`ModelPool`-managed single-model services transparently.

## Decisions from the #516 thread (baked into this design)
- **Drain-before-unload reuses the `/slots` idle contract** (merged in #1088),
  not a separate definition of "drained": a busy incumbent is never scaled down,
  and an unreachable idle check fails closed (incumbent stays resident).
- **No traffic-triggered force-unload.** Unlike a rollout, there is no
  `idleTimeout` that force-proceeds; a swap that cannot drain stays deferred
  (`SwapDeferred` / `PodsBusy` / `IdleCheckFailed`). The bound on a waiting
  request is the router's hold budget (503 + `Retry-After`), never a
  controller-side force. Any force must be an explicit admin action.
- **v1 is k8s-GPU-only.** On a one-GPU node k8s device-plugin gating makes the
  exclusive slot free (the displaced member sits `Pending` until the incumbent
  releases the device). Metal hosts have no such gating, so metal-backed members
  are refused in v1 (`MetalSupported=False`); metal-agent unload-before-load
  enforcement is a follow-up.
- **Fast-interleaved-swap safety is tested explicitly** so a device-plugin
  reclaim lag cannot briefly co-schedule two residents and OOM.

## v2 follow-ups (out of scope here)
- **Prompt-cache save/restore across swaps** (JJGadgets): before unloading a
  member, save its llama-server slot prompt cache to a PVC; restore it on
  reload via the slot API, keyed by a model hash. Attacks the swap's biggest
  cost (context reprocessing) for planner-with-subagents flows.
- **Metal path** (`metal-agent` enforces unload-before-load on hosts without
  device-plugin GPU gating).

## Why cross-pod for the anti-thrash policy
The novel behavior (sticky residency, coalesce-before-swap, min-dwell) is a
*scheduling* concern. In the in-process shape that scheduler lives inside
`llama-server` (bounded by llama.cpp #24849 / #22560). In the cross-pod shape the
`ModelRouter` proxy already sees every request and owns the per-backend queue, so
the policy is implementable in LLMKube's existing Go control/data plane.

## What LLMKube already ships (reuse, don't rebuild)
- `spec.Replicas: 0` gives a real `PhaseStopped` (scale-to-zero exists; just manual).
- `spec.Priority` gives `priorityClassMap` + Kubernetes PriorityClass preemption
  (`internal/controller/scheduling.go`).
- FIFO GPU queue across `WaitingForGPU` services (`calculateQueuePosition`),
  surfaced as `status.QueuePosition` / `WaitingFor` / `EffectivePriority`, plus
  Prometheus `llmkube_gpu_queue_depth` / `gpu_queue_wait_duration_seconds`.
- `ModelRouter` is already the production request boundary and proxy.

The gaps this proposal fills:
1. **Scale-from-zero on demand**: wake a `PhaseStopped` member when a request
   for it arrives (activator pattern).
2. **Exclusive slot with VRAM gating**: at most one member resident; the
   incumbent fully unloads (VRAM freed) before the next loads.
3. **Sticky + anti-thrash swap policy**: see below.

## Confirmed decisions
- [x] CRD: dedicated `ModelPool` CR (separation of concerns; different node
      types get different policy).
- [x] `swapPolicy: sticky | priority`, sticky default.
- [x] min-dwell is **queue-depth N** (a count), not seconds (`minResidency`).
- [x] Activation lives in `ModelRouter` (owns queue + cold-start budget).
- [x] First-PR scope: `ModelPool` CR + exclusive-slot reconciler (sticky); then
      activation + anti-thrash as PR 2.
- [x] DCO identity: Sylvain Niles <540991+sylvainsf@users.noreply.github.com>.

All decisions locked. The comment below is ready to post on #516.

---

--- COMMENT ON #516 ---

Following up from our chat, here is the **cross-pod** shape written up as a
concrete resolution path for this milestone, complementary to the in-process
`runtime: llamacpp-router` already proposed here. A `ModelRouter` can front both
kinds transparently, so they coexist.

### `ModelPool`: cross-pod sticky model swapping on a shared GPU slot

### Problem

On a constrained single node (e.g. a 64 GB unified-memory mini-PC, or a single
24 GB GPU), two large models can't co-reside: a ~32B judge/eval model and a
similarly-sized coder need the *same* memory. I want to offer both behind one
endpoint, swapping which is resident on demand, **across independent Pods** so
each can keep its own image / context size / runtime.

This is the cross-pod, VRAM-gated swap noted as out-of-scope in the issue
description. Raising it here since, per our discussion, it can serve as this
milestone's resolution alongside the in-process runtime. The two are
complementary: the filed proposal is one Pod swapping models in-process; this is
N single-model `InferenceService`s sharing one exclusive GPU slot, fronted by
`ModelRouter`. A `ModelRouter` can front both kinds transparently.

### Why a separate shape (not just the in-process runtime)

- **Per-member runtime divergence.** Different images, context sizes, flags, or
  even engines (llama.cpp vs vLLM) per model, impossible in one shared
  `llama-server` process.
- **Policy lives in LLMKube, in Go.** The swap scheduler is the `ModelRouter`
  proxy, which already owns the per-backend queue, so sticky/anti-thrash policy
  needs no upstream llama.cpp change (unlike in-process mode, bounded by
  ggml-org/llama.cpp#24849 / #22560).
- **No upstream anti-thrash on the roadmap.** In-process mode would inherit its
  swap policy from `llama-server`, but there is **no planned anti-thrash feature
  in llama.cpp**: #24849 is only the in-flight-abort *bug* (finish before
  unload), and #22560 is preemptive *priority* scheduling. Neither covers
  coalescing same-model demand or min-dwell, and nothing else is filed. So the
  in-process path can't get this behavior without first landing a new upstream
  feature; the cross-pod path delivers it in LLMKube today.
- **Reuses existing machinery.** `Replicas: 0` gives `PhaseStopped`, the
  `priorityClassMap` preemption path, and the FIFO `WaitingForGPU` queue with
  `QueuePosition` / `EffectivePriority` status + queue-depth metrics already
  exist; this composes them.

### Proposed shape

A `ModelPool` CR names a set of member `InferenceService`s that share one
exclusive GPU slot on a node, plus the swap policy:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelPool
metadata:
  name: heavy-slot
  namespace: lab
spec:
  # The GPU slot these members share (one node / one device class).
  nodeSelector:
    lab.example/inference: heavy
  gpu: 1
  swapPolicy: sticky        # sticky | priority
  members:
    - inferenceServiceRef: { name: judge }
    - inferenceServiceRef: { name: coder }
  # Optional: which member is warm on a cold pool (else first request wins).
  default: judge
```

Controller invariant: **at most one member `Ready` per pool**; the rest are held
at `PhaseStopped`. Members are otherwise ordinary single-model
`InferenceService`s (own image, ctxSize, etc.).

### Swap policy

- **Sticky (default).** Whatever member is resident **stays** resident until a
  *different* member is requested, no automatic restore of a default. Example:
  coder warm for hours; a judge request arrives; the slot flips to the judge and
  *stays* through many judge requests until a coder request returns hours later.
- **Drain before swap.** The incumbent finishes its in-flight request(s) before
  unloading; VRAM is fully freed before the next member loads (the hard
  unload-before-load requirement).
- **Coalesce same-model demand (anti-thrash).** Don't swap on the *first*
  cross-model request; flip only once the incumbent's queue is **empty**. A
  cross-model request does not get rejected while it waits: the router **holds
  it open** (the same activator hold described below), and that held connection
  *is* its place in the queue. Worked example: A resident with 1 in-flight; a B
  request arrives and is held open (queued); before A drains, another A request
  arrives, so the second A is served on the warm A and B stays held; the slot
  flips to B (drain + unload A, load B, forward B on its held connection) only
  once A's queue actually empties. No request is 503'd in this path; 503 is only
  the budget-exceeded fallback (see Cost to document). Prevents GPU ping-pong
  under interleaved multi-agent traffic. This idleness gate is the *entire*
  anti-thrash mechanism in v1: no request counting, no timers (a guaranteed
  min-dwell floor is deferred to v2, see below).
- **`priority` mode (opt-in).** Reuse `EffectivePriority` so a designated member
  reclaims the slot when the preemptor goes idle, the behavior that falls out
  of the current priority model, for batch/SLO fleets.

### Activation (ModelRouter)

`ModelRouter` is the home for scale-from-zero: a request to a `PhaseStopped`
member is **held open** while the pool drains+unloads the incumbent and scales
the target to 1, then the held request is forwarded, the KServe activator
pattern. Holding is also how cross-model requests "queue" during coalescing
above: a queued request is just a held-open connection, never a rejection, so
queue position is real connection state in the proxy, not something the client
has to retry into. The hold is bound by a dedicated pool `swapBudget`
(`ModelPool.spec.swapBudget`, default 300s), decoupled from the router's
response-header (generation) timeout so a large cold load can take minutes
without operators having to inflate the per-request generation cap for every
request. The proxy's per-backend queue of held requests is exactly the signal
the coalescing + min-dwell policy needs (in-flight count + pending held
requests per member).

### Cost to document

- Cold-swap latency (incumbent unload + target load) on the first request after
  a model change. Default behavior is the activator **hold** above: the client's
  single request simply takes longer, inside the router's per-request timeout
  budget, so no client-side retry is required and there is no OOM. Holding is
  preferred over returning an error because retry-on-503 is not universal across
  clients (the OpenAI SDKs retry `>=500` with a couple of backoff attempts, but
  plain `httpx`/`requests`, curl, and many agent frameworks do not), and a cold
  load can exceed those default retry windows anyway.
- **503 is only the fallback** when the cold-start hold exceeds the budget. In
  that case return `503` with a `Retry-After` header (the standards-correct
  "temporarily unavailable" signal) so well-behaved clients back off
  deterministically rather than relying on per-client 503 defaults. `429` is the
  wrong code here (it implies rate limiting).
- **Client timeout vs swap time.** The hold only works if the client's request
  timeout outlives the swap. A big-model cold swap (unload incumbent + load
  target) is roughly 15 to 45s for a ~32B Q4, and 30 to 90s for a ~70B Q4,
  depending on whether the GGUF is in page cache or cold off NVMe. Generous
  client defaults clear this easily (OpenAI Python/Node SDK and LiteLLM default
  to 600s; curl has no default), but **short defaults do not**: raw `httpx`
  defaults to 5s total and will fail mid-swap. Operators should set client
  timeouts to at least worst-case swap time plus generation time. The router
  hold budget is sized independently through `ModelPool.spec.swapBudget` (default
  300s), which bounds only the swap wait and is deliberately separate from the
  response-header (generation) timeout: raise `swapBudget` for large models
  without loosening the per-request generation cap. This belongs in the docs so
  callers on `httpx`-based stacks are not surprised.
- **Client disconnect during a swap.** If a client gives up mid-hold, its held
  request is dropped, but an in-progress load must **not** be aborted just
  because one caller bailed: finish the swap so the now-warm member serves the
  next request. (Unload-before-load still holds; a disconnect never leaves two
  members resident.)
- Per-member HPA is constrained: a pooled member scales 0 to 1 within its slot,
  not freely.

### Proposed first-PR scope

`ModelPool` CR + the exclusive-slot reconciler implementing **sticky** residency
(at most one member `Ready`, drain-before-unload), with Ginkgo coverage under
`internal/controller/`. Activation + coalescing/min-dwell + `priority` mode would
follow as PR 2 on `ModelRouter`. Happy to prototype against `api/v1alpha1` +
`scheduling.go` + the router proxy if the direction looks right.

Signed-off-by: Sylvain Niles <540991+sylvainsf@users.noreply.github.com>
