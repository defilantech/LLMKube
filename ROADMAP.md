# LLMKube Roadmap

**Current Version:** 0.7.0 (released 2026-04-18)
**Last Updated:** 2026-04-21

---

## Vision

Make GPU-accelerated LLM inference on Kubernetes **dead simple**. Deploy production-ready inference services in minutes, not days.

**Target Users:**
- Developers building AI-powered applications
- Platform teams running internal LLM services
- Organizations needing air-gapped / edge deployments
- Anyone wanting OpenAI-compatible APIs on their own infrastructure

---

## What's Working Now (v0.7.0)

### Core Platform
- Kubernetes-native CRDs (`Model`, `InferenceService`)
- Automatic model download from HuggingFace / HTTP / PVC / local file
- OpenAI-compatible `/v1/chat/completions` API
- Multi-replica deployment with Horizontal Pod Autoscaling (HPA)
- Full CLI (`llmkube deploy/list/status/delete/queue/cache/benchmark/inspect/license`)
- Helm chart for easy installation
- Model catalog with pre-configured models and one-command deploy
- Air-gapped mode with `file://` and local path model sources

### Runtime Backends (pluggable)
- **llama.cpp** — default CPU/GPU backend with GGUF model support
- **vLLM** — high-throughput batched inference with PagedAttention
- **TGI** — Hugging Face Text Generation Inference
- **PersonaPlex (Moshi)** — real-time voice/speech model serving
- **Ollama / oMLX** — Metal-accelerated backends for Apple Silicon

### GPU Acceleration
- NVIDIA GPU support (T4, L4, A100, H100, Blackwell) via CUDA 13 images
- 17x performance improvement vs CPU (64+ tok/s on L4 for 3B models)
- Automatic GPU layer offloading
- Single-node multi-GPU sharding (layer-split and tensor-split modes, custom layer splits)
- Hybrid GPU/CPU offloading for MoE models (CPU-resident expert layers)
- Cost optimization (spot instances, auto-scale to zero, HPA)

### Apple Silicon (Metal) Support
- Metal Agent daemon for native macOS llama-server processes
- oMLX and Ollama as alternative Metal-native backends
- Pre-flight memory validation with GGUF-based estimation
- Per-model `memoryBudget` and `memoryFraction` CRD fields
- Continuous health monitoring, agent metrics, Homebrew auto-detection
- Heterogeneous clusters (mix NVIDIA and Apple Silicon nodes)

### GPU Scheduling & Queue Management
- GPU contention visibility (`WaitingForGPU` phase)
- Queue position tracking for pending services
- Priority classes (critical / high / normal / low / batch)
- `llmkube queue` command

### Observability
- 10+ custom Prometheus metrics (downloads, phases, queues, reconciliation, HPA)
- OpenTelemetry tracing to Tempo (gRPC port 4317)
- GPU metrics via DCGM exporter
- PodMonitor for inference pod scraping
- Pre-built Grafana dashboards (operator + inference)
- SLO alerts for GPU health and service availability

### Model Management
- PVC-based model caching with SHA256 cache keys
- `llmkube cache` (list, clear, preload, inspect)
- Native GGUF parser with metadata extraction
- `llmkube inspect` for GGUF file introspection
- License compliance scanning (`llmkube license`)
- Init container customization with custom CA certificate support
- Agentic-coding flags for extended reasoning and long-context workloads

### Benchmarking
- `llmkube benchmark` (load, stress, sweep modes)
- Concurrency / context / token sweeps
- Per-request latency (p50, p95, p99) and throughput capture
- GPU monitoring during benchmarks

### Deployment Options
- Minikube / Kind (local dev)
- GKE / EKS / AKS with GPU support (Terraform modules)
- NetworkPolicy-ready Helm chart

---

## Near-term Focus

Direction of travel, not dated commitments. Community feedback shapes priority.

### API stability (v1beta1 prep)
- Consolidate runtime-specific config into per-runtime substructs (`LlamaCppConfig` to mirror existing `VLLMConfig` / `TGIConfig` / `PersonaPlexConfig`)
- Replace negative-boolean flags with clearer enums (e.g., `NoKvOffload` → `KVCacheDevice`)
- Audit `ExtraArgs` usage and promote common flags to typed fields
- Add validating webhook (Secret existence, `MinReplicas ≤ MaxReplicas`, resource quantity strings)
- Conversion webhook for `v1alpha1` ↔ `v1beta1`
- Status Phase → standard Conditions migration

