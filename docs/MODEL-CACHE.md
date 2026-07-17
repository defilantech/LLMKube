# Persistent Model Cache

LLMKube includes a persistent model cache that avoids re-downloading models when InferenceServices are deleted and recreated.

## Overview

Without persistent caching, models are downloaded via init container every time a pod starts. For large models (13B-70B), this means 26-40GB+ downloads taking 10-30+ minutes each time you recreate a deployment.

With persistent caching:
- A PVC is created **automatically** in each namespace where you deploy models
- Models are downloaded **once** to the namespace's PVC
- Subsequent pods mount the cache and skip download
- Delete/recreate cycles complete in seconds

## Architecture

LLMKube uses **per-namespace PVCs** for model caching. This provides:
- **Namespace isolation**: Each namespace has its own cache
- **No cross-namespace dependencies**: Models work independently
- **Simple RBAC**: No need for cross-namespace access

```
┌─────────────────────────────────────────────────────────────┐
│                 Namespace: production                        │
│  ┌─────────────────────────────────────────────────────┐    │
│  │          llmkube-model-cache PVC                     │    │
│  │  /models/<cache-key>/model.gguf                     │    │
│  └─────────────────────────────────────────────────────┘    │
│           ▲                              ▲                   │
│           │ (init container writes)      │ (read-only)       │
│           │                              │                   │
│  ┌────────┴────────┐        ┌────────────┴──────────────┐   │
│  │  First Pod      │        │  Subsequent Pods          │   │
│  │  - Downloads    │        │  - Mount cache read-only  │   │
│  │  - Caches model │        │  - Skip download          │   │
│  └─────────────────┘        └───────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│                 Namespace: staging                           │
│  ┌─────────────────────────────────────────────────────┐    │
│  │          llmkube-model-cache PVC                     │    │
│  │  /models/<cache-key>/model.gguf                     │    │
│  └─────────────────────────────────────────────────────┘    │
│                          ▲                                   │
│                          │                                   │
│                 ┌────────┴────────┐                         │
│                 │  Pods in staging │                         │
│                 └─────────────────┘                         │
└─────────────────────────────────────────────────────────────┘
```

## Deploying to Any Namespace

You can deploy models to any namespace using the CLI:

```bash
# Deploy to production namespace
llmkube deploy llama-3.1-8b --gpu -n production

# Deploy to staging namespace
llmkube deploy llama-3.1-8b --gpu -n staging

# Deploy to default namespace
llmkube deploy llama-3.1-8b --gpu
```

