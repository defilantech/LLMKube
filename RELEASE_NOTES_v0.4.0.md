# LLMKube v0.4.0 Release Notes

**Release Date**: November 25, 2025
**Status**: Feature Release
**Codename**: "Multi-GPU"

## Overview

LLMKube v0.4.0 introduces multi-GPU support with layer-based sharding, enabling deployment of large language models that don't fit on a single GPU. This release also brings true multi-cloud support, removing hardcoded cloud-specific logic and enabling deployment across GKE, AKS, EKS, bare metal, and local Kubernetes environments.

**TL;DR**: Deploy 13B, 70B, and larger models across multiple GPUs with automatic layer sharding. Works on any cloud or bare metal Kubernetes cluster.

## New Features

### Multi-GPU Support with Layer-Based Sharding (Issue #2)

Deploy models larger than a single GPU's VRAM by splitting layers across multiple GPUs:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-70b
spec:
  modelRef:
    name: llama-70b-model
  accelerator:
    type: nvidia
    gpuCount: 4  # Split across 4 GPUs
```

**How it works:**
- Uses llama.cpp's `--split-mode layer` for efficient model distribution
- Automatic `--tensor-split` calculation for equal GPU utilization
- GPU count can be specified on either Model or InferenceService (InferenceService takes precedence)
- Single-GPU and CPU-only modes continue to work unchanged

**Verified Performance:**
- Llama 2 13B Q4_K_M on 2x RTX 5060 Ti: ~44 tok/s generation
- Both GPUs at 45-53% utilization during inference

**Multi-Model Benchmark (ShadowStack - Dual RTX 5060 Ti):**

| Model | Size | Gen tok/s | P50 Latency | P99 Latency |
|-------|------|-----------|-------------|-------------|
| Llama 3.2 3B | 3B | 53.3 | 1930ms | 2260ms |
| Mistral 7B v0.3 | 7B | 52.9 | 1912ms | 2071ms |
| Llama 3.1 8B | 8B | 52.5 | 1878ms | 2178ms |

*10 iterations, 256 max tokens per test*

### Multi-Cloud Support

The controller now works on any Kubernetes distribution without cloud-specific configuration:

**New API Fields** (`InferenceServiceSpec`):
- `tolerations` - Custom pod tolerations for spot/preemptible instances
- `nodeSelector` - Target specific node pools or instance types

**Cloud-Specific Examples Included:**
- `multi-gpu-azure-spot.yaml` - Azure AKS with spot instances
- `multi-gpu-gke-spot.yaml` - Google GKE with preemptible VMs
- `multi-gpu-eks-spot.yaml` - AWS EKS with spot instances
- `multi-gpu-llama-13b-model.yaml` - Cloud-agnostic (bare metal, Minikube, K3s)

**Example: Azure AKS with Spot Instances**
```yaml
spec:
  tolerations:
    - key: "kubernetes.azure.com/scalesetpriority"
      operator: "Equal"
      value: "spot"
      effect: "NoSchedule"
  nodeSelector:
    agentpool: gpupool
```

### Cloud Infrastructure (Terraform)

Complete Terraform modules for multi-GPU clusters:

**Azure AKS** (`terraform/azure/`):
- System + GPU node pools with auto-scaling
- Supports V100, T4, A100 GPU types
- Spot instance configuration (~80% cost savings)
- Interactive quick-start script

**AWS EKS** (`terraform/eks/`):
- GPU node groups with auto-scaling
- P-instance and G-instance support
- Spot instance configuration
- GPU quota request helper script

**GKE** (`terraform/gke/`):
- Updated multi-GPU quick-start script
- T4 and L4 GPU configurations

## Documentation

### Helm Repository on GitHub Pages

LLMKube now has a proper Helm repository:

```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update
helm install llmkube llmkube/llmkube --namespace llmkube-system --create-namespace
```

### New Documentation
- `docs/MULTI-GPU-DEPLOYMENT.md` - Step-by-step multi-GPU deployment guide
- `docs/MULTI_CLOUD_DEPLOYMENT.md` - Cloud-agnostic deployment principles
- `test/e2e/multi-gpu-test-plan.md` - Complete E2E test plan

## Bug Fixes

### Service Name DNS-1035 Compliance (from v0.3.3)

Model names with dots (e.g., `llama-3.1-8b`) now deploy correctly. Service names are automatically sanitized to replace dots with dashes.

## Technical Details

### API Changes

New optional fields in `InferenceServiceSpec`:

```go
// Tolerations for pod scheduling (optional)
// Allows targeting spot/preemptible instances or specialized node pools
Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

