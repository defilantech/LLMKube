# GPU Sharing

GPU sharing lets an InferenceService declare how it consumes its GPU so that
small services do not waste whole cards, teams are isolated from each other,
and quota reflects what was actually consumed. The field lives on
`InferenceService.spec.resources.gpuSharing` and is vendor-neutral: the
operator resolves the mode plus the Model's GPU vendor to the concrete
scheduling mechanism (extended resource name, node pool, tolerations).

When `gpuSharing` is unset, the behavior is identical to every manifest
written before this field existed: the pod owns whole device(s) exclusively.

## Modes

| Mode | NVIDIA | AMD (Strix APU) | Scheduling |
|---|---|---|---|
| `exclusive` | `nvidia.com/gpu: N`, tensor-parallelism capable | existing Vulkan/ROCm path | any GPU node (today's behavior) |
| `partitioned` | `nvidia.com/mig-<profile>: 1` (e.g. `nvidia.com/mig-1g.24gb`) | rejected (APU cannot partition) | implicit via MIG resource name |
| `shared` | `nvidia.com/gpu: 1` on a time-sliced pool | iGPU co-location + VRAM quota | pool nodeSelector from Helm mapping |

### exclusive

The default. The pod owns whole device(s). This is the only mode that
supports tensor parallelism (multi-GPU). No change from existing behavior.

### partitioned

Requests a hardware partition of a device. For NVIDIA this resolves to a
MIG extended resource name derived from the `profile` field (e.g. profile
`1g.24gb` becomes `nvidia.com/mig-1g.24gb`). The profile must match the
pattern `Ng.Mgb` (e.g. `1g.24gb`, `3g.90gb`). Partitioned mode requires
exactly 1 GPU and is rejected for non-NVIDIA vendors (AMD APU cannot
partition; MI300 CPX/SPX mapping is a follow-up).

### shared

Co-locates the pod with other workloads on a shared device. For NVIDIA
this means a time-sliced pool: the pod still requests `nvidia.com/gpu: 1`
but is steered onto a label-separated node pool so it does not collide
with exclusive workloads. The operator rejects shared mode when no shared
pool is configured (fail-closed). For AMD APU (Vulkan/ROCm iGPU),
co-location shares the single device resource by design, so no pool
separation is needed.

## When to use each mode

- **exclusive**: production workloads that need the full device, or models
  that use tensor parallelism across multiple GPUs.
- **partitioned**: multi-tenant production where teams need hardware-isolated
  slices of a datacenter GPU. Each partition is a hard boundary: one team
  cannot OOM another.
- **shared**: dev/test workloads, burst capacity, or small models that fit
  comfortably alongside each other on a single device. Requires a
  `memoryLimitGiB` declaration for VRAM-based quota accounting.

## InferenceService examples

### exclusive (default)

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: team-a-70b
spec:
  modelRef: llama-70b
  resources:
    gpu: 2
```

No `gpuSharing` field is needed; exclusive is the default. This service
owns 2 whole GPUs and can use tensor parallelism.

### partitioned

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: team-b-7b
spec:
  modelRef: qwen-7b
  resources:
    gpu: 1
    gpuSharing:
      mode: partitioned
      profile: "1g.24gb"
```

This service requests one MIG partition of profile `1g.24gb`. The operator
resolves this to the extended resource `nvidia.com/mig-1g.24gb`. The
profile must be valid (matches `Ng.Mgb` pattern) and the Model's GPU
vendor must be NVIDIA.

### shared

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: team-c-dev-7b
spec:
  modelRef: qwen-7b
  resources:
    gpu: 1
    gpuSharing:
      mode: shared
      memoryLimitGiB: 24
```

This service co-locates on a shared GPU pool and declares its memory
footprint via `memoryLimitGiB`. The operator steers the pod onto the shared
pool using the configured node selector. The `memoryLimitGiB` drives
VRAM-based quota accounting.

`memoryLimitGiB` is a declaration, not a cap. Nothing enforces it at
runtime: a workload that grows its KV cache past the figure it declared will
do so, and on a time-sliced device it can exhaust the VRAM its co-tenants
need. Time-slicing multiplexes execution, not memory. Treat `shared` as
co-scheduling for workloads that already trust each other, and reach for
`partitioned` when you need a boundary that holds between tenants, since a
MIG partition is enforced by the hardware.

## ModelPool: many models, one slot

`exclusive`, `partitioned` and `shared` all divide a device between workloads
that run *at the same time*. A ModelPool solves the opposite problem: several
models that each want the whole slot, but are not needed simultaneously. At most
one member is resident; the rest are held at `Stopped` with `replicas: 0`, and
the slot changes hands on demand.

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelPool
metadata:
  name: coder-pool
spec:
  gpu: 1
  nodeSelector:
    kubernetes.io/hostname: gpu-node-1
  swapPolicy: sticky
  swapBudget: 300s
  default: coder-small
  members:
    - inferenceServiceRef: {name: coder-small}
    - inferenceServiceRef: {name: coder-large}
```

Each member is an ordinary single-model InferenceService with its own image,
context size and runtime. `spec.gpu` is informational: members declare their own
resource requests, and this only records the slot size so the pool invariant is
visible at a glance. `nodeSelector` pins the slot, and every member is expected
to land on that node so they genuinely contend for the same device memory.

### What it looks like when it is working

```
$ kubectl get modelpool coder-pool
NAME          POLICY   RESIDENT      PHASE   AGE
coder-pool    sticky   coder-small   Ready   4m

$ kubectl get isvc coder-small coder-large
NAME          PHASE     ...
coder-small   Ready               # owns the slot, replicas 1
coder-large   Stopped             # held by the operator, replicas 0
```

The operator sets `replicas: 0` on the non-resident members itself. Do not
"fix" a member that shows `Stopped`: in a pool that is the normal held state,
not a failure.

Four conditions report the pool's health:

| Condition | Meaning |
| --- | --- |
| `MembersValid` | every `inferenceServiceRef` resolves. `False` / `MissingMembers` lists the names that do not exist, and the pool sits `Degraded` |
| `SlotAllocated` | `True` / `Resident` names the owner. `False` / `Swapping` means no member is resident yet |
| `SwapDeferred` | a swap is being held off rather than performed |
| `MetalSupported` | whether the members are Kubernetes GPU-gated or Apple-metal backed |

### GitOps: leave `replicas` to the pool

The pool owns its members' `spec.replicas`. It has no other request signal: a
non-resident member at `replicas: 1` or more *is* a request for the slot, which
is how the router activates a member and why a manual `kubectl scale` works the
same way. Any tool that writes a member's `replicas` back on a schedule turns
every sync into an activation.

Symptoms of a tool fighting the pool:

- the resident member changes at your GitOps sync interval, with nobody sending
  requests for the other model
- `kube_pod_created` for the members shows a new pod every interval
- a request for one model loads the other one first, then swaps, and a large
  model that takes most of an interval to load rarely serves at all

Leave `replicas` out of every member's manifest:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: coder-large
spec:
  modelRef: coder-large
  # no replicas: the ModelPool scales this member between 0 and 1
```

**Flux.** kustomize-controller applies server-side, so once `replicas` is gone
from the manifest Flux no longer owns the field and stops resetting it. The
commit that drops it can cause one more swap: when Flux gives up the field, the
API server fills in the CRD default of `1`, and the pool reads that as an
activation for a held member. It settles after that. Do not reach for the
`kustomize.toolkit.fluxcd.io/ssa: IfNotPresent` annotation instead: it stops
Flux from applying any later change in Git to those objects, not just
`replicas`.

**Argo CD.** Leave `replicas` out of the manifests, or tell Argo to ignore it.
`ignoreDifferences` on its own only hides the field from the diff; a sync still
applies the Git value unless the Application also sets
`RespectIgnoreDifferences=true`:

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
spec:
  ignoreDifferences:
    - group: inference.llmkube.dev
      kind: InferenceService
      jsonPointers:
        - /spec/replicas
  syncPolicy:
    syncOptions:
      - RespectIgnoreDifferences=true
```

**Autoscaling.** Do not set `spec.autoscaling` on a pool member. With it set,
the HorizontalPodAutoscaler owns the member's Deployment replica count and the
operator preserves the live value, so the pool's `replicas: 0` never scales the
member down and the swap waits for Pods that do not go away. LLMKube rejects
this combination: the pool reports `MembersValid=False` with reason
`MemberAutoscaling`, names the affected member, and makes no replica changes.

### Removing a member: stop it yourself

Removing a member from `spec.members` takes effect on the next reconcile. If the
member that left was the resident, status stops naming it in that same pass, and
the slot resolves the way a cold pool resolves it. A swap already in flight
continues first. Otherwise, when no remaining member is scaled up, `spec.default`
warms if it names one, and the pool stays cold if it does not. When members are
scaled up, `spec.default` takes the slot if it is one of them, else the first
scaled-up member in declaration order.

The pool does not stop the departing member, and it does not wait for its Pods.
Every replica write it makes targets a current member, so once the member is no
longer listed the pool leaves its `spec.replicas` alone. That cuts both ways for
a resident:

- **Scaling a listed resident to zero does not stop it.** The pool owns its
  members' replicas and scales its resident back up on the next reconcile.
- **Scaling it to zero after the removal does stop it.** Nothing writes to a
  member the pool no longer lists.

There are two safe orders, and the GPU mode decides which one to use.

**Remove it, then stop it.** The next owner is picked in the same pass that drops
the resident, so whenever `spec.default` is set or another member is scaled up,
that owner starts while the departing member is still running:

```sh
# 1. Remove the member from spec.members in your manifest and apply it.
# 2. Stop the member the pool no longer manages.
kubectl scale isvc coder-large --replicas=0
```

**Delete it, then remove it.** If the member is not `spec.default`, delete its
`InferenceService` while it is still listed, wait for its Pod to exit, then
remove it from `spec.members`. While the reference does not resolve the pool sits
`Degraded` with `MembersValid=False` / `MissingMembers`, makes no replica changes,
and rechecks every few seconds, so nothing new starts until the departing Pod is
gone. Deleting the `InferenceService` garbage-collects its Deployment and Pods.

The overlap in the first order is safe only on discrete GPUs under `exclusive`
(whole devices) or `partitioned` (MIG). There the incoming Pod either waits
`Pending` for the device the departing Pod holds, so the pool looks stuck rather
than oversubscribed, or lands on a free one. Use the second order everywhere
else:

- **`shared` (time-sliced).** Both Pods run and share device memory, which is
  the co-residence the pool exists to prevent.
- **Unified-memory nodes** (an AMD APU, a GB10), in any mode. The device plugin
  frees the GPU when the departing Pod starts terminating, before its process
  has released the memory, so the incoming Pod can start in that window and the
  two briefly co-reside. The pool closes this window for its own members by
  waiting until their Pods are gone, and the second order gets the same wait
  from you.

Two edits belong in the same apply:

- **`spec.default`.** If it names the departing member, repoint or clear it.
  Reclaiming to a default that is not a member stops, and a cold pool has no
  default to warm. On-demand activation still works, because the pool falls back
  to the first scaled-up member.
- **The `ModelRouter` rule.** A backend counts as pooled only while the pool
  lists it. Until the router's config catches up it can still scale the member
  to one, and once it is gone from the pool nothing scales it back down.

### Sticky is the default policy, and it means what it says

`swapPolicy: sticky` (the default) keeps the incumbent until a *different* member
is requested.

One consequence surprises operators, so it is worth stating plainly: **editing
`spec.default` on a warm sticky pool does not move the slot.** `default` names the
member to warm on a *cold* pool, before anything has picked an owner. Once a
member is resident, sticky keeps it there and the `default` edit is inert until
the pool next goes cold. Verified on-cluster: after repointing `default` at the
other member, the incumbent still owned the slot two minutes later.

### Returning the slot to a background default: `swapPolicy: reclaim`

Sticky never brings the pool back to a preferred model on its own. That is fine
when both members are peers, but not when one is a *background default* you want
resident at rest and the other is an *on-demand* model a batch job bursts
against. With sticky, the first on-demand burst takes the slot and keeps it: the
on-demand member stays resident and idle, and (with `poolActivation: IfIdle`,
below) an interactive workload rides that idle on-demand model forever. Nothing
returns the slot to the default.

`swapPolicy: reclaim` adds that missing half. It behaves like sticky for
cross-model demand, and additionally returns the slot to `spec.default` once the
resident *non-default* member has been continuously idle for `spec.reclaimAfter`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: ModelPool
spec:
  swapPolicy: reclaim
  default: glimmer          # the background model, resident at rest
  reclaimAfter: 10m         # idle grace before the slot returns to glimmer
  members:
    - inferenceServiceRef: { name: glimmer }   # background default
    - inferenceServiceRef: { name: coder }     # on-demand burst tenant
```

The reclaim reuses the ordinary drain contract: the idle resident is drained
like any displaced incumbent, then the default loads. A busy resident is never
reclaimed. Reclaims are counted in `llmkube_modelpool_reclaims_total`.

A request for the resident can arrive after the reclaim has started draining it,
before the default is up. The router then scales the resident back up, and that
cancels the reclaim. The resident keeps the slot, and its idle clock starts over.

To tell this re-activation apart from the resident simply staying up, the pool
marks every member it scales to zero with the annotation
`inference.llmkube.dev/modelpool-drained: "true"`, and removes the mark when it
scales that member up as the owner.

**`reclaimAfter` must be longer than the on-demand member's inter-burst gap.**
This is the one way to misconfigure reclaim. If the grace is shorter than the gap
between the on-demand model's requests, the pool reclaims the default the moment
the burst pauses, the next request immediately swaps it back out, and you have
traded request coalescing for load thrash — two cold loads where sticky would
have done none. Size `reclaimAfter` to comfortably outlast a typical lull in the
on-demand traffic, not the gap between two adjacent requests. The idle clock
resets only when one of the idle polls catches the resident serving. Idleness is
sampled on a fixed interval (capped at ~30s, because no event fires on a
busy-to-idle transition), so a request that starts and finishes between two
polls is never observed and does not reset the clock: a stream of on-demand work
that keeps the member busy across polls holds the slot, but a sparse trickle of
short, sub-poll requests can slip between samples and let the slot reclaim during
a lull. Size the grace to outlast a genuine quiet period, and expect the guard to
keep the on-demand model resident only while its traffic keeps at least one poll
busy. A poll the resident does not answer holds the clock where it is rather than
restarting it, so whether the server happened to be up at poll time does not
change when the slot is reclaimed.

A member whose server is not answering is also not held resident on the drain
side. For a non-resident member that reports `Ready`, the idle probe answers
first, so a busy member is still deferred. The member's Pods are consulted only
when the connection is refused, because then nothing on its Service path is
accepting work: no Pod, a Pod that is terminating, or a Pod with no running
server container cannot be serving, and one running Pod is the Pod the Service
would have forwarded to. Those are drained. A member that does not report
`Ready` is not probed at all: it cannot be serving, so it drains as before. A
probe failure that could still hide in-flight work, a timeout, a non-200
response, an absent idle endpoint, or a refusal among two or more running Pods,
still fails closed and defers the swap, as does a member the probe reports busy.

### Serving on the warm member instead of waiting: `poolActivation: IfIdle`

By default a request for the non-resident member is held until the incumbent
drains, however long that takes (up to `swapBudget`). That is the right call
when the caller only accepts that one model. It is the wrong call for a caller
that *prefers* one member but would rather run on the other than queue behind
someone else's work: an interactive assistant that likes the small model, on a
slot a batch coder keeps busy for an hour at a time.

A `ModelRouter` rule can express that preference. List the members in
preference order and set `route.poolActivation: IfIdle`:

```yaml
rules:
  - name: assistant
    match:
      models: ["assistant"]
    route:
      backends: ["coder-small", "coder-large"]
      poolActivation: IfIdle
```

Under `IfIdle` the proxy only starts a swap when the incumbent is idle. If
`coder-small` is not resident and `coder-large` has requests in flight, the
proxy skips `coder-small` at once and dispatches to `coder-large`, the member
that is already warm. Nothing is held and no swap is queued. When the slot is
idle the rule behaves exactly like the default (`Wait`): the swap runs under
`swapBudget` and the request is served by `coder-small` afterwards. A swap that
is already in flight is waited on in both modes, because the incumbent is
unloading and cannot serve anyway. The fall-through backend receives its own
served model name in the outbound request (its `--served-model-name`, by
default `spec.modelRef`, for vLLM and SGLang; the model-store directory
basename for oMLX; otherwise the InferenceService name) rather than the alias
the client asked for, so a runtime that validates the field accepts it instead
of answering `404 The model ... does not exist`.

"Busy" means requests the proxy itself is tracking. Traffic that reaches a
member's Service directly, bypassing the router, is invisible to this check
(the controller's `/slots` drain still protects it at swap time). Route all
pool traffic through the router if you rely on `IfIdle`. Skips are counted in
`llmkube_modelpool_busy_skips_total{router,pool,member}`.

If *every* backend in an `IfIdle` rule skips because no preferred member is warm
(the slot is held busy by a member outside the rule), the request cannot be
served right now. The proxy returns `503` with a `Retry-After` header and the
audit reason `pool_incumbent_busy`, the same retryable signal as a swap that
outruns `swapBudget`, so a client that retries after the incumbent drains is
served. It is deliberately not a `502`: nothing is broken upstream, the slot is
just occupied.

### Precise drain for a custom-image member: the metric idle probe

The `llamacpp`, `vllm` and `sglang` runtimes know how to ask their server whether
it is idle (llama.cpp's `/slots`, vLLM's `vllm:num_requests_running` gauge), so a
swap waits for in-flight work to finish before freeing the slot. The `generic`
runtime, used for a custom image whose entrypoint the built-in runtimes cannot
drive, has no such knowledge. By default it can only be told a health path via
`inference.llmkube.dev/idle-endpoint`, and a plain health check returns 2xx even
while the server is busy, so the pool treats such a member as always idle and can
preempt it mid-request.

If the custom image exposes Prometheus metrics with an in-flight-work gauge (most
vLLM- and SGLang-derived images do), point the generic idle probe at it instead:

```yaml
metadata:
  annotations:
    inference.llmkube.dev/idle-metric: "vllm:num_requests_running"
    # optional, default 0
    inference.llmkube.dev/idle-metric-threshold: "0"
    # optional, default /metrics
    inference.llmkube.dev/idle-metric-path: "/metrics"
```

The probe scrapes the metrics path, sums the named gauge, and reports the member
idle only when the sum is at or below the threshold, the same precise drain the
native runtimes get. It fails closed: a non-200 scrape or an absent gauge counts
as busy, so a member whose idleness cannot be established is never preempted.
`idle-metric` takes precedence over `idle-endpoint` when both are set.

### swapBudget, and why it is separate from the request timeout

`swapBudget` (default `300s`) bounds how long the router holds a cross-model
request open while it drains the incumbent and cold-loads the target. It is
deliberately decoupled from the router's response-header timeout so a large
model can take minutes to load without loosening the per-request generation cap
for every request on the fleet. If the budget elapses before the target is
resident, the held request fails with `503` and a `Retry-After`; the caller can
retry, and the now-warm member serves the next one.

`swapBudget` also bounds the swap itself, not just the request holding on it. If
the target has not reported `Ready` within the budget, the router abandons the
swap: it scales the target back to zero, logs `model pool swap abandoned` at
`ERROR` with the pool, the two members and the elapsed time, counts it in
`llmkube_modelpool_swap_failures_total{router,pool,member,reason}` (with
`reason="deadline"`; an activation the controller rejected outright is counted
with `reason="error"`), and fails the waiting requests with `503` and a
`Retry-After`, the same retryable signal as a hold that runs out of budget.
Whichever of the two deadlines passes first, the client is told to retry rather
than read the response as an upstream outage. This
matters because the pool is otherwise unusable while a swap is in flight: the
resident member's fast path is closed for the duration, so an unbounded swap
whose target can never become `Ready` would 503 every request on the pool -
including requests for the member that is still loaded - until the proxy was
restarted. Abandoning the swap reopens the resident's fast path, and the next
request for the target starts a fresh swap. A pool with no explicit
`swapBudget` uses twice the router's response-header timeout as the bound.

A swap that has started runs to `Ready` or to that deadline even when the
request that started it goes away. A caller that times out or disconnects is
dropped, and other requests for the pool are held until the swap finishes, but
the load continues, so the retry finds the member warm instead of paying for a
second cold load. A failed swap scales its own target back down before the
requests waiting on it are told the swap failed, whether the swap was abandoned
at its deadline or the activation itself failed. The hedge is that a request for
the member that is still loaded
waits out a swap nobody else wanted, because the resident's fast path is closed
for every swap in flight, waited on or not. With 45 to 90 second boots that wait
is bounded by `swapBudget`, and the alternative was a cold load on every retry.

### A pooled router runs one replica

When any backend resolves to a ModelPool member, the operator pins the router
to a single replica regardless of what the spec asks for. Pool activation
serialises swaps through an in-process lock, so a second proxy replica would
race it and thrash the slot. Cross-replica swap coordination is a follow-up, so
do not plan a highly-available pooled router yet.

## Fleet configuration

GPU sharing requires fleet-level configuration so the operator knows where
the shared pool lives and how much VRAM a whole device has.

### Shared pool node selector

The shared pool is configured via the Helm chart value
`gpuSharing.pools.shared.nodeSelector`, which the operator passes as the
`--gpu-sharing-shared-pool-selector` flag. This is a comma-separated
`key=value` string that becomes a node selector merged onto every
shared-mode pod.

Chart example:

```yaml
gpuSharing:
  pools:
    shared:
      nodeSelector:
        gpu-pool: shared
```

Or via the operator flag directly:

```
--gpu-sharing-shared-pool-selector=gpu-pool=shared
```

When this is empty (the default), shared mode is rejected with an
actionable error: at admission on fleets running the multitenancy webhook
(`multitenancy.enabled=true`), or at reconcile time (the InferenceService
parks at `Phase=Failed`) on installs without it. This is intentional:
shared workloads collide with exclusive ones on the same extended
resource name, so they must be steered to a label-separated pool.

### VRAM per device

The operator flag `--gpu-sharing-vram-per-device-gib` (chart value
`gpuSharing.vramPerDeviceGiB`) declares the device memory in GiB of one
whole GPU in the fleet. It is used to derive the VRAM footprint of
exclusive-mode InferenceServices for GPUQuota `vramBytes` accounting.

Chart example:

```yaml
gpuSharing:
  vramPerDeviceGiB: 80
```

When this is 0 (the default), exclusive-mode footprints are unknown. A
quota that declares a `vramBytes` cap then denies exclusive-mode
admissions with an actionable message rather than silently waving them
through.

## VRAM-based GPUQuota accounting

When a GPUQuota declares a `vramBytes` cap, the operator derives each
InferenceService's VRAM footprint from its `gpuSharing` spec. The
footprint per pod is:

| Mode | Footprint derivation |
|---|---|
| `partitioned` | Parsed from the MIG profile name (e.g. `1g.24gb`). The partition size IS the hard footprint. |
| `shared` | `memoryLimitGiB` when declared, else the Model's `hardware.gpu.memory` quantity, else unknown. |
| `exclusive` | GPU count x `vramPerDeviceGiB` (fleet config). Zero/unset means unknown. |

The total footprint is per-pod footprint multiplied by replicas.

### Unknown-footprint denial

When a quota declares a `vramBytes` cap, the operator cannot admit a
workload whose footprint is unknowable. This prevents undeclared
workloads from consuming the very budget the cap exists to protect. The
denial message is actionable:

```
cannot derive the VRAM footprint required by this quota's vramBytes cap:
declare gpuSharing.memoryLimitGiB (shared), a partition profile
(partitioned), or configure --gpu-sharing-vram-per-device-gib for
exclusive workloads
```

To resolve this, either declare the footprint on the InferenceService or
configure the fleet-level `vramPerDeviceGiB` for exclusive workloads.

For more on GPUQuota and multi-tenancy, see [multi-tenancy.md](multi-tenancy.md).

## Cluster-admin day-0: NVIDIA GPU Operator setup

LLMKube does not manage node configuration. The GPU Operator owns node
configuration; LLMKube owns the request and governance. The following
steps are the cluster-admin's responsibility before deploying
InferenceServices that use GPU sharing.

### MIG geometry (partitioned mode)

To use partitioned mode, the NVIDIA GPU Operator must be configured with
`mig.strategy: mixed` in the ClusterPolicy. This enables the MIG manager
and device plugin to advertise MIG extended resources.

```yaml
apiVersion: nvidia.com/v1
kind: ClusterPolicy
metadata:
  name: gpu-operator
spec:
  mig:
    strategy: mixed
```

After applying this, label each MIG-capable node with the NAME of a
partition layout defined in the MIG manager's configuration (the
`default-mig-parted-config` ConfigMap, or a custom one). The label value
is a config name such as `all-1g.24gb`, not a raw profile string:

```bash
kubectl label node gpu-node-1 nvidia.com/mig.config=all-1g.24gb
```

The MIG manager applies that layout and the device plugin then advertises
`nvidia.com/mig-1g.24gb` as an extended resource. Available profiles and
the shipped layout names depend on the GPU model; consult the NVIDIA MIG
User Guide and the mig-parted config for your GPU Operator version.

### Time-slicing (shared mode)

To use shared mode on NVIDIA GPUs, configure time-slicing in the NVIDIA
device plugin. With the GPU Operator this is a ConfigMap of named
configurations (the sharing block lives under `sharing.timeSlicing`),
referenced from the ClusterPolicy:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: time-slicing-config
  namespace: gpu-operator
data:
  any: |
    version: v1
    sharing:
      timeSlicing:
        resources:
          - name: nvidia.com/gpu
            replicas: 4
```

Wire it up in the ClusterPolicy so the device plugin consumes it:

```yaml
spec:
  devicePlugin:
    config:
      name: time-slicing-config
      default: any
```

This example creates 4 time-sliced replicas of `nvidia.com/gpu` on each
node, allowing up to 4 pods to share a single GPU. The `replicas` value
should be chosen based on the expected workload density and memory
footprints.

### Pool node labels (shared mode)

Label the shared-pool nodes so the operator's node selector can steer
shared-mode pods onto them. The label key and value must match the
`gpuSharing.pools.shared.nodeSelector` chart value.

```yaml
apiVersion: v1
kind: Node
metadata:
  name: gpu-node-2
  labels:
    gpu-pool: shared
```

Note the direction of enforcement: the pool label steers shared-mode
pods IN, but nothing repels exclusive-mode pods from shared-pool nodes.
Keeping exclusive workloads off the pool is cluster-admin scheduling
policy: either give exclusive services a nodeSelector for the exclusive
nodes, or taint the shared-pool nodes with a custom key. If you taint,
remember that shared-mode services must then declare a matching
toleration in `spec.tolerations` (the operator's automatic GPU
toleration covers only the device resource taint, not custom pool
taints).

## AMD APU (unified-memory iGPU) note

AMD Strix APUs with integrated GPUs (Vulkan/ROCm) share a single device
resource by design. Every workload on the node lands on the same iGPU,
so no pool separation is needed. Shared mode on AMD APU resolves to the
same resource name as exclusive mode, with VRAM accounting driven by
`memoryLimitGiB` or the Model's `hardware.gpu.memory`.

Partitioned mode is not supported on AMD APU: the hardware cannot
partition the iGPU. If you need hardware isolation on AMD, use exclusive
mode with separate nodes.

## Current limitations

- **Partitioned is NVIDIA MIG only**: AMD MI300 CPX/SPX partition mapping
  is a follow-up. Partitioned mode is rejected for any non-NVIDIA vendor.
- **MPS is a follow-up**: NVIDIA MPS (Multi-Process Service) memory
  pinning is not yet supported as a `shared` refinement. The current
  shared mode uses time-slicing only.
- **No automatic profile selection**: the operator does not choose a MIG
  profile for you; you must declare it explicitly in the InferenceService
  spec.
- **No dynamic pool rebalancing in `shared` mode**: the shared pool is
  static. If you need to move workloads between pools, update the node
  labels and the chart value, then restart affected InferenceServices.
  Note this is a limitation of `shared` mode specifically. If what you
  want is several models taking turns on one slot rather than co-resident
  workloads, that is a [ModelPool](#modelpool-many-models-one-slot).
- **Pooled routers are single-replica**: a router with any ModelPool
  member as a backend is pinned to one replica, because swap activation
  serialises through an in-process lock.
