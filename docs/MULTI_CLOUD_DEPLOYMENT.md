# Multi-Cloud Deployment Guide

LLMKube is designed to work across any Kubernetes environment:
- **Cloud**: GKE, AKS, EKS, and others
- **On-Premises**: Bare metal, OpenShift, Rancher
- **Local**: Minikube, K3s, MicroK8s, Kind

## Cloud-Agnostic Design

The LLMKube controller uses **only standard Kubernetes APIs** for GPU workloads:

1. **GPU Resource Requests**: `nvidia.com/gpu: 2`
   - Works on all clouds with NVIDIA GPU Operator
   - Kubernetes automatically schedules to nodes with available GPUs

2. **Base GPU Toleration**: `nvidia.com/gpu=present:NoSchedule`
   - Standard toleration for GPU node taints
   - Applied automatically by the controller

3. **No Hardcoded Cloud Logic**: No assumptions about node pools, regions, or cloud providers

## Cloud-Specific Configuration

For cloud-specific features (spot instances, node pools), use the **optional** `tolerations` and `nodeSelector` fields:

### Azure AKS with Spot Instances

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-service
spec:
  modelRef: llama-13b
  resources:
    gpu: 2

  # Azure spot instance toleration
  tolerations:
    - key: kubernetes.azure.com/scalesetpriority
      operator: Equal
      value: spot
      effect: NoSchedule

  # Optional: target specific node pool
  nodeSelector:
    agentpool: gpupool
```

### Google GKE with Spot VMs

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-service
spec:
  modelRef: llama-13b
  resources:
    gpu: 2

  # GKE spot instance toleration
  tolerations:
    - key: cloud.google.com/gke-spot
      operator: Equal
      value: "true"
      effect: NoSchedule

  # Optional: target specific node pool
  nodeSelector:
    cloud.google.com/gke-nodepool: gpu-pool
```

### AWS EKS with Spot Instances

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-service
spec:
  modelRef: llama-13b
  resources:
    gpu: 2

  # EKS spot instance toleration
  tolerations:
    - key: spotInstance
      operator: Equal
      value: "true"
      effect: NoSchedule

  # Optional: target specific node group
  nodeSelector:
    eks.amazonaws.com/nodegroup: gpu-spot-nodes
```

### Bare Metal / Generic Kubernetes

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-service
spec:
  modelRef: llama-13b
  resources:
    gpu: 2

  # No cloud-specific configuration needed!
  # The controller automatically adds nvidia.com/gpu toleration
```

## How It Works

1. **Controller adds base NVIDIA toleration** automatically for any GPU workload
2. **User-provided tolerations are merged** with the base toleration
3. **User-provided node selectors** are applied if specified
4. **Kubernetes scheduler** finds nodes with:
   - Available GPUs matching the resource request
   - Labels matching the node selector (if specified)
   - Taints that are tolerated by the pod

## Examples

See the `config/samples/` directory for complete examples:
- `multi-gpu-llama-13b-model.yaml` - Generic cloud-agnostic example
- `multi-gpu-azure-spot.yaml` - Azure AKS with spot instances
- `multi-gpu-gke-spot.yaml` - Google GKE with spot VMs
- `multi-gpu-eks-spot.yaml` - AWS EKS with spot instances

## Testing Across Clouds

The multi-GPU implementation has been validated on:
- ✅ **Bare metal** (K3s with 2x NVIDIA GPUs)
- ✅ **Azure AKS** (Standard_NC12s_v3 with 2x V100)
- ✅ **Google GKE** (n1-standard-8 with 2x T4)
- ✅ **AWS EKS** (g4dn.12xlarge with 4x T4)

## Troubleshooting

### Pod stuck in Pending

1. Check GPU availability:
   ```bash
   kubectl get nodes -o custom-columns=NAME:.metadata.name,GPU:.status.allocatable."nvidia\.com/gpu"
   ```

2. Check pod events:
   ```bash
   kubectl describe pod <pod-name>
   ```

3. Common issues:
   - **"Insufficient nvidia.com/gpu"** - No GPU nodes available or GPUs are in use
   - **"Untolerated taint"** - Missing toleration for cloud-specific taints
   - **"Node affinity/selector"** - Node selector doesn't match any nodes

### Cloud-Specific Taints

Each cloud provider may add different taints to GPU nodes:

| Cloud | Taint Key | Value | Purpose |
|-------|-----------|-------|---------|
| Azure Spot | `kubernetes.azure.com/scalesetpriority` | `spot` | Spot instances |
| GKE Spot | `cloud.google.com/gke-spot` | `true` | Preemptible VMs |
| GKE GPU | `nvidia.com/gpu` | `present` | GPU nodes (added by LLMKube) |
| EKS Spot | `spotInstance` | `true` | Spot instances |

Use `kubectl describe node <node-name>` to see what taints are present on your GPU nodes.
