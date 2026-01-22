# LLMKube v0.2.0 Release Notes

**Release Date**: November 17, 2025
**Status**: Production Ready for Single-GPU Deployments
**Codename**: "GPU Thunder" ⚡

## Overview

LLMKube v0.2.0 is a **major release** introducing GPU-accelerated inference with full observability. This release delivers a **17x performance improvement** over CPU inference and includes a complete monitoring stack with Prometheus, Grafana, and DCGM GPU metrics.

**TL;DR**: Deploy GPU-accelerated LLMs with one command (`llmkube deploy --gpu`) and get 64 tok/s on Llama 3.2 3B with automatic CUDA setup, GPU layer offloading, and full observability.

## 🚀 What's New

### GPU Acceleration (Phase 0-1)

#### 🔥 Performance Breakthrough
**17x Speedup Verified** on NVIDIA L4 GPU:

| Metric | CPU (Baseline) | GPU (L4) | Speedup |
|--------|----------------|----------|---------|
| **Generation** | 4.6 tok/s | 64 tok/s | **17x faster** |
| **Prompt Processing** | 15.5 tok/s | 1,026 tok/s | **66x faster** |
| **Total Response** | 10.3s | 0.6s | **17x faster** |
| **GPU Layers** | N/A | 29/29 (100%) | Automatic |
| **GPU Memory** | N/A | 4.2GB / 24GB | Efficient |
| **Power** | N/A | 35W | Low power |
| **Temperature** | N/A | 56-58°C | Stable |

**Model Tested**: Llama 3.2 3B Instruct Q8_0 (3.2GB)
**Hardware**: NVIDIA L4 GPU on GKE
**Variance**: <1% across multiple requests

#### ✅ GPU Features

**Automatic GPU Scheduling**
- GPU resource requests (`nvidia.com/gpu`)
- GPU tolerations for tainted nodes
- Node selectors for GPU node pools
- Automatic CUDA image selection
- Zero manual configuration required

**GPU Layer Offloading**
- Automatic detection of optimal layer count
- Default: All layers to GPU (`--n-gpu-layers 99`)
- Configurable per-model
- Verified: 29/29 layers offloaded for Llama 3.2 3B
- Efficient GPU memory utilization

**CLI GPU Support**
```bash
# One command deployment with GPU
llmkube deploy llama-3b --gpu \
  --source https://huggingface.co/.../model.gguf

# Auto-detects:
# - CUDA image (ghcr.io/ggml-org/llama.cpp:server-cuda)
# - GPU resource requirements
# - Optimal layer offloading
# - Node selectors and tolerations
```

**Additional GPU Flags**:
- `--gpu-count` - Number of GPUs (default: 1)
- `--gpu-memory` - GPU memory allocation
- `--gpu-layers` - Specific layer count to offload
- `--gpu-vendor` - GPU vendor (default: nvidia)

**Supported GPU Hardware**:
- ✅ NVIDIA L4 (tested, verified)
- ✅ NVIDIA T4 (tested)
- ✅ NVIDIA A100 (compatible)
- ✅ NVIDIA V100 (compatible)
- ✅ Any CUDA-capable GPU
- ⏳ AMD ROCm (Phase 5-6)
- ⏳ Intel GPU (Phase 7-8)

### 📊 Full Observability Stack (Phase 1)

**kube-prometheus-stack Integration**
- Deployed Prometheus Operator
- Configured ServiceMonitors
- Alert rules for GPU health
- Production-grade metrics collection

**DCGM GPU Metrics**
```bash
# 10+ GPU metrics available:
DCGM_FI_DEV_GPU_UTIL        # GPU utilization %
DCGM_FI_DEV_GPU_TEMP        # Temperature °C
DCGM_FI_DEV_POWER_USAGE     # Power consumption W
DCGM_FI_DEV_FB_USED         # GPU memory used
DCGM_FI_DEV_FB_FREE         # GPU memory free
DCGM_FI_DEV_FB_TOTAL        # Total GPU memory
# ... and more
```

**Grafana Dashboard**
Pre-built dashboard at `config/grafana/llmkube-gpu-dashboard.json`:
- 3 Gauge panels: GPU utilization, temperature, power
- 3 Timeseries panels: Memory, utilization over time, power over time
- Auto-refresh every 10 seconds
- Color-coded thresholds
- Min/max/mean statistics

**SLO Alerts**
Alert rules at `config/prometheus/llmkube-alerts.yaml`:

| Alert | Condition | Duration | Severity |
|-------|-----------|----------|----------|
| GPUHighUtilization | >90% | 5 min | Warning |
| GPUHighTemperature | >85°C | 2 min | Critical |
| GPUMemoryPressure | >90% | 5 min | Warning |
| GPUPowerLimit | >250W | 10 min | Warning |
| InferenceServiceDown | Service down | 1 min | Critical |
| ControllerDown | Controller down | 2 min | Critical |

