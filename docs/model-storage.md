# Model storage

LLMKube downloads each model's GGUF once and serves it from a PersistentVolumeClaim mounted into the inference pod. How that cache is provisioned is controlled by `modelCache.mode`.

## Modes

### `perService` (default)

Each InferenceService gets its own cache PVC (`<inferenceservice>-model-cache`), created by the operator in the InferenceService's namespace:

- **RWO**, no explicit StorageClass, so it binds `WaitForFirstConsumer` — on whatever node the inference pod is scheduled to.
- The pod's **init container downloads the model into that PVC**, so the download and the server land on the same node by construction.
- Owner-referenced to the InferenceService, so it is garbage-collected when the service is deleted.

This is what makes **heterogeneous, multi-node clusters work**: an InferenceService whose accelerator is on node B (e.g. an AMD/Vulkan node distinct from the operator's node) schedules on node B and caches its model there. A single shared cache cannot do this — an RWO hostpath PVC is pinned to one node, so a GPU on any other node hits a `volume node affinity conflict` (see #728).

Trade-off: models are **not deduplicated across InferenceServices** — each service downloads and stores its own copy. For a cluster running many services off the same model on one node, that is more storage and more downloads. Use `shared` if you have RWX storage and want dedup.

### `shared`

A single cluster-wide cache PVC (`llmkube-model-cache`) that every inference pod mounts. This is the pre-0.9 behavior and is best paired with **ReadWriteMany** storage (NFS, CephFS, etc.) so all nodes can mount it:

```yaml
modelCache:
  mode: shared
  accessMode: ReadWriteMany
  storageClass: <your-rwx-class>
```

With an RWX backend, `shared` gives cross-service dedup and works multi-node. With an RWO backend it pins all inference to one node (single-node clusters only).

## Metadata

The operator reads GGUF metadata (architecture, layer count, context length, etc.) for `Model.Status` by reading **only the file header** over HTTP range requests — it never downloads the whole model itself. The full model bytes are fetched only by the per-service init container, on the serving node. `pvc://` and HuggingFace-repo sources are resolved at pod runtime and are unaffected.

## Migration

Upgrading from a release that used the shared cache: the default is now `perService`. To keep the previous single-cache behavior, set `modelCache.mode: shared` (and an RWX `accessMode`/`storageClass` if you run multi-node).
