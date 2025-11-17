# LLMKube Development Roadmap

**Version**: 0.2.0 (GPU-Enhanced MVP Phase)
**Last Updated**: November 16, 2025
**License**: Apache 2.0

> **Mission**: Build the GPU-accelerated control plane for local LLMs—air-gapped, edge-native, SLO-enforced. Escape laptops; enter production.

## Executive Summary

LLMKube is a Kubernetes operator that treats GPU-accelerated LLM inference as a first-class workload. We're building infrastructure to deploy, scale, and observe local language models with NVIDIA GPUs—perfect for air-gapped environments, cloud-to-edge deployments, and regulated industries demanding high performance.

**Current Status**: ✅ **Phase 1 Complete** (GPU Inference & Observability) | 🚀 **Phase 2 Starting** (Multi-Platform CLI & Multi-GPU)
**Next Milestone**: Multi-GPU Support & Production Hardening (Phase 2-5)

---

## What's Working Now ✅

### Core Infrastructure (Phase 1-2: **COMPLETE**)

#### Kubernetes Operator
- ✅ **Custom Resource Definitions (CRDs)**
  - `Model` CRD: Defines LLM models with source URLs, quantization, hardware requirements
  - `InferenceService` CRD: Manages inference deployments with replicas, resources, endpoints
  - Full status reporting with conditions (Available, Progressing, Degraded)

- ✅ **Model Controller**
  - Automatic model download from HuggingFace and other HTTP sources
  - GGUF format support with metadata parsing
  - Size calculation and validation
  - Path management and status tracking

- ✅ **InferenceService Controller**
  - Automatic deployment creation with init containers for model downloading
  - Service creation (ClusterIP, NodePort, LoadBalancer)
  - GPU resource allocation and tolerations
  - Model reference validation and readiness checks
  - OpenAI-compatible endpoint routing

#### CLI Tool (Partial)
- ✅ **Implemented Commands**
  - `llmkube deploy` - Deploy models with extensive configuration options
  - `llmkube list` - List models and services
  - `llmkube status` - Check deployment status
  - `llmkube delete` - Remove deployments
  - `llmkube version` - Version information

- ⚠️ **Limited Functionality**
  - Basic CRUD operations work
  - Advanced features (watch, logs, port-forward) in development

#### Inference Runtime
- ✅ **llama.cpp Integration**
  - Automatic model download via init containers
  - OpenAI-compatible API (`/v1/chat/completions`)
  - CPU and GPU acceleration support
  - Streaming and non-streaming responses

#### Performance (Observed)
- ✅ **TinyLlama 1.1B Q4_K_M Benchmark** (GKE CPU nodes)
  - Model size: 637.8 MiB
  - Prompt processing: ~29 tokens/sec
  - Token generation: ~18.5 tokens/sec
  - Cold start (with download): ~5 seconds
  - Warm start: <1 second

---

## Completed Phases ✅

### Phase 0: GPU Foundation ✅ **COMPLETE** (Nov 16, 2025)

#### GKE GPU Cluster Setup
- ✅ **Terraform deployment** with L4 GPU node pools (GKE us-west1)
- ✅ **NVIDIA GPU Operator** running (driver 535.261.03, CUDA 12.2)
- ✅ **Device plugin verification** complete (nvidia.com/gpu resource available)
- ✅ **Cost optimization**: Spot instances configured, auto-scale to 0 enabled
- ✅ **Documentation**: GPU performance results in `docs/gpu-performance-phase0.md`

#### GPU-Aware CRDs
- ✅ **Multi-GPU API design** in Model CRD (future-proof)
  - GPU count, vendor, layers, sharding strategy
  - Supports up to 8 GPUs per model
- ✅ **InferenceService GPU resources** (GPU count, GPU memory)
- ✅ **Controller updates** for GPU scheduling
  - Tolerations for `nvidia.com/gpu` taint
  - Resource requests for `nvidia.com/gpu`
  - Node selector for GPU accelerator type
  - **GPU layer offloading logic** (`--n-gpu-layers 99`)

