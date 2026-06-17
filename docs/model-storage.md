# Model storage

LLMKube serves each model's GGUF from a PersistentVolumeClaim mounted into the inference pod. How that cache is provisioned is controlled by `modelCache.mode`.

## Modes

### `shared` (default)

A single cluster-wide cache PVC (`llmkube-model-cache`) that the operator mounts and every inference pod shares, created by the operator in the InferenceService's namespace:

- The pod's **init container downloads each remote model into the shared PVC**, so all InferenceServices reuse one cache (**cross-service dedup**: a model is downloaded once and reused by every service that references it).
- `llmkube cache list` inspects this PVC, so cache inspection works out of the box.
- This is the proven default and is a drop-in for existing single-node clusters.

On a **multi-node** cluster, pair `shared` with **ReadWriteMany** storage (NFS, CephFS, EFS, etc.) so the cache is reachable from any node:

```yaml
modelCache:
  mode: shared
  accessMode: ReadWriteMany
  storageClass: <your-rwx-class>
```

With the default RWO storage class the shared PVC is pinned to one node, so it only works single-node (a GPU on any other node would hit a `volume node affinity conflict`). If your multi-node cluster has no RWX storage class, use `perService` instead.

### `perService` (opt-in escape hatch)

For multi-node clusters **without** an RWX storage class. Each InferenceService gets its own cache PVC (`<inferenceservice>-model-cache`):

- **RWO**, no explicit StorageClass, so it binds `WaitForFirstConsumer` — on whatever node the inference pod is scheduled to.
- The pod's **init container downloads the model into that PVC**, so the download and the server land on the same node by construction.
- Owner-referenced to the InferenceService, so it is garbage-collected when the service is deleted.

This makes **heterogeneous, multi-node clusters work without RWX**: an InferenceService whose accelerator is on node B (e.g. an AMD/Vulkan node distinct from the operator's node) schedules on node B and caches its model there (see #728).

```yaml
modelCache:
  mode: perService
```

Trade-offs: models are **not deduplicated across InferenceServices** (each service downloads and stores its own copy), and `llmkube cache list` is not per-isvc cache aware yet. Prefer `shared` + an RWX storage class on multi-node clusters that have one.

## Choosing a mode

| Cluster | Recommendation |
| --- | --- |
| Single-node | `shared` (default) |
| Multi-node with an RWX storage class | `shared` + `accessMode: ReadWriteMany` + `storageClass: <rwx-class>` |
| Multi-node without RWX | `perService` |

## Metadata

In `perService` mode the operator reads GGUF metadata (architecture, layer count, context length, etc.) for `Model.Status` by reading **only the file header** over HTTP range requests — it never downloads the whole model itself. The full model bytes are fetched only by the init container, on the serving node. `pvc://` and HuggingFace-repo sources are resolved at pod runtime and are unaffected.
