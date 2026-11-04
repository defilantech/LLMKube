# LLMKube Roadmap

**Last Updated:** 2026-06-08, at v0.8.1. The milestones and the "What's shipped" list below date from then. For the current release, see the [latest release](https://github.com/defilantech/LLMKube/releases/latest) and [CHANGELOG.md](CHANGELOG.md).

---

## Vision

**Run LLMs on any accelerator you own, anywhere, under one governed control plane.**

LLMKube treats a fleet of mixed hardware as a single inference substrate, then layers governance on top. It exists for organizations that cannot or will not send AI workloads to a cloud API and need to run them sovereignly across the infrastructure they already operate.

Two pillars:

1. **Heterogeneous fleet.** NVIDIA datacenter GPUs, AMD/Intel, Apple Silicon, and edge nodes, managed by one Kubernetes operator. Apple Silicon as a first-class Kubernetes fleet member is the part nobody else does.
2. **Sovereign and governed.** Air-gapped by design, with policy-aware routing (what data may leave the perimeter), economic accountability (what each team and model costs on your own hardware and power), SLO enforcement, and agentic batch execution on local models.

**Who it's for:** platform and AI-ops teams at regulated, multi-site, or sovereignty-driven organizations (manufacturing, finance, defense, healthcare, public sector) running Kubernetes, who need production LLM inference on-prem and at the edge.

Every feature ladders to one of three jobs: make the heterogeneous fleet **schedulable**, make it **governable**, or make it **economically legible**.

---

## The architecture (north star)

A control plane in four planes, with strict dependency direction. Policy flows down, state flows up, the Kubernetes API server is the only shared state.

- **Serving** (`Model`, `InferenceService`) schedules inference onto the fleet within declared resource and SLO limits.
- **Policy / routing** (`ModelRouter`) is the governance boundary: all traffic, human and agentic, enters through a router that enforces data classification, SLO-aware routing, and durable audit. Nothing talks to an `InferenceService` directly in production.
- **Economics** (InferCost, companion project) enforces token and dollar budgets synchronously and computes marginal cost per team per deployment from real hardware and power.
- **Agentic** (Foreman) submits batch workloads to the top of the stack with budget and SLO constraints that propagate downward before a GPU cycle is spent.

The current work (see milestones below) is about coupling these planes into one loop, not just shipping them as siblings.

---

## What's shipped (as of v0.8.1, June 2026)

Since then, among other changes (see the [CHANGELOG](CHANGELOG.md) for versions): the SGLang runtime, multi-node vLLM serving (`spec.multiNode`), the `ModelPool`, `GPUQuota`, `LoRAAdapter`, `MetalNode` and `FederatedCluster` CRDs, Pyrra SLOs, `s3://` and `oci://` model sources, and `llmkube metal join` for Macs.

### Serving core
- Kubernetes-native CRDs (`Model`, `InferenceService`)
- Model sources: HuggingFace / HTTP / PVC / `file://` (air-gapped), PVC-based cache with SHA256 keys
- OpenAI-compatible `/v1/chat/completions`, multi-replica with HPA and `kubectl scale`
- Full CLI (`deploy/list/status/queue/cache/benchmark/inspect/license`), Helm chart, model catalog
- Runtimes: **llama.cpp** (CUDA/Metal), **vLLM** (PagedAttention), **mlx-server** (Apple Silicon MLX)

### Heterogeneous GPU + Apple Silicon fleet
- NVIDIA (T4/L4/A100/H100/Blackwell) via CUDA 13 images; automatic GPU layer offload
- Single-node multi-GPU sharding (layer-split and tensor-split); hybrid CPU/GPU offload for MoE
- **Apple Silicon as a Kubernetes fleet member** via the metal-agent, with TurboQuant KV cache for long-context, memory pre-flight validation, and health monitoring
- GPU scheduling, contention visibility (`WaitingForGPU`), priority classes, queue management

### Governance and agentic
- **ModelRouter (Phase 1):** one OpenAI-compatible endpoint across local `InferenceService`s and external providers, with fail-closed routing for sensitive data and a per-request audit log
- **Foreman (v0.1):** Kubernetes-native agentic pipeline (coder / verifier / reviewer) dispatched across a heterogeneous local fleet, gated against the repo's own build and tests, including Job-mode execution against in-cluster models

### Observability and economics
- 10+ Prometheus metrics, OpenTelemetry tracing of ModelRouter proxy requests (OTLP, e.g. Tempo), DCGM GPU metrics, Grafana dashboards, PodMonitor
- **InferCost** (companion project): cost-per-token from hardware amortization + electricity + DCGM power, per-team attribution, and cloud-vs-on-prem break-even

---

## Roadmap

Milestones are tracked in [GitHub Milestones](https://github.com/defilantech/LLMKube/milestones). Dates are targets, sequenced around a concrete enterprise deployment (datacenter B200 + multi-site edge fleet) landing in production in Q3 2026.

### [M1: Trustworthy fleet](https://github.com/defilantech/LLMKube/milestone/1) - July 2026
*Fix what shipped and make the fleet substrate reliable before extending it.*

The heterogeneous-fleet claim is only as good as the fleet's ability to tell a live node from a dead one and to ship a correct change. This milestone closes the two correctness bugs on the core paths, adds liveness detection for fleet nodes (#627), migrates the metal-agent off deprecated `Endpoints` (#377), and lands Foreman v0.2 with a CI e2e gate.

### [M2: Sovereign enterprise (B200-ready)](https://github.com/defilantech/LLMKube/milestone/2) - August 2026
*The enterprise gate.*

Multi-cluster federation (#630) so a datacenter cluster and multiple edge sites are one fleet rather than islands; NVIDIA Blackwell B200 validation (#413); air-gapped model sources (#53); declarable SLOs via Pyrra (#415); GPUQuota for multi-tenant clusters (#416); supply-chain provenance (#233); and multi-vendor GPU scheduling for AMD/Intel (#395).

### [M3: Governed + heterogeneous breadth](https://github.com/defilantech/LLMKube/milestone/3) - Q4 2026
*Make governance real, and extend the fleet's reach.*

ModelRouter to GA (#437) with routing metrics, hierarchical budgets, and audit; **SLO-aware routing and scheduling** that turns SLOs from a dashboard into enforcement (#629); **InferCost token budgets as synchronous admission gates** rather than retrospective reports (#631); and runtime breadth across the fleet (per-CR metal runtime #525, multi-model edge serving #516, speculative decoding #502).

### [M4: Foundation + depth](https://github.com/defilantech/LLMKube/milestone/4) - ongoing
Testing and coverage, EndpointSlice migration follow-through, and depth on the Apple Silicon / TurboQuant path.

### [Backlog (Q1 2027+)](https://github.com/defilantech/LLMKube/milestone/5)
Real value, wrong moment: multi-node distributed inference via NVIDIA Dynamo (#517), signed private model registry (#22), LoRA adapters (#12), and advanced opt-in SLO auto-remediation (#10).

---

## What we focus on, and what we don't

We own a compound that no single project covers: **sovereign + heterogeneous + on-prem agentic + FinOps.** We are deliberately *not* trying to win:

- **Maximum throughput** at hyperscaler scale. llm-d, NVIDIA Dynamo, and AIBrix own multi-node KV-disaggregated serving for large GPU fabrics. We integrate with vLLM rather than compete on its benchmark.
- **Day-one simplicity.** If you want the single simplest way to serve one model, other tools are simpler. We make *regulated, heterogeneous, agentic* deployments tractable, which is a different job.
- **Single-machine Apple Silicon speed.** That is a property of the runtime (MLX, llama.cpp), which everyone shares. Our differentiator is managing a *fleet* of Apple Silicon as governed Kubernetes nodes.

---

## Performance goals

| Target | Status |
|--------|--------|
| Single GPU (3B): >60 tok/s | Achieved (64 tok/s on L4) |
| 2-GPU consumer (26B-A4B MoE): >80 tok/s | Achieved (~87 tok/s on 2x RTX 5060 Ti) |
| Multi-GPU (13B): >40 tok/s | Achieved (~44 tok/s on 2x RTX 5060 Ti) |
| Multi-node (70B): <500 ms P99 | Planned (Backlog, distributed inference) |
| Model load: <30 s any model | Planned |

---

## Past releases

- **v0.10.0** - September 2026 · metal-agent serves engines on loopback behind an authenticated ingress (breaking)
- **v0.9.0** - July 2026 · local and hostPath model sources gated by an allowlist, controller SSRF blocked (GHSA-jw3m-8q7m-f35r; breaking)
- **v0.8.1** - June 2026 · docs site restructure, Foreman overview cross-refs, release hardening
- **v0.8.0** - May 2026 · Foreman debut (Kubernetes-native agentic coder/verifier/reviewer fleet), Intel GPU contribution
- **v0.7.9** - May 2026 · mlx-server runtime for Apple Silicon, `kubectl scale` subresource
- **v0.7.8** - May 2026 · ModelRouter Phase 1 (cross-engine routing, fail-closed PII routing, audit log)
- **v0.7.7** - May 2026 · OpenShift first-class, vllm-swift + TurboQuant on Apple Silicon
- **v0.7.0** - April 2026 · Hybrid CPU/GPU offload for MoE, runtime-resolved HF sources, agentic-coding flags
- **v0.5.0** - March 2026 · Metal agent, memory validation, benchmarking
- **v0.4.0** - November 2025 · Multi-GPU support

See [CHANGELOG.md](CHANGELOG.md) for the full history.

---

## Principles

1. **Sovereign by default** - no external call-home, air-gap ready, audit-trail native.
2. **Heterogeneous, honestly** - if we claim we manage a fleet of any accelerator, a dead node must be detectable and the metal path must be genuinely Kubernetes-native.
3. **Governance is enforcement, not a dashboard** - budgets, SLOs, and data-residency rules either gate behavior or they are not features.
4. **Measure before claiming** - controlled benchmarks, quality anchored to speed, honest caveats. A number that looks too good is usually a broken harness.
5. **Production-ready from day one** - observability and reliability are table stakes.

---

## How to contribute

We are actively looking for contributors, especially on the milestone themes:

- Multi-cluster federation (status rollup, model distribution, federation router)
- Multi-vendor GPU scheduling (AMD ROCm, Intel oneAPI)
- SLO-aware routing and Pyrra integration
- Apple Silicon / metal-agent coverage and the mlx-server path
- Documentation, examples, and getting-started guides (see the [docs sprint](https://github.com/defilantech/LLMKube/issues/628))

**Before starting:** comment on the issue or open a [discussion](https://github.com/defilantech/LLMKube/discussions) to avoid duplicate work. See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, commit conventions, and the DCO sign-off requirement.

---

## Release cadence

- **Patch (0.x.y)** - fixes and new features. While on v0.x, release-please bumps the patch version for features (`bump-patch-for-minor-pre-major`), and releases often ship several times a week
- **Minor (0.x.0)** - normally a release that carries a breaking change, listed under "BREAKING CHANGES" in the CHANGELOG
- **Major (x.0.0)** - reserved for post-v1.0

---

## Feedback

- [GitHub Discussions](https://github.com/defilantech/LLMKube/discussions)
- [GitHub Issues](https://github.com/defilantech/LLMKube/issues)
- Star the repo if you find it useful

---

## License

Apache 2.0 - see [LICENSE](LICENSE)

---

*This roadmap is a living document. Priorities shift based on community feedback and real-world usage.*