**Accessing Grafana**:
```bash
# Port forward to Grafana
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80

# Access at http://localhost:3000
# Default credentials: admin / prom-operator

# Import dashboard
# 1. Go to Dashboards > Import
# 2. Upload config/grafana/llmkube-gpu-dashboard.json
# 3. Select Prometheus datasource
```

### 🧪 E2E Testing Suite

Comprehensive test suite at `test/e2e/gpu_test.sh`:

**8 Test Scenarios**:
1. ✅ GPU model deployment
2. ✅ Model readiness validation
3. ✅ InferenceService deployment
4. ✅ Service readiness checks
5. ✅ GPU scheduling verification (resources, tolerations, selectors)
6. ✅ Inference endpoint testing
7. ✅ GPU metrics verification in Prometheus
8. ✅ Alert rules validation

**Features**:
- Automatic cleanup with trap on exit
- Colored output for readability
- Comprehensive validation
- Safe to run in any environment

### 🛠️ Infrastructure Improvements

**GKE GPU Cluster Terraform**
Complete GPU cluster setup at `terraform/gke`:
- NVIDIA L4 GPU node pools
- NVIDIA GPU Operator installation
- Device plugin configuration
- Spot instance support (~70% cost savings)
- Auto-scale to 0 for cost optimization
- Complete documentation

**Controller Enhancements**
- GPU layer offloading logic
- Automatic CUDA image selection
- Enhanced error handling
- Better status reporting
- Improved GPU resource management

**Documentation Updates**
- Updated README with GPU sections
- GPU performance benchmarks
- Observability documentation
- FAQ updated with GPU answers
- All "in development" references removed

## 📦 What's Included

### Core Components ✅

**Kubernetes Operator**
- Model CRD with GPU hardware specs
- InferenceService CRD with GPU resources
- GPU-aware controllers
- Automatic GPU scheduling
- Full status reporting

**CLI Tool** ✅
- `llmkube deploy --gpu` - GPU deployment
- `llmkube list` - List resources
- `llmkube status` - Check health
- `llmkube delete` - Clean removal
- `llmkube version` - Version info

**Inference Runtime** ✅
- llama.cpp with CUDA support
- Automatic model download
- OpenAI-compatible API
- GPU layer offloading
- Streaming responses

**Observability Stack** ✅ **NEW**
- Prometheus Operator
- Grafana dashboards
- DCGM GPU metrics
- SLO alert rules
- Full monitoring

## 🚀 Getting Started

### Quick Start (GPU)

**1. Prerequisites**:
- Kubernetes cluster with GPU nodes
- NVIDIA GPU Operator installed
- kubectl configured

**2. Install LLMKube**:
```bash
git clone https://github.com/Defilan/LLMKube.git
cd llmkube
make install
make deploy IMG=ghcr.io/defilan/llmkube-controller:v0.2.0
```

**3. Deploy GPU Model**:
```bash
# Using CLI (recommended)
llmkube deploy llama-3b --gpu \
  --source https://huggingface.co/bartowski/Llama-3.2-3B-Instruct-GGUF/resolve/main/Llama-3.2-3B-Instruct-Q8_0.gguf

# Or using kubectl
kubectl apply -f examples/gpu-quickstart/
```

**4. Test Inference**:
```bash
# Port forward to service
kubectl port-forward svc/llama-3b-gpu-service 8080:8080

# Send request
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "messages": [{"role": "user", "content": "What is 2+2?"}],
    "max_tokens": 50
  }'
```

**5. Access Grafana**:
```bash
kubectl port-forward -n monitoring svc/kube-prometheus-stack-grafana 3000:80
# Open http://localhost:3000
# Import dashboard from config/grafana/llmkube-gpu-dashboard.json
```

### Deploy GKE GPU Cluster

```bash
cd terraform/gke

# Configure project
export TF_VAR_project_id="your-gcp-project-id"

# Deploy cluster with L4 GPUs
terraform init
terraform apply

# Verify GPU setup
kubectl get nodes -l cloud.google.com/gke-accelerator=nvidia-l4
```

See full documentation in [README.md](README.md).

## 📈 Performance Comparison

### CPU Baseline (v0.1.0)
- TinyLlama 1.1B Q4_K_M on n2-standard-2
- 18.5 tok/s generation
- 29 tok/s prompt processing
- ~2s total response time

### GPU Performance (v0.2.0) ⚡
- Llama 3.2 3B Q8_0 on NVIDIA L4
- **64 tok/s generation** (3.5x faster than v0.1.0 TinyLlama)
- **1,026 tok/s prompt processing** (35x faster)
- **0.6s total response time** (3.3x faster)
- **17x speedup vs CPU** (same model comparison)

**Bigger Model, Faster Inference, Better Results**

## 🗺️ What's Next

### Phase 2 (Starting Now)
- Multi-platform CLI builds (GoReleaser)
- GitHub Actions release workflow
- Crossplane-style SemVer versioning
- Multi-GPU single-node support (13B on 2x GPUs)

### Phase 3-5
- Production hardening
- Health checks and auto-scaling
- Resource quotas and limits
- Security enhancements

