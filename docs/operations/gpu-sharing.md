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
busy.

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
unloading and cannot serve anyway.

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
with `reason="error"`), and fails the waiting requests with a swap error. This
matters because the pool is otherwise unusable while a swap is in flight: the
resident member's fast path is closed for the duration, so an unbounded swap
whose target can never become `Ready` would 503 every request on the pool -
including requests for the member that is still loaded - until the proxy was
restarted. Abandoning the swap reopens the resident's fast path, and the next
request for the target starts a fresh swap. A pool with no explicit
`swapBudget` uses twice the router's response-header timeout as the bound.

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
