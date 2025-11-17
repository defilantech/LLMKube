# LLMKube v0.1.0 Release Notes

**Release Date**: November 15, 2025
**Status**: MVP - Production Ready for Single-Node CPU Inference

## Overview

LLMKube v0.1.0 is the first public release of the Kubernetes-native control plane for local LLM inference. This release provides the core infrastructure needed to deploy, manage, and scale LLM inference services using Kubernetes primitives.

**TL;DR**: Deploy any GGUF model from HuggingFace with a simple CRD, get an OpenAI-compatible API endpoint in seconds.

## What's New

### Core Features ✅

#### Kubernetes Operator
- **Custom Resource Definitions (CRDs)**
  - `Model` CRD: Define LLM models with source URLs, quantization, hardware requirements
  - `InferenceService` CRD: Manage inference deployments with replicas, resources, endpoints
  - Full status reporting with conditions (Available, Progressing, Degraded)

- **Model Controller**
  - Automatic model download from HuggingFace and HTTP sources
  - GGUF format support with metadata parsing
  - Model size calculation and validation
  - Intelligent caching and path management

- **InferenceService Controller**
  - Automatic Deployment creation with init containers
  - Service creation (ClusterIP, NodePort, LoadBalancer)
  - GPU resource allocation and node tolerations
  - Model reference validation and readiness checks
  - OpenAI-compatible endpoint routing

#### CLI Tool
- `llmkube deploy` - Deploy models with extensive configuration options
- `llmkube list` - List models and inference services
- `llmkube status` - Check deployment health and readiness
- `llmkube delete` - Remove deployments cleanly
- `llmkube version` - Version and build information

#### Inference Runtime
- **llama.cpp Integration**
  - Automatic model download via init containers
  - OpenAI-compatible API (`/v1/chat/completions`)
  - CPU and GPU acceleration support
  - Streaming and non-streaming responses
  - Efficient memory management

### Performance Benchmarks

**TinyLlama 1.1B Q4_K_M** (GKE n2-standard-2, CPU):
- Model size: 637.8 MiB
- Prompt processing: ~29 tokens/sec
- Token generation: ~18.5 tokens/sec
- Cold start (with download): ~5 seconds
- Warm start: <1 second
- Latency P50: ~1.5s for simple queries

### Supported Platforms

**Kubernetes Distributions:**
- ✅ GKE (Google Kubernetes Engine)
- ✅ EKS (Amazon Elastic Kubernetes Service)
- ✅ AKS (Azure Kubernetes Service)
- ✅ minikube (local development)
- ✅ kind (Kubernetes in Docker)
- ✅ K3s (lightweight, edge-ready)

**Model Formats:**
- ✅ GGUF (via llama.cpp)
- 🚧 SafeTensors (planned Q2 2026)
- 🚧 HuggingFace native format (planned Q2 2026)

**Hardware:**
- ✅ CPU inference (all architectures)
- ⚠️ GPU inference (NVIDIA CUDA - experimental)
- 🚧 AMD ROCm (planned Phase 5-6)
- 🚧 Intel XPU (planned Phase 7-8)
- 🚧 ARM64 optimization (planned Phase 7-8)

## Installation

### Quick Install

```bash
# Install CRDs
kubectl apply -f https://raw.githubusercontent.com/Defilan/LLMKube/v0.1.0/config/crd/bases/inference.llmkube.dev_models.yaml
kubectl apply -f https://raw.githubusercontent.com/Defilan/LLMKube/v0.1.0/config/crd/bases/inference.llmkube.dev_inferenceservices.yaml

# Install operator
kubectl apply -f https://raw.githubusercontent.com/Defilan/LLMKube/v0.1.0/config/manager/manager.yaml

# Verify
kubectl get pods -n llmkube-system
```

### From Source

```bash
git clone https://github.com/Defilan/LLMKube.git
cd LLMKube
git checkout v0.1.0

make install  # Install CRDs
make deploy IMG=ghcr.io/defilan/llmkube-controller:v0.1.0
```

## Quick Start Example

Deploy TinyLlama in 3 commands:

```bash
# 1. Define the model
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: tinyllama
spec:
  source: https://huggingface.co/TheBloke/TinyLlama-1.1B-Chat-v1.0-GGUF/resolve/main/tinyllama-1.1b-chat-v1.0.Q4_K_M.gguf
  format: gguf
  quantization: Q4_K_M
  hardware:
    accelerator: cpu
  resources:
    cpu: "2"
    memory: "2Gi"
EOF

# 2. Create inference service
kubectl apply -f - <<EOF
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: tinyllama-service
spec:
  modelRef: tinyllama
  replicas: 1
  resources:
    cpu: "1"
    memory: "1Gi"
  endpoint:
    port: 8080
    type: ClusterIP
EOF

# 3. Test the API
kubectl port-forward svc/tinyllama-service 8080:8080 &
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"messages": [{"role": "user", "content": "Hello!"}]}'
```