#### Performance Validation ⚡
- ✅ **Llama 3.2 3B Q8_0 Benchmark** (NVIDIA L4 GPU)
  - **64 tok/s generation** (13.5x faster than CPU)
  - **1,026 tok/s prompt processing** (66x faster than CPU)
  - **0.6s total response time** (17x faster than CPU's 10.3s)
  - All 29 layers offloaded to GPU (4.2GB VRAM used)
  - Power: 35W, Temp: 56-58°C, <1% variance across requests

### Phase 1: GPU Inference & Observability ✅ **COMPLETE** (Nov 17, 2025)

#### Controller Image Build
- ✅ **Rebuilt controller** with GPU layer offloading fix (v0.2-gpu)
- ✅ **Pushed to GCR**: gcr.io/llmkube-478121/llmkube-controller:v0.2-gpu
- ✅ **Deployed to cluster** and verified GPU layers apply automatically (29/29 layers)
- ✅ **E2E test script**: Comprehensive validation suite created (`test/e2e/gpu_test.sh`)

#### CLI Enhancements
- ✅ **`llmkube deploy --gpu`** flag for easy GPU model deployment
- ✅ **GPU auto-detection**: Automatically selects CUDA image when `--gpu` is set
- ✅ **Comprehensive GPU flags**: `--gpu-count`, `--gpu-layers`, `--gpu-memory`, `--gpu-vendor`
- ✅ **Enhanced output**: Beautiful deployment summary with resource details

#### Observability Stack
- ✅ **kube-prometheus-stack** deployed with full Prometheus Operator
- ✅ **DCGM ServiceMonitor** configured for GPU metrics collection
- ✅ **Grafana dashboard** created (`config/grafana/llmkube-gpu-dashboard.json`)
  - 6 panels: GPU utilization, temperature, power, memory (gauges + timeseries)
  - Auto-refresh every 10 seconds
- ✅ **SLO alert rules** configured (`config/prometheus/llmkube-alerts.yaml`)
  - GPU health alerts: utilization, temperature, memory, power
  - Service health: InferenceService down, Controller down
- ✅ **10+ DCGM metrics** flowing into Prometheus

## In Progress 🚧

### Phase 2-3: Multi-GPU Single Node

#### Multi-GPU Support
- ⏳ **13B model deployment** with 2x L4 GPUs
- ⏳ **Layer splitting** across GPUs
- ⏳ **Performance testing**: target 40-50 tok/s on 13B
- ⏳ **Cost analysis**: GPU utilization vs performance

#### Production Hardening
- ⏳ **Auto-scaling** based on GPU utilization
- ⏳ **Health checks** and readiness probes
- ⏳ **Resource limits** and quotas
- ⏳ **Security**: Pod Security Standards, RBAC refinement

---

## Roadmap by Quarter

### Q1 2026: GPU-Enhanced MVP (Months 1-4)

**Phase 1-2** ✅ **COMPLETE** (Nov 1-15)
- [x] Kubernetes Operator with Model + InferenceService CRDs
- [x] Automatic model downloading and caching
- [x] OpenAI-compatible inference endpoints
- [x] CLI for basic deployment operations
- [x] Single-node CPU inference working (baseline: 18.5 tok/s on 1.1B model)

**Phase 0** ✅ **COMPLETE** (Nov 16) - GPU Foundation
- [x] GKE GPU cluster via Terraform (L4 GPUs, spot instances)
- [x] NVIDIA GPU Operator + device plugin installation
- [x] Multi-GPU CRD API design (future-proof)
- [x] GPU-aware controller updates (tolerations, selectors, layer offloading)
- [x] Cost management setup (auto-scale to 0, billing alerts)
- [x] **Performance validation: 64 tok/s on 3B model (17x speedup)** ⚡

**Phase 1** ✅ **COMPLETE** (Nov 17) - GPU Inference & Observability
- [x] Rebuild controller with GPU fixes, deploy to GCR/GKE
- [x] CLI `--gpu` flag support for easy deployment
- [x] Prometheus + Grafana observability setup
- [x] E2E testing with GPU models

**Phase 2-3** (Nov 18 - Dec 31) - Multi-GPU & Platform Support
- [ ] Multi-platform CLI builds with GoReleaser (macOS, Linux, Windows)
- [ ] GitHub Actions release workflow
- [ ] Versioning strategy (Crossplane-style SemVer)
- [ ] Multi-GPU single-node support (2-4 GPUs)
- [ ] Benchmark 13B model on 2x L4 GPUs (target: >40 tok/s)

**Phase 4-5** (Jan 1-31) - Multi-GPU Single Node
- [ ] Multi-GPU layer offloading (2-4 GPUs on single node)
- [ ] GPU memory optimization
- [ ] 13B model deployment with 2x T4 GPUs
- [ ] SLO monitoring (latency thresholds, GPU util alerts)
- [ ] Health checks and readiness probes

**Phase 6** (Feb 1-14) - GPU Observability
- [ ] DCGM metrics integration
- [ ] GPU utilization dashboards
- [ ] Cost tracking per inference (GPU hours)
- [ ] Performance benchmarking suite

### Q2 2026: Multi-Node GPU & Edge (Months 5-7)

**Phase 7-8** (Feb-Mar) - Multi-Node GPU Sharding
- [ ] Layer-aware GPU scheduler (cross-node sharding)
- [ ] P2P KV cache sharing (RDMA for low latency)
- [ ] 70B model deployment across 4 GPU nodes
- [ ] Target latency: <500ms P99 with sharding
- [ ] Resource quota management

**Phase 9-10** (Apr-May) - SLO & Auto-scaling
- [ ] SLO Controller with GPU auto-scaling
- [ ] Automatic fallback to smaller models on breach
- [ ] Horizontal Pod Autoscaling (HPA) for GPU pods
- [ ] Rate limiting and queuing
- [ ] Cost-aware scheduling (prefer cheaper GPUs)

**Phase 11-12** (May-Jun) - Edge Hybrid
- [ ] K3s compatibility (CPU + Jetson GPU)
- [ ] ARM64 support (Raspberry Pi, Jetson Orin)
- [ ] Hybrid cloud-edge deployments
- [ ] Model caching layer (PersistentVolumes)
- [ ] LoRA adapter support

### Q3 2026: Production Hardening (Months 7-9)

**Phase 13-14**
- [ ] Advanced observability (hallucination detection)
- [ ] PII detection and redaction
- [ ] Carbon footprint tracking
- [ ] Cost allocation per inference
- [ ] Advanced GPU scheduling

**Phase 15-16**
- [ ] Horizontal Pod Autoscaling (HPA) integration
- [ ] Custom metrics for autoscaling (tokens/sec)
- [ ] Circuit breakers and fault injection
- [ ] Blue/green and canary deployments
- [ ] A/B testing framework

**Phase 17-18**
- [ ] First external pilot (manufacturing)
- [ ] Second pilot (finance/healthcare)
- [ ] Documentation overhaul
- [ ] Security audit
- [ ] Performance benchmarking suite

### Q4 2026: Enterprise & Open Source (Months 10-12)

**Phase 19-20**
- [ ] Helm chart distribution
- [ ] Open-source core release
- [ ] Enterprise features (TEE, compiler)
- [ ] KubeCon presentation
- [ ] Community building (Discord, docs site)

**Phase 21-22**
- [ ] ArgoCD GitOps integration
- [ ] Terraform provider
- [ ] Multi-cluster federation
- [ ] Advanced networking (service mesh)
- [ ] Backup and disaster recovery

**Phase 23-24**
- [ ] 1.0 Release
- [ ] Production case studies
- [ ] Training and certification
- [ ] Partner integrations
- [ ] Commercial launch

---

## Technical Architecture

### Current Stack
- **Operator**: Kubebuilder v4.x, controller-runtime
- **Runtime**: llama.cpp (CPU/CUDA backends)
- **GPU**: NVIDIA T4/L4 on GKE, GPU Operator, device plugin
- **API**: OpenAI-compatible REST
- **Storage**: EmptyDir (ephemeral), PV (future)
- **Metrics**: Prometheus (in-progress), DCGM for GPU
- **Deployment**: Terraform (GKE), Kustomize, kubectl

### Future Stack Additions
- **Multi-GPU**: Tensor/pipeline parallelism
- **Networking**: Istio/Linkerd for traffic management, RDMA for GPU-to-GPU
- **Tracing**: OpenTelemetry + Jaeger with GPU spans
- **Security**: OPA/Gatekeeper for policies
- **CI/CD**: GitHub Actions + ArgoCD
- **Edge**: K3s + Jetson GPUs, KubeEdge

---

## Success Metrics

### Technical KPIs (GPU-Enhanced)
- ✅ **Uptime**: 100% (current CPU deployment)
- ✅ **CPU Baseline**: 18.5 tok/s for 1.1B model (established)
- 🎯 **GPU Phase 2**: >100 tok/s for 7B on T4 (5-10x CPU speedup)
- 🎯 **GPU Phase 4**: >200 tok/s for 13B on 2x T4
- 🎯 **GPU Phase 7**: <500ms P99 for 70B on 4-node cluster
- 🎯 **Latency P99**: <1s for 7B on GPU (vs 2s target on CPU)
- 🎯 **Model Load Time**: <30s (current: 5s for 637MB)
- 🎯 **GPU Utilization**: >80% during active inference
- 🎯 **Cost Efficiency**: <$0.01 per 1K tokens on T4 spot instances
- 🎯 **Hallucination Rate**: <5% (monitoring TBD)

### Business KPIs
- 🎯 **GitHub Stars**: 1,000 by Q2 2026
- 🎯 **Active Pilots**: 3 by Q3 2026 (1 cloud GPU, 2 hybrid edge)
- 🎯 **Pipeline**: $500K by Q4 2026
- 🎯 **Contributors**: 10+ by Q3 2026
- 🎯 **Cloud Spend**: <$1,500/mo MVP budget (GPU optimization)

---

## Risk Management

### Current Risks

| Risk | Likelihood | Impact | Mitigation |
|------|------------|--------|------------|
| **GPU Cost Overruns** | **High** | **High** | Spot T4 instances, auto-scale to 0, $1,500/mo hard cap, daily monitoring |
| **CUDA/Driver Compatibility** | Medium | High | Pin NVIDIA driver versions in Terraform, test with llama.cpp official images |
| **Multi-GPU Complexity** | Medium | High | Incremental: Phase 2 (1 GPU) → Phase 4 (multi-GPU single node) → Phase 7 (multi-node) |
| **GPU/Edge Compatibility** | Medium | Medium | Start NVIDIA only; AMD/Intel later; Jetson for edge (Phase 11+) |
| **Hallucination SLO Accuracy** | Medium | High | Integrate LlamaGuard; A/B testing framework |
| **Model Licensing** | Medium | Medium | Curated model registry; license detection |
| **Team Bandwidth** | Low | Medium | Hiring GPU optimization contractor Q1 2026 |
| **Competition** | Medium | Medium | Focus on air-gapped + GPU + edge differentiation |

### Recently Mitigated
- ✅ **Model Loading** - Fixed with init container approach (Nov 15, 2025)
- ✅ **Path Inconsistencies** - Standardized on `/models` volume mount
- ✅ **Controller Deployment** - Resolved with proper image digest management

---

## Contributing & Next Steps

### Immediate Priorities (Phase 0-1: Next 2 Weeks)
1. **Deploy GKE GPU Cluster** - Run `terraform apply` in `terraform/gke`
2. **Verify GPU Setup** - NVIDIA device plugin, test GPU scheduling
3. **Update Controllers** - Add GPU tolerations, resource requests to InferenceService
4. **Benchmark Baseline** - Test llama.cpp CUDA backend on T4 with 7B model
5. **Documentation** - GPU setup guide (✅ complete), API reference next

### How to Contribute
- See open issues tagged `good-first-issue` and `help-wanted`
- Follow development in CLAUDE.md for AI-assisted workflow
- Join weekly sync: Mondays 10am PT (details in Discord)

### Resources
- **Slack/Discord**: [Coming Soon]
- **Docs**: README.md, CLAUDE.md, `/docs` (in progress)
- **Examples**: `config/samples/`
- **Benchmarks**: See performance section above

---

## Changelog

### v0.1.0 - November 15, 2025
- ✅ Initial operator implementation
- ✅ Model and InferenceService CRDs
- ✅ CLI tool (basic commands)
- ✅ Automatic model downloading
- ✅ OpenAI-compatible API
- ✅ GKE deployment tested

---

## Contact

**Owner**: Chris Maher
**Repo**: github.com/defilan/llmkube
**License**: Apache 2.0

*"Kubernetes for Intelligence. Let's ship it."*

## Phase 2 Additions (Deferred from Phase 1 Discussion)

### Multi-Platform CLI Builds & Versioning
- [ ] **GoReleaser Setup**: Add `.goreleaser.yaml` for multi-platform builds
  - Platforms: macOS (x86/ARM), Linux (ARM64/AMD64), Windows (ARM/x86)
- [ ] **Release Workflow**: Create `.github/workflows/release.yml`
  - Auto-trigger on git tags
  - Upload binaries to GitHub Releases
- [ ] **Versioning Strategy**: Implement Crossplane-style SemVer
  - Tag current state as v0.2.0
  - Add version.go for version tracking
  - Quarterly minor releases (similar to Crossplane)
  - Document in CONTRIBUTING.md
- [ ] **Release Process Documentation**
  - Conventional commits for changelog
  - Pre-release tags (alpha, beta, rc)
  - Multi-version support strategy

**Reference Research**: See discussion on 2025-11-17 for Crossplane versioning analysis.

