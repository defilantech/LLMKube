---
title: Model cache and node-local storage
description: Choose shared, per-service, or pre-provisioned model storage, including strict-taint GPU nodes.
---

# Model cache and node-local storage

LLMKube downloads a remote Model through the `model-downloader` init
container in the inference pod. Persistent caching controls which PVC that
init container and the serving container mount.

## Cache modes

| Cluster/storage topology | Configuration |
| --- | --- |
| Single node | `shared` with the default RWO storage |
| Multi-node with RWX | `shared` with `accessMode: ReadWriteMany` and an RWX StorageClass |
| Multi-node without RWX, compatible topology-aware provisioner | `perService` |
| Strictly tainted node with unsuitable dynamic provisioner | Pre-provision a node-aligned PVC and set `spec.modelCache.claimName` |

`shared` is the default. A single cluster-wide PVC named `llmkube-model-cache`
is created (or reused if it already exists). When using an RWO storage class,
the shared claim binds to one node, so all inference pods scheduling onto that
PVC must land on the same node.

`perService` creates a dedicated PVC named `<inferenceservice>-model-cache` for
each InferenceService. Each per-service PVC uses RWO semantics and relies on the
StorageClass's `WaitForFirstConsumer` volume binding behavior to bind on the
node where the inference pod schedules. This avoids cross-node shared-RWO
affinity but does not affect external provisioner helper pods (see below).

## Strictly tainted GPU nodes

When a GPU node carries a hard `NoSchedule` taint, dynamic provisioning can
fail for reasons unrelated to the inference pod's tolerations. The safest
approach is to pre-provision a PVC bound to the tainted node and reference it
through `spec.modelCache.claimName`.

The inference pod already receives the GPU toleration derived from the Model's
`hardware.gpu` configuration. The `model-downloader` runs as an init container
in that same pod, so a pre-existing PVC does not require a separate download
Job.

The claim must already exist and be node-aligned by the cluster administrator's
storage setup. `claimName` is user-owned: LLMKube mounts it through the same
prep and download init containers as the built-in cache, but never creates,
mutates, or deletes it. If the named PVC is missing, the InferenceService is
marked Degraded rather than silently falling back to the shared cache.

Example with an AMD GPU node:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: amd-vulkan-model
spec:
  source: https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q4_K_M.gguf
  format: gguf
  hardware:
    gpu:
      enabled: true
      count: 1
      vendor: amd
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: amd-vulkan-service
spec:
  modelRef: amd-vulkan-model
  modelCache:
    claimName: gpu-node-model-cache
  resources:
    gpu: 1
```

The `source` URL above is an example. Replace it with an allowlisted, reachable
model source for your environment. This manifest does not create the PVC — the
PVC named `gpu-node-model-cache` must be pre-provisioned by the cluster
administrator and bound to a node that matches the GPU taint topology.

**Note:** `claimName` is ignored for `pvc://` model sources because those
weights are already staged on the cluster and mounted read-only; no download
occurs.

### Verification

After applying the Model and InferenceService, confirm the PVC is bound and
the inference pod lands on the expected node:

```bash
kubectl get pvc gpu-node-model-cache
kubectl get pod -l app=amd-vulkan-service \
  -o custom-columns=NAME:.metadata.name,NODE:.spec.nodeName
kubectl describe pod -l app=amd-vulkan-service
```

The PVC should show `Bound`, the pod should report the tainted GPU node, and
`describe` should list the correct toleration and volume mount.

### Why a tolerated inference pod may still have a Pending PVC

Some dynamic provisioners create a per-node helper pod to provision volumes.
For example, MicroK8s hostpath deploys a `hostpath-provisioner-<node>-*` pod
on each node. This helper pod is separate from the LLMKube inference pod and
typically carries only the default not-ready/unreachable tolerations.

When a GPU node has a hard `NoSchedule` taint, the helper pod may remain
Pending with an untolerated-taint event — even though the inference pod itself
has the correct GPU toleration. A PVC has no pod `nodeSelector` or
`tolerations` field for LLMKube to set, and LLMKube does not manage the
external helper pod.

Strict-taint choices:

- Pre-provision a static, node-aligned claim and use `claimName`.
- Use a provisioner whose helper configuration supports the taint.
- Apply a narrowly scoped cluster-level policy maintained by the cluster
  administrator.

Global `perService` mode only avoids cross-node shared-RWO affinity. It cannot
make an untolerated external helper pod schedule on a tainted node.

## Troubleshooting

### Pending PVC with `hostpath-provisioner-<node>-*` showing `untolerated taint`

The external provisioner's helper pod cannot schedule on the tainted node.
This is a limitation of the provisioner, not LLMKube. Use one of the
strict-taint choices above: pre-provision a static claim with `claimName`,
switch to a provisioner whose helpers tolerate the taint, or apply a
cluster-level policy.

### `volume node affinity conflict`

A shared RWO PVC is bound to a different node than where the inference pod is
trying to schedule. Options:

- Use an RWX StorageClass with the `shared` mode.
- Switch to `perService` so each InferenceService gets its own RWO PVC that
  binds via `WaitForFirstConsumer`.
- Pre-provision a node-aligned PVC and reference it with `claimName`.

### Cache inspection with `llmkube cache list`

The operator-managed shared and labeled per-service cache PVCs are discoverable
through the `app.kubernetes.io/component=model-cache` label. A user-managed
`claimName` PVC is not necessarily labeled as an operator cache; it remains
the user's responsibility to manage its lifecycle.

### Relaxing the taint

As an alternative to pre-provisioning, changing a GPU node's taint from
`NoSchedule` to `PreferNoSchedule` allows both the inference pod and the
external provisioner's helper pod to schedule with a scheduling penalty rather
than a hard block. This is a cluster-level trade-off: it increases scheduling
flexibility but reduces the guarantee that only tolerated workloads land on
GPU nodes.