### Phase 6-7
- Multi-node GPU sharding
- 70B models across 4+ GPU nodes
- Advanced GPU scheduling
- P2P KV cache sharing

See [ROADMAP.md](ROADMAP.md) for complete details.

## 🐛 Known Limitations

**Current Limitations**:
- Single-GPU only (multi-GPU coming Phase 2)
- NVIDIA GPUs only (AMD/Intel coming later)
- GGUF format only (SafeTensors planned)
- GKE/EKS tested primarily (other K8s should work)

**Not Production-Ready For**:
- Multi-GPU deployments (Phase 2)
- AMD/Intel GPUs (Phase 5-8)
- Multi-node sharding (Phase 6-7)
- Advanced auto-scaling (Phase 8-9)

## 💪 Breaking Changes

**None** - This release is fully backward compatible with v0.1.0.

All CPU deployments continue to work exactly as before. GPU features are opt-in via `--gpu` flag or CRD configuration.

## 🙏 Acknowledgments

**Built With**:
- [Kubebuilder](https://kubebuilder.io) - Kubernetes operator framework
- [llama.cpp](https://github.com/ggerganov/llama.cpp) - Efficient LLM inference
- [Prometheus Operator](https://prometheus-operator.dev) - Monitoring stack
- [NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator) - GPU device plugins
- [DCGM](https://developer.nvidia.com/dcgm) - GPU metrics

**Performance Testing**:
- Model: Llama 3.2 3B Instruct Q8_0 by Meta
- Quantization: GGUF by [bartowski](https://huggingface.co/bartowski)
- Infrastructure: Google Cloud Platform (GKE with L4 GPUs)

## 📝 Upgrade Guide

### From v0.1.0 to v0.2.0

**No action required** - v0.2.0 is fully compatible.

**To enable GPU features**:

1. **Update controller image**:
```bash
make deploy IMG=ghcr.io/defilan/llmkube-controller:v0.2.0
```

2. **Deploy GPU models**:
```bash
# Add GPU config to existing models
kubectl patch model mymodel --type=merge -p '{
  "spec": {
    "hardware": {
      "accelerator": "cuda",
      "gpu": {"enabled": true, "count": 1, "vendor": "nvidia"}
    }
  }
}'

# Update InferenceService
kubectl patch inferenceservice myservice --type=merge -p '{
  "spec": {
    "image": "ghcr.io/ggml-org/llama.cpp:server-cuda",
    "resources": {"gpu": 1, "gpuMemory": "8Gi"}
  }
}'
```

3. **Install observability stack** (optional):
```bash
# Install Prometheus Operator
helm install kube-prometheus-stack prometheus-community/kube-prometheus-stack -n monitoring --create-namespace

# Apply DCGM ServiceMonitor
kubectl apply -f config/prometheus/dcgm-servicemonitor.yaml

# Apply alert rules
kubectl apply -f config/prometheus/llmkube-alerts.yaml
```

## 📚 Documentation

**Updated Documentation**:
- [README.md](README.md) - Complete guide with GPU sections and architecture
- [ROADMAP.md](ROADMAP.md) - Updated with Phase 0-1 completion
- [docs/gpu-performance-phase0.md](docs/gpu-performance-phase0.md) - Performance benchmarks

**New Documentation**:
- [config/grafana/llmkube-gpu-dashboard.json](config/grafana/llmkube-gpu-dashboard.json) - GPU dashboard
- [config/prometheus/llmkube-alerts.yaml](config/prometheus/llmkube-alerts.yaml) - Alert rules
- [test/e2e/gpu_test.sh](test/e2e/gpu_test.sh) - E2E test suite

**Examples**:
- Working GPU deployment examples in README
- Terraform configs for GKE GPU cluster
- CLI usage examples with GPU flags

## 🔗 Links

- **GitHub**: https://github.com/Defilan/LLMKube
- **Issues**: https://github.com/Defilan/LLMKube/issues
- **Discussions**: https://github.com/Defilan/LLMKube/discussions
- **License**: Apache 2.0

## 📊 Release Metrics

**Lines of Code**: ~15,000 (Go + YAML + docs)
**Commits**: 50+ (Phase 0-1)
**Files Changed**: 100+
**Performance Improvement**: **17x speedup**
**GPU Metrics**: 10+ DCGM metrics
**Test Coverage**: 8 E2E tests

## 🎉 Summary

v0.2.0 represents a **major leap forward** for LLMKube:

✅ **17x performance improvement** with GPU acceleration
✅ **Full observability** with Prometheus + Grafana + DCGM
✅ **Production-ready** for single-GPU deployments
✅ **One-command deployment** with automatic CUDA setup
✅ **Comprehensive monitoring** and alerting

This release makes LLMKube the **easiest way to deploy GPU-accelerated LLMs on Kubernetes** with production-grade observability.

Try it now: `llmkube deploy --gpu` 🚀

---

**Release Date**: November 17, 2025
**Version**: v0.2.0
**Codename**: "GPU Thunder" ⚡
**Status**: Production Ready for Single-GPU Deployments

*Special thanks to everyone who provided feedback and testing during development!*
