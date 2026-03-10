# LLMKube Roadmap

**Current Version:** 0.5.0
**Last Updated:** March 2026
**Status:** ✅ Phase 2 in Progress - Metal Agent, Memory Management, Observability, Benchmarking

---

## Vision

Make GPU-accelerated LLM inference on Kubernetes **dead simple**. Deploy production-ready inference services in minutes, not days.

**Target Users:**
- Developers building AI-powered applications
- Platform teams running internal LLM services
- Organizations needing air-gapped/edge deployments
- Anyone wanting OpenAI-compatible APIs on their own infrastructure

---

## Current Status

### ✅ What's Working Now (v0.5.0)

**Core Platform:**
- ✅ Kubernetes-native CRDs (`Model`, `InferenceService`)
- ✅ Automatic model download from HuggingFace/HTTP
- ✅ OpenAI-compatible `/v1/chat/completions` API
- ✅ Multi-replica deployment support
- ✅ Full CLI tool (`llmkube deploy/list/status/delete/queue/cache/benchmark/inspect/license`)
- ✅ Helm chart for easy installation
- ✅ Model catalog with pre-configured models and one-command deployment
- ✅ Air-gapped mode with `file://` and local path model sources

**GPU Acceleration:**
- ✅ NVIDIA GPU support (T4, L4, A100)
- ✅ **17x performance improvement** (64 tok/s GPU vs 4.6 tok/s CPU)
- ✅ Automatic GPU layer offloading
- ✅ Single-node multi-GPU sharding (up to 8 GPUs, layer-based splitting)
- ✅ Cost optimization (spot instances, auto-scale to 0)

**Apple Silicon (Metal) Support:**
- ✅ Metal Agent daemon for native macOS llama-server processes
- ✅ Pre-flight memory validation with GGUF-based estimation
- ✅ Per-model `memoryBudget` and `memoryFraction` CRD fields
- ✅ Continuous health monitoring of managed processes
- ✅ Agent metrics (memory budget, process health, active count)
- ✅ Homebrew auto-detection for llama-server binary
- ✅ Heterogeneous clusters (mix NVIDIA and Apple Silicon nodes)

**GPU Scheduling & Queue Management:**
- ✅ GPU contention visibility (`WaitingForGPU` phase)
- ✅ Queue position tracking for pending services
- ✅ Priority classes (critical/high/normal/low/batch)
- ✅ `llmkube queue` command to view waiting services
- ✅ Detailed scheduling status and messages

**Observability:**
- ✅ 10 custom Prometheus metrics (downloads, phases, queues, reconciliation)
- ✅ OpenTelemetry tracing with Tempo export (gRPC port 4317)
- ✅ GPU metrics via DCGM exporter (utilization, temperature, power, memory)
- ✅ PodMonitor for llama.cpp inference metrics scraping
- ✅ Pre-built Grafana dashboards
- ✅ SLO alerts for GPU health and service availability

**Model Management:**
- ✅ PVC-based model caching with SHA256 cache keys
- ✅ `llmkube cache` command (list, clear, preload, inspect)
- ✅ Native GGUF parser with metadata extraction
- ✅ `llmkube inspect` command for GGUF file introspection
- ✅ License compliance scanning (`llmkube license`)
- ✅ Init container customization with custom CA certificate support

**Benchmarking:**
- ✅ `llmkube benchmark` with load testing, stress testing, and sweep modes
- ✅ Concurrency, context size, and token count sweeps
- ✅ Per-request latency and throughput capture (p50, p95, p99)
- ✅ GPU monitoring during benchmarks

**Deployment Options:**
- ✅ Minikube/Kind (local development)
- ✅ GKE with GPU support (Terraform)
- ✅ EKS with GPU support (Terraform)
- ✅ AKS with GPU support (Terraform)
- ✅ Network policies in Helm chart

---

## What's Next

### Q1 2026: Polish & Growth *(in progress)*

**Focus:** Make it easier to use, support more use cases

**Completed:**
- ✅ **Model Catalog** - Pre-configured popular models with `llmkube deploy llama-3-8b --gpu`
- ✅ **`llmkube benchmark`** - Comprehensive performance testing (load, stress, sweeps)
- ✅ **Health checks and readiness probes** - Three-probe pattern for llama.cpp pods
- ✅ **Persistent model storage** - PVC-based caching (v0.4.1)
- ✅ **GPU queue management** - Priority classes and queue tracking (v0.4.9)
- ✅ **Multi-GPU** - Single-node sharding for 13B+ models (~65 tok/s on 8B with 2x RTX 5060 Ti)

**Remaining:**
- `llmkube init` - Interactive setup wizard
- `llmkube chat <model>` - Test inference from CLI
- Horizontal Pod Autoscaling (HPA) support

### Q2 2026: Edge & Hybrid

**Focus:** Support more deployment scenarios

**Planned:**
- **K3s Compatibility** - Lightweight Kubernetes for edge
- **ARM64 Support** - Raspberry Pi, NVIDIA Jetson
- **Cost Tracking** - Per-deployment cost attribution
- **Additional Model Formats** - SafeTensors, HuggingFace native

