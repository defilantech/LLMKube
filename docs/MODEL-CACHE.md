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
