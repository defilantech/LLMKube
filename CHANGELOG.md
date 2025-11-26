# Changelog

All notable changes to LLMKube will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.2](https://github.com/defilantech/LLMKube/compare/llmkubev0.4.1...llmkubev0.4.2) (2025-11-26)


### Bug Fixes

* Resolve staticcheck SA5011 lint errors and update CONTRIBUTING.md ([#60](https://github.com/defilantech/LLMKube/issues/60)) ([c0b5824](https://github.com/defilantech/LLMKube/commit/c0b5824fa3c42a547c1c760c7dbb5dd68bd4e89f))

## [0.4.1](https://github.com/defilantech/LLMKube/compare/llmkube-0.4.0...llmkubev0.4.1) (2025-11-26)


### Features

* Add benchmark command and reorganize documentation ([58307be](https://github.com/defilantech/LLMKube/commit/58307bece720644bbdf1e27026a90279b9009c51))
* Add benchmark command and reorganize documentation ([ac8888e](https://github.com/defilantech/LLMKube/commit/ac8888ea2ac41f90ebd6b529deea86b2fa67f24f)), closes [#6](https://github.com/defilantech/LLMKube/issues/6)
* Add persistent model cache to avoid re-downloading ([83f844f](https://github.com/defilantech/LLMKube/commit/83f844f7b8ca18c2eed407b0f6995f2dc13e0965)), closes [#52](https://github.com/defilantech/LLMKube/issues/52)
* Add Release Please automation and version-agnostic docs ([dc2d54e](https://github.com/defilantech/LLMKube/commit/dc2d54ea15f936a62b6fa1d382c1f606d97a5610))
* **helm:** Add image digest support for production deployments ([a38801d](https://github.com/defilantech/LLMKube/commit/a38801dd61d5f6606209577744cc5376bf1eb626))
* Implement automatic port forwarding for benchmark command ([472b3ae](https://github.com/defilantech/LLMKube/commit/472b3ae74b73d1d55d5a8a2051625ed1c3834ad9))
* Persistent model cache with per-namespace PVC support ([ab04261](https://github.com/defilantech/LLMKube/commit/ab0426161e3765e539e82ccbf864da943974f199))
* Support per-namespace model cache PVCs ([c3cb891](https://github.com/defilantech/LLMKube/commit/c3cb891dc74c3718f495068c98418d84c78b6da9))


### Bug Fixes

* Add cacheKey to CRD and restrict cache to llmkube-system namespace ([464c23d](https://github.com/defilantech/LLMKube/commit/464c23d07bffebcab8cda58d8ce8d00ad8d4ecba))
* Address lint issues in benchmark command ([bf80610](https://github.com/defilantech/LLMKube/commit/bf806107c664425d9f8a4a3056600ba6ec95b34e))


### Documentation

* Update MODEL-CACHE.md for per-namespace PVC pattern ([0be3f46](https://github.com/defilantech/LLMKube/commit/0be3f4697fd249aba4e9120de93fe0d5942a3f90))

## [0.3.0] - 2025-11-23

### Added

#### Metal GPU Support for macOS (Apple Silicon)
- **Native Metal GPU Acceleration**: Full support for Apple Silicon (M1/M2/M3/M4) GPUs
  - 60-120 tok/s generation on M4 Max (Llama 3.1 8B: 40-60 tok/s, Llama 3.2 3B: 80-120 tok/s)
  - Native llama-server processes with Metal GPU offloading
  - Hybrid architecture: Kubernetes orchestration + native Metal performance
- **Metal Agent**: Background daemon for macOS that manages llama-server processes
  - Watches InferenceService CRDs and spawns native processes
  - Automatic Service and Endpoints creation for cluster integration
  - Health checking and process lifecycle management
  - Configurable via LaunchAgent (deployment/macos/com.llmkube.metal-agent.plist)
- **Platform Detection**: Automatic detection of Metal availability and GPU capabilities
- **CLI Metal Support**: `--accelerator metal` flag for one-command Metal deployments
  - `llmkube deploy llama-3.1-8b --accelerator metal`
  - Automatic GPU layer configuration and optimization
- **Multi-Accelerator Support**: Unified CLI for CUDA (cloud) and Metal (local) deployments
  - Same Kubernetes CRDs work across both platforms
  - Test locally on Mac, deploy to cloud with same configs

#### Developer Experience
- **GoReleaser Configuration**: Multi-platform CLI builds for macOS, Linux, Windows
  - Separate Metal agent binary for macOS (Intel + Apple Silicon)
  - Automated release workflow with GitHub Actions
- **Metal Quick Start Guide**: Comprehensive guide at `examples/metal-quickstart/README.md`
  - Architecture diagrams and explanations
  - Step-by-step setup instructions
  - Troubleshooting and performance tuning
- **macOS Deployment Guide**: Production deployment instructions at `deployment/macos/README.md`

### Changed
- **Deploy Command**: Enhanced to support Metal accelerator alongside GPU flag
- **Service Registry**: Added support for manual Endpoints management to bridge native processes

### Fixed
- Endpoints API deprecation warnings (SA1019) with appropriate nolint directives
- Metal agent linter issues and production stability improvements

### Documentation
- New: `examples/metal-quickstart/README.md` - Metal GPU quick start guide
- New: `deployment/macos/README.md` - macOS deployment and setup
- New: `cmd/metal-agent/main.go` - Metal agent binary implementation
- New: `pkg/agent/` - Agent, executor, watcher, and registry implementations
- New: `internal/platform/detect.go` - Platform and GPU detection
- Updated: README with Metal support documentation

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