### Supply-chain hardening
- cosign-sign binaries, controller images, and Helm chart
- SBOM publication with each release
- Checksum verification in `install.sh`
- `govulncheck` and security linters in CI

### Operator decomposition
- Split `inferenceservice_controller.go` into focused per-concern files (model storage, HPA, scheduling, deployment builder, status)

---

## Medium-term

- **Distributed inference** across nodes (llama.cpp RPC, vLLM distributed)
- **Additional GPU vendors** — AMD ROCm, Intel oneAPI
- **Advanced autoscaling** — queue-depth and latency-based signals
- **K3s / edge** compatibility, broader ARM64 node support
- **Additional model formats** — SafeTensors, HuggingFace native (alongside GGUF)
- **Cost tracking** — per-deployment cost attribution

---

## Performance Goals

| Milestone | Target | Status |
|-----------|--------|--------|
| Single GPU (3B model) | >60 tok/s | ✅ Achieved (64 tok/s on L4) |
| Multi-GPU (8B model) | >60 tok/s | ✅ Achieved (~65 tok/s on 2×RTX 5060 Ti) |
| Multi-GPU (13B model) | >40 tok/s | ✅ Achieved (~44 tok/s on 2×RTX 5060 Ti) |
| Multi-node (70B model) | <500 ms P99 latency | 🎯 Planned |
| Model load time | <30 s for any model | 🎯 Planned |

---

## Past Releases

- **v0.7.0** — April 2026 · Hybrid CPU/GPU offload for MoE, tensor overrides, batch-size controls, runtime-resolved HF sources, agentic-coding flags, vLLM `extraArgs` passthrough (breaking)
- **v0.6.0** — April 2026 · Pluggable runtime backends (vLLM, TGI, PersonaPlex), HPA, custom GPU layer splits, CUDA 13 default image
- **v0.5.3** — March 2026 · oMLX and Ollama as alternative Metal runtimes; KV cache type configuration
- **v0.5.0** — March 2026 · Metal agent, memory validation, health monitoring, benchmarking
- **v0.4.13** — February 2026 · GGUF parser, init container customization, NetworkPolicy
- **v0.4.9** — December 2025 · GPU scheduling & priority classes
- **v0.4.1** — November 2025 · Persistent model cache (PVC)
- **v0.4.0** — November 2025 · Multi-GPU support

See [CHANGELOG.md](CHANGELOG.md) for the full history.

---

## Principles

1. **Ease of Use** — If it takes more than 5 minutes to deploy, we're doing it wrong
2. **Show, Don't Tell** — Working examples over lengthy docs
3. **Performance Matters** — GPU acceleration should be automatic and obvious
4. **Production-Ready** — Observability and reliability from day one
5. **Community-First** — Build what users need, not what we think they need
6. **Keep it Simple** — Avoid over-engineering until there's proven demand

---

## How to Contribute

We're actively looking for contributors — especially for:

- Distributed inference (llama.cpp RPC, vLLM distributed)
- Additional GPU vendors (AMD ROCm, Intel oneAPI)
- v1beta1 API cleanup and conversion webhook
- Additional runtime backends (SGLang, MLC-LLM, …)
- Documentation, examples, and getting-started guides

**Good first issues:**
- Documentation improvements
- Example manifests and tutorials
- Additional model catalog entries
- Testing on different K8s platforms

**Before starting:** comment on the issue or open a [discussion](https://github.com/defilantech/LLMKube/discussions) to avoid duplicate work.

See [CONTRIBUTING.md](CONTRIBUTING.md) for local setup, commit conventions, and the DCO sign-off requirement.

---

## Release Cadence

- **Patch (0.x.y)** — bug fixes, minor improvements as needed
- **Minor (0.x.0)** — new features, mostly backward compatible (breaking changes flagged in CHANGELOG while on v0.x)
- **Major (x.0.0)** — reserved for post-v1.0

---

## Feedback

Your feedback shapes our roadmap:

- What features would make LLMKube more useful for you?
- What's blocking production adoption?
- Which models / use cases should we prioritize?

- 💬 [GitHub Discussions](https://github.com/defilantech/LLMKube/discussions)
- 🐛 [GitHub Issues](https://github.com/defilantech/LLMKube/issues)
- ⭐ Star the repo if you find it useful

---

## License

Apache 2.0 — see [LICENSE](LICENSE)

---

*This roadmap is a living document. Priorities shift based on community feedback and real-world usage.*
