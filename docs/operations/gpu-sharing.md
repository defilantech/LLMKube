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

This service co-locates on a shared GPU pool and declares a memory limit
via `memoryLimitGiB`. The operator steers the pod onto the shared pool
using the configured node selector. The `memoryLimitGiB` drives VRAM-based
quota accounting.

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
- **No dynamic pool rebalancing**: the shared pool is static. If you need
  to move workloads between pools, update the node labels and the chart
  value, then restart affected InferenceServices.
