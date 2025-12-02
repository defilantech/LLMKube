# Air-Gapped Deployment Guide

Deploy LLMKube in environments without internet access. This guide covers deploying models from local file paths, private registries, and pre-downloaded GGUF files.

## Use Cases

- **Government/Defense**: Classified networks with no internet access
- **Healthcare**: HIPAA-compliant environments with restricted egress
- **Finance**: Air-gapped trading systems and compliance environments
- **Edge**: Remote locations with limited or no connectivity
- **Corporate**: Private networks with strict firewall rules

## Prerequisites

- Kubernetes cluster (v1.11.3+) with no internet access
- LLMKube operator installed (see [offline installation](#offline-operator-installation))
- Pre-downloaded GGUF model file(s)
- `llmkube` CLI installed on a workstation with cluster access

## Quick Start: Deploy from Local Path

### Step 1: Pre-download the Model

On a machine with internet access, download the GGUF file:

```bash
# Example: Download Llama 3.1 8B
curl -L -o llama-3.1-8b-q4_k_m.gguf \
  "https://huggingface.co/bartowski/Meta-Llama-3.1-8B-Instruct-GGUF/resolve/main/Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf"

# Verify the file
ls -lh llama-3.1-8b-q4_k_m.gguf
# Should show ~4.9GB
```

### Step 2: Transfer to Air-Gapped Environment

Transfer the GGUF file to a location accessible by your Kubernetes nodes:

```bash
# Option A: Copy to a shared NFS mount
cp llama-3.1-8b-q4_k_m.gguf /mnt/nfs/models/

# Option B: Copy to each node (if no shared storage)
scp llama-3.1-8b-q4_k_m.gguf node1:/mnt/models/
scp llama-3.1-8b-q4_k_m.gguf node2:/mnt/models/

# Option C: Use a PersistentVolume
# (see PVC-based deployment below)
```

### Step 3: Deploy with CLI

```bash
# Deploy using local path
llmkube deploy my-llama --gpu \
  --source /mnt/models/llama-3.1-8b-q4_k_m.gguf \
  --cpu 4 \
  --memory 8Gi \
  --gpu-layers 32

# Or use catalog defaults with local override
llmkube deploy llama-3.1-8b --gpu \
  --source-override /mnt/models/llama-3.1-8b-q4_k_m.gguf
```

### Step 4: Verify Deployment

```bash
# Check model status (should show "Copying" then "Ready")
llmkube list models

# Check service status
llmkube list services

# Test the endpoint
kubectl port-forward svc/my-llama 8080:8080 &
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"Hello!"}]}'
```

## Deployment Options

### Option 1: Absolute File Path

The simplest approach for nodes with local model storage:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: local-llama
spec:
  source: /mnt/models/llama-3.1-8b-q4_k_m.gguf
  format: gguf
  hardware:
    accelerator: cuda
    gpu:
      enabled: true
      count: 1
```

**Requirements:**
- Model file must exist at the same path on all nodes where pods may run
- Use a DaemonSet or node affinity to ensure pods land on nodes with the model

### Option 2: file:// URL

Equivalent to absolute path, but explicit about the scheme:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: local-llama
spec:
  source: file:///mnt/models/llama-3.1-8b-q4_k_m.gguf
  format: gguf
```

### Option 3: Private HTTP Server

For environments with an internal model server:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: internal-llama
spec:
  source: http://model-server.internal.corp:8080/models/llama-3.1-8b-q4_k_m.gguf
  format: gguf
```

**Setup a simple model server:**
```bash
# On your internal server
cd /path/to/models
python3 -m http.server 8080
```

## Offline Operator Installation

### Option 1: Pre-built Container Images

1. On a machine with internet access, pull and save the images:

```bash
# Pull images
docker pull ghcr.io/defilantech/llmkube:v0.4.9
docker pull ghcr.io/ggerganov/llama.cpp:server-cuda

# Save to tar files
docker save ghcr.io/defilantech/llmkube:v0.4.9 > llmkube-controller.tar
docker save ghcr.io/ggerganov/llama.cpp:server-cuda > llama-server-cuda.tar
```

2. Transfer tar files to the air-gapped environment

3. Load images on each node or into your private registry:

```bash
# Load directly on nodes
docker load < llmkube-controller.tar
docker load < llama-server-cuda.tar

# Or push to private registry
docker load < llmkube-controller.tar
docker tag ghcr.io/defilantech/llmkube:v0.4.9 registry.internal/llmkube:v0.4.9
docker push registry.internal/llmkube:v0.4.9
```

### Option 2: Helm with Private Registry

```bash
# Add Helm repo (on connected machine)
helm repo add llmkube https://defilantech.github.io/LLMKube
helm pull llmkube/llmkube --untar

# Transfer chart to air-gapped environment, then install:
helm install llmkube ./llmkube \
  --namespace llmkube-system --create-namespace \
  --set image.repository=registry.internal/llmkube \
  --set image.tag=v0.4.9
```

## CLI Commands for Air-Gapped Deployments

```bash
# Deploy from local file
llmkube deploy my-model --gpu \
  --source /mnt/models/model.gguf

# Deploy catalog model with local file override
llmkube deploy llama-3.1-8b --gpu \
  --source-override /mnt/models/llama-3.1-8b-q4_k_m.gguf

# Deploy from file:// URL
llmkube deploy my-model --gpu \
  --source file:///mnt/models/model.gguf

# Deploy from internal HTTP server
llmkube deploy my-model --gpu \
  --source http://model-server.internal:8080/model.gguf
```

## Storage Strategies

### Shared Storage (NFS/GlusterFS)

Best for multi-node clusters:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: model-storage
spec:
  capacity:
    storage: 100Gi
  accessModes:
    - ReadOnlyMany
  nfs:
    server: nfs.internal
    path: /exports/models
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: model-storage
spec:
  accessModes:
    - ReadOnlyMany
  resources:
    requests:
      storage: 100Gi
```

### Local Storage with Node Affinity

For single-node or node-specific deployments:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: local-llama
spec:
  modelRef: local-llama
  replicas: 1
  # Add node selector to ensure pod lands on node with model
  # (configure via Deployment after creation)
```

## Troubleshooting

### Model Status Shows "Failed"

```bash
# Check model status
kubectl describe model my-model

# Common issues:
# - "file does not exist" - Model path is incorrect or not accessible
# - "permission denied" - File permissions issue
# - "copy incomplete" - Disk space or I/O error
```

### File Not Found on Node

```bash
# Verify file exists on the node
kubectl debug node/NODE_NAME -it --image=busybox -- ls -la /mnt/models/

# Check if path is mounted in the pod
kubectl exec -it POD_NAME -- ls -la /models/
```

### Permission Denied

```bash
# Check file permissions (should be readable by container user)
ls -la /mnt/models/model.gguf

# Fix permissions
chmod 644 /mnt/models/model.gguf
```

## Security Considerations

1. **Model Integrity**: Verify SHA256 checksums of downloaded models before deployment
2. **File Permissions**: Restrict model file access to necessary users/groups
3. **Network Segmentation**: Ensure internal model servers are properly firewalled
4. **Audit Logging**: Track model deployments and access (see [SOC2 compliance](/docs/compliance.md))

## Next Steps

- [GPU Setup Guide](gpu-setup-guide.md) - Configure GPU acceleration
- [Model Cache Guide](MODEL-CACHE.md) - Manage cached models
- [Multi-GPU Deployment](MULTI-GPU-DEPLOYMENT.md) - Scale to multiple GPUs

## Support

- **Issues**: [GitHub Issues](https://github.com/defilantech/LLMKube/issues)
- **Documentation**: [README.md](../README.md)