**Already shipped early:**
- ~~Air-gapped Mode~~ ✅ Shipped in v0.4.x (file:// and local path model sources)

### Q3 2026: Scale & Performance

**Focus:** Larger models, better performance

**Planned:**
- **Distributed Inference** - Shard large models (70B+) across nodes via llama.cpp RPC backend
- **Advanced Auto-scaling** - Queue depth, latency-based scaling
- **AMD GPU Support** - ROCm backend for AMD GPUs
- **Intel GPU Support** - oneAPI integration
- **Performance Optimizations** - KV cache sharing, batching improvements

### Q4 2026: Community & Ecosystem

**Focus:** Build sustainable open-source project

**Planned:**
- **v1.0 Release** - Production-hardened, stable APIs
- **Operator Hub** - Official Kubernetes Operator listing
- **ArgoCD/Flux Templates** - GitOps-ready deployments
- **Terraform Provider** - Infrastructure-as-code support
- **VS Code Extension** - YAML validation, snippets
- **Community Program** - Contributor guides, good first issues

---

## Performance Goals

| Milestone | Target | Status |
|-----------|--------|--------|
| **Single GPU (3B model)** | >60 tok/s | ✅ **Achieved** (64 tok/s on L4) |
| **Multi-GPU (13B model)** | >40 tok/s | ✅ **Achieved** (~44 tok/s on 2x RTX 5060 Ti) |
| **Multi-GPU (8B model)** | >60 tok/s | ✅ **Achieved** (~65 tok/s on 2x RTX 5060 Ti) |
| **Multi-node (70B model)** | <500ms P99 latency | 🎯 Planned |
| **Cost Efficiency** | <$0.01 per 1K tokens | 🎯 Planned |
| **Model Load Time** | <30s for any model | 🎯 Planned |

---

## Community Metrics

| Metric | Current | Q2 2026 Goal | Q4 2026 Goal |
|--------|---------|--------------|--------------|
| **GitHub Stars** | 25 | 200 | 1,000 |
| **Contributors** | 4 | 10 | 25 |
| **Forks** | 4 | 15 | 50 |
| **Production Deployments** | 1 | 10 | 25 |
| **Models Supported** | Any GGUF | 20+ pre-configured | 50+ pre-configured |

---

## How to Contribute

We're actively looking for contributors! Here's how you can help:

### 🐛 Found a Bug?
[Open an issue](https://github.com/defilantech/LLMKube/issues/new) with:
- What you expected to happen
- What actually happened
- Steps to reproduce
- Your environment (K8s version, cloud provider, etc.)

### 💡 Have a Feature Idea?
[Start a discussion](https://github.com/defilantech/LLMKube/discussions) to:
- Explain your use case
- Describe the proposed solution
- Share why others might find it useful

### 🔧 Want to Code?

**Good First Issues:**
- Documentation improvements
- Example applications (chatbot UI, RAG system, etc.)
- Additional model configurations
- Testing on different K8s platforms

**Advanced Contributions:**
- Distributed inference via llama.cpp RPC (70B+ models)
- Additional GPU vendors (AMD, Intel)
- Model format support (SafeTensors)
- Horizontal Pod Autoscaling (HPA)
- Performance optimizations

**Before starting:** Comment on the issue or open a discussion to avoid duplicate work.

### 📚 Help with Documentation?
- Tutorials and guides
- Deployment examples
- Use case write-ups
- Troubleshooting tips

---

## Release Schedule

We ship frequently with semantic versioning:

- **Patch releases (0.2.x):** Bug fixes, minor improvements - Monthly
- **Minor releases (0.x.0):** New features, backward compatible - Quarterly
- **Major releases (x.0.0):** Breaking changes, major milestones - Yearly

**Past releases:**
- **v0.4.0** - November 2025 (multi-GPU support) ✅
- **v0.4.1** - November 2025 (persistent model cache) ✅
- **v0.4.9** - December 2025 (GPU scheduling & priority classes) ✅
- **v0.4.13** - February 2026 (GGUF parser, init container customization, NetworkPolicy) ✅
- **v0.5.0** - March 2026 (Metal agent, memory validation, health monitoring, benchmarking) ✅ **Current**

**Upcoming releases:**
- **v0.6.0** - Q2 2026 (edge deployment, K3s, HPA)
- **v1.0.0** - Q4 2026 (stable, production-ready)

---

## Principles

**What guides our development:**

1. **Ease of Use** - If it takes more than 5 minutes to deploy, we're doing it wrong
2. **Show, Don't Tell** - Working examples over lengthy docs
3. **Performance Matters** - GPU acceleration should be automatic and obvious
4. **Production-Ready** - Observability and reliability from day one
5. **Community-First** - Build what users actually need, not what we think they need
6. **Keep it Simple** - Avoid over-engineering until there's proven demand

---

## Feedback

Your feedback shapes our roadmap! Tell us:

- What features would make LLMKube more useful for you?
- What's blocking you from using it in production?
- What models/use cases should we prioritize?
- What's confusing or hard to use?

**Ways to share:**
- 💬 [GitHub Discussions](https://github.com/defilantech/LLMKube/discussions)
- 🐛 [GitHub Issues](https://github.com/defilantech/LLMKube/issues)
- ⭐ Star the repo if you find it useful!

---

## License

Apache 2.0 - See [LICENSE](LICENSE)

---

**Last Updated:** March 2026
**Next Review:** June 2026

*This roadmap is a living document. Priorities may shift based on community feedback and real-world usage.*
