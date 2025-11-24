# Changelog

All notable changes to LLMKube will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.2] - 2025-11-23

### Added

#### Model Catalog (Phase 1)
- **Pre-configured Model Catalog**: 10 battle-tested LLM models with optimized settings
  - Small models (1-3B): Llama 3.2 3B, Phi-3 Mini
  - Medium models (7-8B): Llama 3.1 8B, Mistral 7B, Qwen 2.5 Coder 7B, DeepSeek Coder 6.7B, Gemma 2 9B
  - Large models (13B+): Qwen 2.5 14B, Mixtral 8x7B, Llama 3.1 70B
- **Catalog CLI Commands**:
  - `llmkube catalog list` - Browse all available models with specifications
  - `llmkube catalog info <model-id>` - View detailed model information
  - `llmkube catalog list --tag <tag>` - Filter models by tags (code, small, recommended, etc.)
- **One-Command Deployments**: Deploy catalog models without specifying source URLs
  - `llmkube deploy llama-3.1-8b --gpu` - No need to find GGUF URLs
  - Automatic application of optimized settings (quantization, resources, GPU layers)
  - Flag overrides still work for customization
- **Embedded Catalog**: YAML catalog embedded in CLI binary for offline usage

#### Developer Experience
- **Enhanced Deploy Command**: Made `--source` flag optional for catalog models
- **Smart Defaults**: Catalog models come with pre-configured CPU, memory, GPU layers, and quantization
- **Better Error Messages**: Helpful suggestions when model not found in catalog
- **Documentation Updates**: README showcases catalog feature prominently

### Changed
- **CLI Help Text**: Updated deploy command examples to highlight catalog usage
- **README**: Added catalog section to features and quick start

### Fixed
- Line length and linter compliance in catalog implementation
- E2E test binary path for catalog tests

### Documentation
- New: `pkg/cli/catalog.yaml` - Embedded model catalog with 10 models
- New: Comprehensive unit tests (13 test functions, 50+ test cases)
- New: E2E tests for catalog commands
- Updated: README with catalog usage examples
- Updated: Deploy command help text with catalog examples

## [0.2.0] - 2025-11-17

### Added