// NodeSelector for pod scheduling (optional)
// Allows targeting specific node pools across any cloud provider
NodeSelector map[string]string `json:"nodeSelector,omitempty"`
```

### Controller Changes

- Removed hardcoded GKE-specific node selector (`cloud.google.com/gke-nodepool`)
- Added automatic toleration merging (base NVIDIA GPU toleration + user-specified)
- Added `calculateTensorSplit()` for even GPU layer distribution
- Multi-GPU deployment construction with `--split-mode layer` and `--tensor-split` args

### Files Changed
- `api/v1alpha1/inferenceservice_types.go` - New tolerations/nodeSelector fields
- `api/v1alpha1/zz_generated.deepcopy.go` - Generated deep copy functions
- `config/crd/bases/inference.llmkube.dev_inferenceservices.yaml` - Updated CRD
- `internal/controller/inferenceservice_controller.go` - Multi-GPU and multi-cloud logic
- `internal/controller/inferenceservice_controller_test.go` - Comprehensive tests

### Testing

New test coverage for multi-GPU support:
- `calculateTensorSplit()` unit tests (0, 1, 2, 4, 8 GPUs)
- Multi-GPU deployment construction tests
- GPU count precedence tests (Model > InferenceService)
- Tolerations and NodeSelector propagation tests
- End-to-end reconciliation with multi-GPU config

## Installation & Upgrade

### Upgrading from v0.3.x

#### CLI Installation

**macOS (Homebrew):**
```bash
brew upgrade llmkube
```

**macOS (Apple Silicon):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.4.0/LLMKube_0.4.0_darwin_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**macOS (Intel):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.4.0/LLMKube_0.4.0_darwin_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Linux (amd64):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.4.0/LLMKube_0.4.0_linux_amd64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

**Linux (arm64):**
```bash
curl -L https://github.com/defilantech/LLMKube/releases/download/v0.4.0/LLMKube_0.4.0_linux_arm64.tar.gz | tar xz
sudo mv llmkube /usr/local/bin/
```

### Controller Upgrade

**Option 1: Helm (Recommended)**
```bash
helm repo add llmkube https://defilantech.github.io/LLMKube
helm repo update
helm upgrade llmkube llmkube/llmkube --namespace llmkube-system
```

**Option 2: Kustomize**
```bash
kubectl apply -k https://github.com/defilantech/LLMKube/config/default?ref=v0.4.0
```

## Quick Start: Multi-GPU Deployment

### 1. Deploy a Multi-GPU Model

```bash
# Create a Model with GPU count
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: llama-13b
spec:
  source:
    huggingFace:
      repo: TheBloke/Llama-2-13B-chat-GGUF
      filename: llama-2-13b-chat.Q4_K_M.gguf
  gpuCount: 2
EOF

# Create an InferenceService
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: llama-13b
spec:
  modelRef:
    name: llama-13b
  accelerator:
    type: nvidia
EOF
```

### 2. Verify Multi-GPU Usage

```bash
# Check pod GPU allocation
kubectl describe pod -l inference.llmkube.dev/service-name=llama-13b | grep -A5 "Limits:"

# Expected output:
#   nvidia.com/gpu: 2
```

### 3. Test Inference

```bash
kubectl port-forward svc/llama-13b 8080:8080

curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"Explain quantum computing in simple terms."}]}'
```

## Breaking Changes

None. This release is backward compatible with v0.3.x configurations.

## Full Changelog

### Features
- Multi-GPU support with layer-based sharding (#47, resolves #2)
- Multi-cloud deployment support (Azure, GKE, EKS, bare metal)
- New `tolerations` and `nodeSelector` fields in InferenceService API
- Azure AKS Terraform infrastructure with spot instance support
- AWS EKS Terraform infrastructure with spot instance support
- Helm repository hosted on GitHub Pages (#46)

### Documentation
- Added docs/MULTI-GPU-DEPLOYMENT.md comprehensive guide
- Added docs/MULTI_CLOUD_DEPLOYMENT.md for cloud-agnostic deployments
- Added test/e2e/multi-gpu-test-plan.md
- Updated README.md with multi-GPU feature availability
- Updated ROADMAP.md to reflect v0.4.0 milestone completion

### Testing
- Added comprehensive multi-GPU unit tests
- Added tolerations and nodeSelector propagation tests
- Verified on bare metal cluster (2x RTX 5060 Ti, ~44 tok/s)

## What's Next

### v0.5.0 (Planned)
- Auto-scaling based on request queue depth
- Prometheus metrics and Grafana dashboards
- Multi-node distributed inference

See [ROADMAP.md](ROADMAP.md) for complete roadmap.

## Resources

- **Issue #2**: [Multi-GPU single-node support](https://github.com/defilantech/LLMKube/issues/2)
- **Multi-GPU Guide**: [MULTI-GPU-DEPLOYMENT.md](docs/MULTI-GPU-DEPLOYMENT.md)
- **Multi-Cloud Guide**: [docs/MULTI_CLOUD_DEPLOYMENT.md](docs/MULTI_CLOUD_DEPLOYMENT.md)
- **Documentation**: [README.md](README.md)
- **Roadmap**: [ROADMAP.md](ROADMAP.md)

## Community & Support

- **GitHub Issues**: [Report bugs or request features](https://github.com/defilantech/LLMKube/issues)
- **Discussions**: [Ask questions and share ideas](https://github.com/defilantech/LLMKube/discussions)

---

**Version**: v0.4.0
**Release Date**: November 25, 2025
**License**: Apache 2.0