**See [examples/quickstart/](examples/quickstart/) for detailed walkthrough.**

## Breaking Changes

None - this is the first release.

## Known Limitations

### Current Limitations
- **GPU support is experimental** - NVIDIA CUDA works but not fully tested
- **CLI is partially implemented** - `kubectl` workflow is more reliable
- **No observability yet** - Prometheus metrics coming in Phase 3-4
- **Single-node only** - Multi-node sharding coming in Phase 5-6
- **No SLO enforcement** - Auto-scaling and failover coming in Phase 9-10
- **GGUF only** - Other formats coming in Q2 2026

### Known Issues
- Large models (>13B) may require significant memory on single nodes
- Model download progress not visible in real-time (check pod logs)
- No automatic retry on download failures
- Service endpoint may take 10-20s to become ready after pod starts

**See [GitHub Issues](https://github.com/Defilan/LLMKube/issues) for tracking.**

## Migration Guide

Not applicable - first release.

## Deprecation Notices

None - first release.

## What's Next

See [ROADMAP.md](ROADMAP.md) for the full development plan.

### Phase 3-4 (Next 4 Weeks): Observability
- Prometheus metrics (tokens/sec, latency, request rate)
- Grafana dashboard templates
- OpenTelemetry tracing integration
- Health checks and readiness probes

### Phase 5-6 (Weeks 5-8): Multi-Node & GPU
- Layer-aware model sharding across nodes
- GPU support (NVIDIA, AMD)
- P2P KV cache sharing with RDMA
- Resource quota management

### Q2 2026: Edge & Advanced Features
- K3s and ARM64 optimization
- LoRA adapter support
- SLO Controller with auto-remediation
- Natural language deployment compiler
- TEE support (SGX, AMD SEV)

## Community & Contributing

We're building LLMKube in the open and welcome contributions!

**Ways to Contribute:**
- 🐛 **Report bugs**: [GitHub Issues](https://github.com/Defilan/LLMKube/issues)
- 💡 **Suggest features**: [GitHub Discussions](https://github.com/Defilan/LLMKube/discussions)
- 🔧 **Submit PRs**: See [CONTRIBUTING.md](CONTRIBUTING.md)
- 📖 **Improve docs**: Documentation PRs always welcome
- ⭐ **Star the repo**: Help us grow the community

**Help Wanted:**
- Prometheus metrics integration
- GPU testing (NVIDIA, AMD)
- E2E test coverage
- Documentation improvements
- Grafana dashboard templates

## Acknowledgments

Built with:
- [Kubebuilder](https://kubebuilder.io) - Kubernetes operator framework
- [llama.cpp](https://github.com/ggerganov/llama.cpp) - Efficient LLM inference
- [Cobra](https://github.com/spf13/cobra) - CLI framework

Special thanks to the Kubernetes and LLM communities for inspiration and tools.

## Support & Resources

- **Documentation**: [README.md](README.md)
- **Roadmap**: [ROADMAP.md](ROADMAP.md)
- **Contributing**: [CONTRIBUTING.md](CONTRIBUTING.md)
- **Examples**: [examples/](examples/)
- **Issues**: [GitHub Issues](https://github.com/Defilan/LLMKube/issues)

## Download

**Container Images:**
- Controller: `ghcr.io/defilan/llmkube-controller:v0.1.0`
- Runtime: `ghcr.io/ggerganov/llama.cpp:server` (upstream)

**Source Code:**
- GitHub: https://github.com/Defilan/LLMKube/releases/tag/v0.1.0
- Tarball: https://github.com/Defilan/LLMKube/archive/refs/tags/v0.1.0.tar.gz

**CLI Binaries:**
- Linux (amd64): [Coming soon]
- macOS (amd64): [Coming soon]
- macOS (arm64): [Coming soon]
- Windows (amd64): [Coming soon]

## License

LLMKube is licensed under the [Apache License 2.0](LICENSE).

---

**Questions?** Open an issue or start a discussion on GitHub.

**Want to help?** Check out [good-first-issue](https://github.com/Defilan/LLMKube/labels/good-first-issue) tags!

## Changelog

### v0.1.0 (November 15, 2025)

**Features:**
- Initial Kubernetes operator implementation
- Model and InferenceService CRDs
- Automatic model downloading from HuggingFace
- OpenAI-compatible API endpoints
- CLI tool (basic commands)
- CPU inference support
- Multi-replica deployments
- Support for ClusterIP, NodePort, LoadBalancer services

**Infrastructure:**
- GitHub Actions CI/CD (test, lint, e2e)
- Terraform configurations for GKE deployment
- Comprehensive documentation (README, ROADMAP, CONTRIBUTING)
- Quickstart examples and tutorials

**Testing:**
- Unit tests for controllers
- E2E test framework
- Validated on GKE, minikube, kind

---

*"Kubernetes for Intelligence. Let's ship it."*