The controller will automatically:
1. Create a `llmkube-model-cache` PVC in the target namespace (if it doesn't exist)
2. Configure the pod's init-container to download the model to the PVC
3. Mount the PVC read-only for the main container

**Note**: Each namespace has its own PVC, so the same model deployed to multiple namespaces will be downloaded once per namespace.

## Cache Key

Models are cached using a SHA256 hash of the source URL (first 16 characters). This means:
- Models with the same source URL share cache entries
- Changing the source URL creates a new cache entry
- The cache key is stored in `Model.Status.CacheKey`

Example:
```
Source: https://huggingface.co/TheBloke/Llama-2-7B-GGUF/resolve/main/llama-2-7b.Q4_K_M.gguf
Cache Key: a3b8c9d4e5f67890
Path: /models/a3b8c9d4e5f67890/model.gguf
```

## Configuration

### Helm Values

```yaml
modelCache:
  # Enable persistent model cache (default: true)
  enabled: true

  # Provisioning mode: shared or perService.
  # shared (default): a single cluster-wide PVC that all InferenceServices share.
  # perService: operator-managed per-InferenceService PVC that binds on the node
  #   where the inference pod schedules. A separate user-managed override is
  #   available via spec.modelCache.claimName on individual InferenceServices.
  mode: shared

  # Storage size for model cache
  size: 100Gi

  # Storage class (leave empty for default)
  storageClass: ""

  # Access mode
  # - ReadWriteOnce: Single-node clusters
  # - ReadWriteMany: Multi-node clusters (requires NFS, EFS, etc.)
  accessMode: ReadWriteOnce

  # Mount path inside controller pod
  mountPath: /models

  # PVC annotations (e.g., for backup policies)
  annotations: {}
```

### Multi-Node Clusters

For multi-node clusters where pods may run on different nodes, you need a storage class that supports `ReadWriteMany`:

**AWS EKS (EFS):**
```yaml
modelCache:
  storageClass: efs-sc
  accessMode: ReadWriteMany
```

**GKE (Filestore):**
```yaml
modelCache:
  storageClass: filestore-standard
  accessMode: ReadWriteMany
```

**Azure AKS (Azure Files):**
```yaml
modelCache:
  storageClass: azurefile-premium
  accessMode: ReadWriteMany
```

**On-Premise (NFS):**
```yaml
modelCache:
  storageClass: nfs-client
  accessMode: ReadWriteMany
```

#### Strictly tainted GPU nodes

When a GPU node carries a hard `NoSchedule` taint, dynamic provisioning can fail for reasons unrelated to the inference pod's tolerations. The safest approach is to pre-provision a PVC bound to the tainted node and reference it through `spec.modelCache.claimName`.

The inference pod already receives the GPU toleration derived from the Model's `hardware.gpu` configuration. The `model-downloader` runs as an init container in that same pod, so a pre-existing PVC does not require a separate download Job.

The claim must already exist and be node-aligned by the cluster administrator's storage setup. `claimName` is user-owned: LLMKube mounts it through the same prep and download init containers as the built-in cache, but never creates, mutates, or deletes it. If the named PVC is missing, the InferenceService is marked Degraded rather than silently falling back to the shared cache.

**Note:** `claimName` is ignored for `pvc://` model sources because those weights are already staged on the cluster and mounted read-only; no download occurs.

Some dynamic provisioners create a per-node helper pod to provision volumes. This helper pod is separate from the LLMKube inference pod and typically carries only the default not-ready/unreachable tolerations. When a GPU node has a hard `NoSchedule` taint, the helper pod may remain Pending with an untolerated-taint event — even though the inference pod itself has the correct GPU toleration.

Strict-taint choices:

- Pre-provision a static, node-aligned claim and use `claimName`.
- Use a provisioner whose helper configuration supports the taint.
- Apply a narrowly scoped cluster-level policy maintained by the cluster administrator.

### Per-InferenceService Cache PVC (Bring Your Own)

The cache backend above is an operator-global choice. To point a *single*
InferenceService at its own pre-existing, user-owned PVC — for example a
node-local volume for a large model pinned to one node, while everything else
rides the shared cache — set `spec.modelCache.claimName`:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-3.1-70b
spec:
  modelRef: llama-3.1-70b
  modelCache:
    claimName: my-model-cache   # pre-existing PVC in the same namespace
```

Behavior:

- The named PVC becomes the writable model cache for this workload only: the
  same `model-cache-prep` and `model-downloader` init containers run against
  it, weights land under the usual `<cacheKey>/` subdirectory, and the serving
  container mounts it read-only. `RefreshPolicy` and cache-key semantics are
  unchanged, so multiple models can safely share one claim.
- The operator **never creates or deletes** the claim — you own it end-to-end
  (unlike `perService` mode, where the operator provisions and
  garbage-collects `<isvc>-model-cache`). If the claim does not exist, the
  InferenceService is marked `Degraded` with a `ModelCachePVCNotFound` event
  instead of silently falling back to the shared cache.
- `claimName` targets the download path, so it is ignored — with a
  `ModelCacheClaimIgnored` warning event — for pre-staged `pvc://` model
  sources (mounted read-only, no download), and whenever caching is inactive
  (chart `modelCache.enabled: false`, a local `file://` source, or a remote
  model not yet fingerprinted); in the inactive-caching cases the pod falls
  back to an ephemeral emptyDir and re-downloads on every restart.
- `claimName` is mutable: changing it rolls the Deployment onto the new claim,
  but the old claim (and the weights on it) is **not** garbage-collected —
  clean it up yourself if it is no longer needed.
- Node alignment is your responsibility: for an RWO or node-local claim, use
  `nodeSelector` so the pod lands where the PVC binds (a
  `WaitForFirstConsumer` local class binds on the first consumer; a pre-bound
  RWO PVC pins the pod).
- `llmkube cache list` discovers the shared cache and operator-managed
  per-service cache PVCs. A user-managed `spec.modelCache.claimName` PVC is
  outside the operator's cache label/discovery contract and may not appear in
  the listing. Cache inspection may need a running pod or a transient inspector
  pod for Pending `WaitForFirstConsumer` claims.

## CLI Commands

### List Cached Models

```bash
# List cached models in default namespace
llmkube cache list

# List from all namespaces
llmkube cache list -A
```

Output:
```
Model Cache Entries
═══════════════════════════════════════════════════════════════════════════════
CACHE KEY         SIZE      MODELS              SOURCE
a3b8c9d4e5f67890  4.1 GiB   llama-2-7b          ...TheBloke/Llama-2-7B-GGUF/...
f1c314277254a2fd  7.2 GiB   llama-3.1-8b        ...meta-llama/Meta-Llama-3.1-8B/...

Total: 2 cache entries, 2 models
```

### Clear Cache

```bash
# Clear cache for a specific model
llmkube cache clear --model llama-2-7b

# Clear all cache (with confirmation)
llmkube cache clear

# Force clear without confirmation
llmkube cache clear --force
```

### Preload Models

Pre-download models before deploying them:

```bash
# Preload a catalog model
llmkube cache preload llama-3.1-8b

# Preload to a specific namespace
llmkube cache preload llama-3.1-8b -n production
```

This is useful for:
- Air-gapped environments (pre-populate cache on a connected machine)
- Reducing deployment time (model already cached)
- Bandwidth management (download during off-peak hours)

## Air-Gapped Deployments

For air-gapped environments:

1. **On a connected machine**, preload models:
   ```bash
   llmkube cache preload llama-3.1-8b
   llmkube cache preload mistral-7b
   ```

2. **Export the PVC** (or use external storage):
   ```bash
   # Option 1: Copy from PVC to local storage
   kubectl cp llmkube-system/llmkube-controller-manager:/models ./model-cache

   # Option 2: Use a storage system that can be transported
   ```

3. **On the air-gapped cluster**, import the cache:
   ```bash
   # Copy to the new PVC
   kubectl cp ./model-cache llmkube-system/llmkube-controller-manager:/models
   ```

4. **Deploy models** (they'll be found in cache):
   ```bash
   llmkube deploy llama-3.1-8b --gpu
   ```

## Troubleshooting

### Model Not Using Cache

If models are still being downloaded via init container:

1. Check if the Model has a CacheKey:
   ```bash
   kubectl get model llama-3.1-8b -n <namespace> -o jsonpath='{.status.cacheKey}'
   ```

2. Verify the controller has cache enabled:
   ```bash
   kubectl get deploy -n llmkube-system llmkube-controller-manager -o yaml | grep model-cache
   ```

3. Check PVC exists in your namespace:
   ```bash
   kubectl get pvc llmkube-model-cache -n <namespace>
   ```

4. Check if model is cached in the PVC:
   ```bash
   kubectl exec -n <namespace> <pod-name> -- ls -la /models/
   ```

### Cache PVC Full

If the cache PVC runs out of space:

1. List cache entries:
   ```bash
   llmkube cache list -n <namespace>
   ```

2. Clear unused models:
   ```bash
   llmkube cache clear --model <unused-model> -n <namespace>
   ```

3. Or resize the PVC (if your storage class supports it):
   ```bash
   kubectl patch pvc llmkube-model-cache -n <namespace> \
     -p '{"spec":{"resources":{"requests":{"storage":"200Gi"}}}}'
   ```

### Cache Corruption

If you suspect cache corruption:

1. Clear the specific cache entry by deleting the directory in the PVC:
   ```bash
   # Find a pod in the namespace to exec into
   kubectl exec -n <namespace> <pod-name> -- rm -rf /models/<cache-key>
   ```

2. Delete and recreate the InferenceService to trigger re-download:
   ```bash
   kubectl delete inferenceservice llama-3.1-8b -n <namespace>
   kubectl apply -f inferenceservice.yaml
   ```

   Or delete and recreate the Model:
   ```bash
   kubectl delete model llama-3.1-8b -n <namespace>
   kubectl apply -f model.yaml
   ```

## Performance Considerations

- **Storage Performance**: Use SSD-backed storage for faster model loading
- **Network**: For ReadWriteMany, ensure low-latency network between nodes and storage
- **Cache Size**: Plan for 1.5-2x your total model sizes to allow for cache rotation

## Disabling Cache

To disable persistent caching (not recommended):

```yaml
# values.yaml
modelCache:
  enabled: false
```

This will revert to the legacy behavior where each pod downloads the model via init container.

## Security Considerations

### Local and hostPath model sources (host-path allowlist)

Besides remote downloads, the model storage machinery can serve node-local sources: a Model whose `spec.source` is an absolute path (`/srv/models/model.gguf`) or a `file://` URL is mounted into the inference pod via a hostPath volume. As of v0.9.0, these sources are gated by an operator-configured allowlist ([GHSA-jw3m-8q7m-f35r](https://github.com/defilantech/LLMKube/security/advisories/GHSA-jw3m-8q7m-f35r)).

The allowlist is **empty by default**, which disables local and hostPath sources entirely. With the default configuration, a Model with a local source is rejected: it goes to phase `Failed` with a `SourceNotAllowed` condition, nothing is fetched, and no hostPath volume is created. Remote sources (`https://`, `pvc://`, `hf://`) are unaffected.

To keep serving models from node-local disk, list the absolute directory roots you trust:

```yaml
# values.yaml
modelSource:
  allowedHostPathRoots:
    - /srv/models
```

The equivalent controller flag is `--allowed-host-path-roots` (comma-separated). A source is permitted only when its cleaned path is within one of the configured roots; `..` escapes are rejected. The check is lexical and does not resolve symlinks, so only allowlist roots whose contents you control.

### Automatic shared cache on `fsGroupPolicy: None` backends (CephFS, NFS)

The automatic shared-model-cache workflow is a first-class supported path, **including** on `fsGroupPolicy: None` CSIs such as CephFS and NFS. On those backends Kubernetes does not apply the pod `fsGroup` to the volume, so the PVC root stays `root:root` and the non-root model downloader cannot write to it. The `model-cache-prep` init container fixes the ownership for you so the cache just works. You do not need to pre-stage models or hand-manage a PVC.

The prep is the **compatibility implementation** for these backends, and it runs as **non-privileged root (uid 0)** with `ALL` capabilities dropped and only `CHOWN`+`FOWNER` added (no privilege escalation, read-only rootfs, seccomp `RuntimeDefault`). Root here is a backend-driven requirement, not an avoidable misconfiguration: chowning a root-owned mount needs `CAP_CHOWN`, the prep shells out to `chown`, and a non-root process loses its capabilities across `execve` (Kubernetes has no ambient-capability field), so the exec'd `chown` would fail `EPERM`. Root retains its capabilities across exec. (Running the prep non-root broke the shared cache on these backends in 0.8.20; reverted in 0.8.21. A future change may move the chown into a small dedicated helper that performs the syscall in its own entrypoint, which would let the prep run non-root again without the exec capability loss.)

### PSA `restricted`

Because the prep needs root plus `CHOWN`/`FOWNER`, it cannot satisfy Pod Security Admission `restricted` (which forbids running as root and adding any capability except `NET_BIND_SERVICE`). This is a property of the storage backend, not of the cache feature: it applies only in the narrow intersection of an `fsGroupPolicy: None` backend AND a namespace that enforces `restricted`. If you are in exactly that case, the options are:

- Use an `fsGroupPolicy: File` CSI so Kubernetes applies ownership itself and no prep is needed.
- Use an `emptyDir` model store (no shared persistent cache).
- Relax the namespace policy to `baseline`.

### Shared-cache group-write multi-tenancy

In `shared` mode, the cache-prep init container sets `chmod g+rwX /models`, making the cache root writable by any pod in the same `fsGroup`. On a shared cache PVC, multiple InferenceServices share that `fsGroup` and can write to each other's cached model files.

Within one operator's trust domain this is fine. For a multi-tenant cache where distrusting tenants share the same namespace, this is a real consideration. Use per-service caches (`modelCache.mode: perService`) for isolation between distrusting tenants.