#### GPU Acceleration (Phase 0-1)
- **17x Performance Improvement**: GPU-accelerated inference on NVIDIA GPUs (L4, T4, A100, V100)
  - 64 tok/s generation on Llama 3.2 3B (vs 4.6 tok/s CPU)
  - 1,026 tok/s prompt processing (66x faster than CPU)
  - 0.6s total response time (17x faster than CPU's 10.3s)
- **Automatic GPU Scheduling**: GPU resource requests, tolerations, and node selectors configured automatically
- **GPU Layer Offloading**: Automatic detection and configuration of optimal GPU layer count
- **CLI GPU Support**: `--gpu` flag for one-command GPU deployments
- **Multi-GPU API**: Future-proof CRD design supporting up to 8 GPUs per model
- **GPU Configuration Flags**: `--gpu-count`, `--gpu-memory`, `--gpu-layers`, `--gpu-vendor`

#### Observability Stack (Phase 1)
- **Prometheus Integration**: Full kube-prometheus-stack deployment with ServiceMonitors
- **DCGM GPU Metrics**: 10+ GPU metrics (utilization, temperature, power, memory)
- **Grafana Dashboard**: Pre-built GPU monitoring dashboard (`config/grafana/llmkube-gpu-dashboard.json`)
  - 3 gauge panels: GPU utilization, temperature, power
  - 3 timeseries panels: Memory, utilization over time, power over time
  - Auto-refresh every 10 seconds
- **SLO Alert Rules**: Production-ready alerts for GPU health and service availability
  - GPUHighUtilization, GPUHighTemperature, GPUMemoryPressure, GPUPowerLimit
  - InferenceServiceDown, ControllerDown

#### Infrastructure & Testing
- **GKE GPU Cluster Terraform**: Complete GPU cluster setup with NVIDIA L4 GPUs
  - Spot instance support (~70% cost savings)
  - Auto-scale to 0 for cost optimization
  - NVIDIA GPU Operator installation
- **E2E Test Suite**: Comprehensive 8-test validation suite (`test/e2e/gpu_test.sh`)
  - GPU scheduling verification
  - Inference endpoint testing
  - GPU metrics validation
  - Alert rules validation
- **GPU Quickstart Example**: Complete working example (`examples/gpu-quickstart/`)
  - Model and InferenceService YAML files
  - Automated test script
  - Comprehensive documentation with troubleshooting

### Changed
- **Controller Image**: Updated to support GPU layer offloading automatically
- **CLI Deploy Command**: Enhanced with GPU-specific flags and auto-detection
- **Documentation**: Complete rewrite of README, launch materials, and performance benchmarks
- **Version**: Bumped from 0.1.0 to 0.2.0

### Fixed
- **GPU Layer Offloading**: Controller now correctly applies `--n-gpu-layers 99` for automatic offloading
- **CUDA Image Selection**: CLI automatically selects CUDA image when `--gpu` flag is set

### Performance
- **Llama 3.2 3B Q8_0 on NVIDIA L4**:
  - Generation: 64 tok/s (17x faster than CPU)
  - Prompt Processing: 1,026 tok/s (66x faster than CPU)
  - Total Response: 0.6s (17x faster than CPU)
  - GPU Layers: 29/29 (100% offloaded)
  - GPU Memory: 4.2GB / 24GB
  - Power: 35W
  - Temperature: 56-58°C

### Documentation
- New: `RELEASE_NOTES_v0.2.0.md` - Comprehensive v0.2.0 release notes
- New: `examples/gpu-quickstart/` - GPU deployment quickstart guide
- New: `config/grafana/llmkube-gpu-dashboard.json` - GPU monitoring dashboard
- New: `config/prometheus/llmkube-alerts.yaml` - SLO alert rules
- New: `test/e2e/gpu_test.sh` - E2E test suite
- Updated: `README.md` - GPU sections, performance benchmarks
- Updated: `ROADMAP.md` - Phase 0-1 completion status
- Updated: `LAUNCH_ANNOUNCEMENT.md` - GPU-focused launch messaging

### Known Limitations
- Single-GPU only (multi-GPU coming in Phase 2-3)
- NVIDIA GPUs only (AMD/Intel support planned for later sprints)
- GGUF format only (SafeTensors planned)
- Tested primarily on GKE/EKS (other K8s distributions should work)

## [0.1.0] - 2025-11-15

### Added
- **Kubernetes Operator**: Complete operator implementation with Kubebuilder
- **Model CRD**: Define LLM models with source URLs, quantization, and hardware requirements
- **InferenceService CRD**: Manage inference deployments with replicas and resources
- **Model Controller**: Automatic model download from HuggingFace and other HTTP sources
  - GGUF format support
  - Size calculation and validation
  - Path management and status tracking
- **InferenceService Controller**: Automatic deployment and service creation
  - Init containers for model downloading
  - Service creation (ClusterIP, NodePort, LoadBalancer)
  - OpenAI-compatible endpoint routing
- **CLI Tool**: Basic CRUD operations
  - `llmkube deploy` - Deploy models
  - `llmkube list` - List models and services
  - `llmkube status` - Check deployment status
  - `llmkube delete` - Remove deployments
  - `llmkube version` - Version information
- **Inference Runtime**: llama.cpp integration
  - Automatic model download via init containers
  - OpenAI-compatible API (`/v1/chat/completions`)
  - CPU inference support
  - Streaming and non-streaming responses

### Performance
- **TinyLlama 1.1B Q4_K_M on GKE CPU nodes**:
  - Model size: 637.8 MiB
  - Prompt processing: ~29 tokens/sec
  - Token generation: ~18.5 tokens/sec
  - Cold start (with download): ~5 seconds
  - Warm start: <1 second

### Documentation
- Initial `README.md` with installation and usage instructions
- `ROADMAP.md` with development plan
- API documentation for CRDs
- Architecture overview in README

---

**Release Links:**
- v0.2.0: Full release notes at [RELEASE_NOTES_v0.2.0.md](RELEASE_NOTES_v0.2.0.md)
- Repository: https://github.com/Defilan/LLMKube
- Issues: https://github.com/Defilan/LLMKube/issues
- Discussions: https://github.com/Defilan/LLMKube/discussions
