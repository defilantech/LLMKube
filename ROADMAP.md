# LLMKube Roadmap

**Last Updated:** 2026-11-22, at v0.10.19 (released 2026-11-18). For the current release, see the [latest release](https://github.com/defilantech/LLMKube/releases/latest) and [CHANGELOG.md](CHANGELOG.md).

---

## Vision

**Run LLMs on any accelerator you own, anywhere, under one governed control plane.**

LLMKube is a Kubernetes operator for self-hosted LLM inference. It treats a fleet of mixed hardware as a single inference substrate, then layers governance on top. It exists for organizations that cannot or will not send AI workloads to a cloud API and need to run them sovereignly across the infrastructure they already operate.

Two pillars:

1. **Heterogeneous fleet.** NVIDIA datacenter GPUs, AMD/Intel, Apple Silicon, and edge nodes, managed by one Kubernetes operator. Apple Silicon as a first-class Kubernetes fleet member is the part nobody else does.
2. **Sovereign and governed.** Air-gapped by design, with policy-aware routing (what data may leave the perimeter), economic accountability (what each team and model costs on your own hardware and power), and SLO enforcement.

**Who it's for:** platform and AI-ops teams at regulated, multi-site, or sovereignty-driven organizations (manufacturing, finance, defense, healthcare, public sector) running Kubernetes, who need production LLM inference on-prem and at the edge.

Every feature ladders to one of three jobs: make the heterogeneous fleet **schedulable**, make it **governable**, or make it **economically legible**.

---

## The architecture (north star)

A control plane in three planes, with strict dependency direction. Policy flows down, state flows up, the Kubernetes API server is the only shared state.

- **Serving** (`Model`, `InferenceService`) schedules inference onto the fleet within declared resource and SLO limits.
- **Policy / routing** (`ModelRouter`) is the governance boundary: all traffic, human and agentic, enters through a router that enforces data classification, SLO-aware routing, and durable audit. Nothing talks to an `InferenceService` directly in production.
- **Economics** (InferCost, companion project) enforces token and dollar budgets synchronously and computes marginal cost per team per deployment from real hardware and power.

The current work (see milestones below) is about coupling these planes into one loop, not just shipping them as siblings.

---

## What's shipped

### At v0.10.19

- **CRDs (8, all `v1alpha1`):** `Model`, `InferenceService`, `ModelRouter`, `ModelPool`, `GPUQuota`, `LoRAAdapter` and `MetalNode` in `inference.llmkube.dev`, and `FederatedCluster` in `federation.llmkube.dev`. The `llmkube` Helm chart installs all eight.
- **Runtimes (12 `spec.runtime` values):** seven run in-cluster as pods (`llamacpp`, `llamacpp-router`, `vllm`, `sglang`, `personaplex`, `generic`, and `tgi`, kept as a legacy runtime pinned to its final upstream release). Five more run only through the metal-agent, as native processes on Apple Silicon beside llama.cpp's `llama-server`: `mlx-server`, `omlx`, `vllm-swift`, `ollama` and `tensorfold`.
- **Accelerators:** `Model.spec.hardware.accelerator` accepts `cuda`, `rocm`, `vulkan`, `intel`, `metal` and `cpu`.

### Since v0.8.1 (June to November 2026)

78 releases shipped between v0.8.1 (2026-06-01) and v0.10.19 (2026-11-18). Their CHANGELOG entries list 351 features: 133 belong to Foreman, the agentic coding harness, which is now its own project, [Meitheal](https://github.com/defilantech/meitheal), and the other 218 are LLMKube's. The highlights, by theme, with the release each landed in:

- **Serving:** speculative decoding for llama.cpp (v0.8.17), extended to draft Models in v0.9.18; embedding and rerank serving through `spec.mode` (v0.8.20); the SGLang runtime (v0.9.4); the `GPUQuota` and `LoRAAdapter` CRDs (v0.9.6); the `llamacpp-router` runtime for several models behind one server (v0.9.8); the `ModelPool` CRD for swapping models on a shared GPU slot (v0.9.16); architecture-aware runtime placement (v0.9.18); multi-node vLLM serving with `spec.multiNode` (v0.9.25); and pinned default builds for llama.cpp, vLLM and SGLang, with vLLM's flags checked against its pin (v0.10.11).
- **Accelerator breadth:** AMD through Vulkan images (v0.8.8) and a ROCm/HIP tier (v0.9.8); configurable GPU resource names for AMD, Vulkan and Intel device plugins (v0.8.8); DRA `ResourceClaim` passthrough for any DRA driver (v0.8.9); vendor-neutral GPU sharing tiers with VRAM-based quota accounting, and Blackwell-ready image pins and platform floors (v0.9.9).
- **Model supply chain:** `s3://` sources (v0.9.6) and `oci://` sources through Kubernetes ImageVolume (v0.9.30); prefetch into the shared cache without an InferenceService (v0.9.11), with OCI registry publishing (v0.10.17); resumable downloads (v0.9.26 for single files, v0.10.1 for multi-file); Hugging Face mirrors through `HF_ENDPOINT` (v0.9.30); digest verification for multi-file and `pvc://` sources, and signed [Socair](https://github.com/defilantech/socair) attestations that can be required before a Model is served (v0.10.2); and `spec.files` staging from several repositories at once (v0.10.17).
- **Apple Silicon as a fleet member:** engines served on loopback behind an authenticated ingress, with an authenticated relay in front (v0.10.0, breaking); the TensorFold runtime for MLX speculative decoding (v0.10.0); memory admission that counts what running engines already hold (v0.10.5); the `MetalNode` CRD, verified by a controller dial-back, and `llmkube metal join`, `leave` and token commands (v0.10.12); engines that keep serving across metal-agent restarts, and signed, notarized macOS binaries (v0.10.13); a Homebrew cask (v0.10.15); and controller-to-agent version-skew and free-disk checks (v0.10.19). As of v0.10.19 the metal-agent also starts on Linux hosts, experimentally.
- **Routing and governance (`ModelRouter`):** a Gateway data plane on Envoy AI Gateway with JWT authentication, per-team model allowlists and token budgets (v0.8.8); an AgentGateway data plane beside Proxy and Gateway (v0.9.25); `/v1/embeddings` and `/v1/rerank` on the proxy (v0.9.26); token and dollar budgets enforced in the proxy data plane (v0.9.30); load-aware, session-sticky placement (v0.10.7) with prefix affinity (v0.10.17); the Anthropic Messages API (v0.10.9); opt-in caller attribution on the audit log (v0.10.14); shadow mirroring (v0.10.15); and `spec.backendSelector` with per-pod KV and slot metrics (v0.10.19).
- **Federation:** the `FederatedCluster` CRD, a registry of member clusters with a fleet-wide inference status rollup (v0.9.11).
- **Reliability and observability:** per-InferenceService SLOs through Pyrra (v0.9.8); Grafana dashboards for the vLLM and SGLang (v0.9.12) and llama.cpp (v0.9.14) runtimes, and an AMD GPU exporter and dashboard (v0.9.14); a pinned llama.cpp metric contract (v0.9.25); engine-stall liveness for vLLM and SGLang, and alerts for GPU faults the device plugin ignores (v0.10.3); watch-path stall detection (v0.10.4); alerts when a spec change goes unreconciled (v0.10.16); and router-proxy request spans exported over OTLP (v0.10.17).
- **Security and compliance:** local and hostPath model sources gated by an allowlist, and a blocked controller SSRF path (v0.9.0, GHSA-jw3m-8q7m-f35r, breaking); the authenticated metal-agent ingress (v0.10.0); an allowlisted environment for Metal engines and an admission policy that limits the metal-agent's Service and EndpointSlice writes (v0.10.15); and third-party license and NOTICE files in release archives and images (v0.10.19).

### Baseline as of v0.8.1

#### Serving core
- Kubernetes-native CRDs (`Model`, `InferenceService`)
- Model sources: HuggingFace / HTTP / PVC / `file://` (air-gapped), PVC-based cache with SHA256 keys
- OpenAI-compatible `/v1/chat/completions`, multi-replica with HPA and `kubectl scale`
- Full CLI (`deploy/list/status/queue/cache/benchmark/inspect/license`), Helm chart, model catalog
- Runtimes: in-cluster **llama.cpp**, **vLLM** (PagedAttention), TGI, PersonaPlex and `generic` containers; on the metal-agent, `llama-server`, **mlx-server** (Apple Silicon MLX), oMLX, vllm-swift and Ollama, chosen by the agent's `--runtime` flag

#### Heterogeneous GPU + Apple Silicon fleet
- NVIDIA (T4/L4/A100/H100/Blackwell) via CUDA 13 images; automatic GPU layer offload
- Single-node multi-GPU sharding (layer-split and tensor-split); hybrid CPU/GPU offload for MoE
- **Apple Silicon as a Kubernetes fleet member** via the metal-agent, with TurboQuant KV cache for long-context, memory pre-flight validation, and health monitoring
- GPU scheduling, contention visibility (`WaitingForGPU`), priority classes, queue management

#### Governance
- **ModelRouter (Phase 1):** one OpenAI-compatible endpoint across local `InferenceService`s and external providers, with fail-closed routing for sensitive data and a per-request audit log

#### Observability and economics
- 26 Prometheus metrics from the controller and metal-agent, DCGM GPU metrics, Grafana dashboards, PodMonitor
- **InferCost** (companion project): cost-per-token from hardware amortization + electricity + DCGM power, per-team attribution, and cloud-vs-on-prem break-even

---

## Roadmap

Milestones are tracked in [GitHub Milestones](https://github.com/defilantech/LLMKube/milestones). Dates are targets. Each heading gives the milestone's state on the date at the top of this page.

### [M1: Trustworthy fleet](https://github.com/defilantech/LLMKube/milestone/1) - target July 2026 · closed 2026-06-17
*Fix what shipped and make the fleet substrate reliable before extending it.*

The heterogeneous-fleet claim is only as good as the fleet's ability to tell a live node from a dead one and to ship a correct change. For inference, this milestone fixed cached Models that never refreshed when their upstream source changed (#619, with `refreshPolicy` and drift detection in #635), added liveness and retries for metal-agent endpoint registrations (#663, #657), and moved the metal-agent and controller from core/v1 `Endpoints` to discovery/v1 `EndpointSlice` (#377).

### [M2: Sovereign enterprise (B200-ready)](https://github.com/defilantech/LLMKube/milestone/2) - target August 2026 · closed 2026-11-22
*The enterprise gate.*

Delivered: air-gapped model sources (#53), declarable SLOs via Pyrra (#415), GPUQuota for multi-tenant clusters (#416), container image scanning and govulncheck in CI (#233), configurable GPU resource names for AMD, Vulkan and Intel scheduling (#395), a first-class AMD tier on Vulkan and ROCm (#696), the federation foundation with the `FederatedCluster` registry and fleet status rollup (#1234, #1235), and the Blackwell readiness work that does not need the hardware (#1375, #1376).

Its last two issues moved out when it closed: making Envoy AI Gateway fleet-aware (#661) went to the Backlog for a re-scope after upstream changes to the Gateway API Inference Extension, and the Foreman half of Homebrew packaging for the agents (#822) went to Meitheal; the metal-agent half shipped as a cask in v0.10.15. The B200 validation matrix (#413) and the multi-cluster federation epic (#630) moved to M3.

### [M3: Governed + heterogeneous breadth](https://github.com/defilantech/LLMKube/milestone/3) - target Q1 2027 (moved from Q4 2026) · open
*Make governance real, and extend the fleet's reach.*

Delivered: the ModelRouter epic (#437), with routing metrics (#433) and hierarchical token and dollar budgets with an audit log (#434); per-CR runtimes on the metal-agent (#525); speculative decoding (#502); a ROCm HIP tier for AMD (#701); cross-pod model swapping with `ModelPool` (#1111); drain-before-roll (#856); and OCI ImageVolume model sources (#1379).

Open:
- **SLO-aware routing and scheduling** that turns SLOs from a dashboard into enforcement (#629)
- **InferCost token budgets as synchronous admission gates** rather than retrospective reports (#631), and persistent budget and SLO state (#440)
- An MCP server endpoint and inline data classifier on the router (#441)
- Multi-cluster federation (#630): cross-cluster model distribution (#1236) and a federation router for tier overflow and data residency (#1237)
- The NVIDIA Blackwell B200 (sm_100) validation matrix (#413, executed in #1377)
- A position on GPU Dynamic Resource Allocation that keeps cross-pod GPU sharing (#1378)
- TurboQuant KV cache quantization (#308)
- GPU offload for the `llamacpp-router` runtime, which shipped in v0.9.8 without it (#516)

### [M4: Foundation + depth](https://github.com/defilantech/LLMKube/milestone/4) - ongoing · open
Testing, coverage and depth on the Apple Silicon / TurboQuant path. Delivered: broader unit test coverage (#89), an audit of controller tests that assert implementation rather than spec (#378), the docs sprint (#628), and the oMLX TurboQuant KV and paged SSD cache settings on the metal-agent (#277, #278). Open: a macOS CI runner for metal-agent coverage (#455), and a TurboQuant KV spike on consumer Blackwell (sm_120) (#601).

### [Backlog (Q1 2027+)](https://github.com/defilantech/LLMKube/milestone/5)
Real value, wrong moment: an NVIDIA Dynamo runtime for `InferenceService` (#517), a signed private model registry (#22), LoRA adapters on the llama.cpp path (#12; the SGLang `LoRAAdapter` CRD shipped in v0.9.6), and a fleet-aware Envoy AI Gateway integration, once it is re-scoped (#661). Advanced SLO auto-remediation (#10) was closed as not planned.

---

## What LLMKube focuses on, and what it doesn't

LLMKube covers a combination no single project does: **sovereign, heterogeneous inference with policy routing and cost accounting.** It deliberately does *not* try to win:

- **Maximum throughput** at hyperscaler scale. llm-d, NVIDIA Dynamo, and AIBrix own multi-node KV-disaggregated serving for large GPU fabrics. LLMKube integrates with vLLM rather than competing on its benchmark.
- **Day-one simplicity.** If you want the single simplest way to serve one model, other tools are simpler. LLMKube makes *regulated, heterogeneous* deployments tractable, which is a different job.
- **Single-machine Apple Silicon speed.** That is a property of the runtime (MLX, llama.cpp), which everyone shares. LLMKube's differentiator is managing a *fleet* of Apple Silicon as governed Kubernetes nodes.

---

## Performance goals

| Target | Status |
|--------|--------|
| Single GPU (3B): >60 tok/s | Achieved: 64 tok/s, Llama 3.2 3B on an NVIDIA L4 ([v0.2.0 notes](docs/releases/RELEASE_NOTES_v0.2.0.md)) |
| Multi-GPU (13B): >40 tok/s | Achieved: ~44 tok/s, Llama 2 13B Q4_K_M on 2x RTX 5060 Ti ([v0.4.0 notes](docs/releases/RELEASE_NOTES_v0.4.0.md)) |
| Multi-node (70B): <500 ms P99 | Not measured. Multi-node vLLM serving (`spec.multiNode`) shipped in v0.9.25 |
| Model load: <30 s any model | Not measured |

---

## Past releases

- **v0.10.19** - November 2026 · latest: ModelRouter `spec.backendSelector`, per-pod KV and slot metrics, the metal-agent on Linux (experimental), third-party license and NOTICE files in releases
- **v0.10.12** - October 2026 · `MetalNode` CRD, `llmkube metal join` and `leave`, agent tokens
- **v0.10.0** - September 2026 · metal-agent serves engines on loopback behind an authenticated ingress (breaking)
- **v0.9.25** - September 2026 · multi-node vLLM serving (`spec.multiNode`), AgentGateway data plane for ModelRouter
- **v0.9.0** - July 2026 · local and hostPath model sources gated by an allowlist, controller SSRF blocked (GHSA-jw3m-8q7m-f35r; breaking)
- **v0.8.1** - June 2026 · typed `spec.ropeScaling` for RoPE and YaRN context extension
- **v0.8.0** - May 2026 · Intel GPU support (oneAPI/SYCL), InferenceService name allowlist for multi-Mac fleets
- **v0.7.9** - May 2026 · mlx-server runtime for Apple Silicon, `kubectl scale` subresource
- **v0.7.8** - May 2026 · ModelRouter Phase 1 (cross-engine routing, fail-closed PII routing, audit log)
- **v0.7.7** - May 2026 · OpenShift first-class, vllm-swift + TurboQuant on Apple Silicon
- **v0.7.0** - April 2026 · Hybrid CPU/GPU offload for MoE, runtime-resolved HF sources, llama.cpp and vLLM runtime controls (reasoning budget, prefix caching)
- **v0.5.0** - March 2026 · Metal agent memory pre-flight validation, health monitoring, and per-model memory budgets
- **v0.4.0** - November 2025 · Multi-GPU support

See [CHANGELOG.md](CHANGELOG.md) for the full history.

---

## Principles

1. **Sovereign by default** - no external call-home, air-gap ready, audit-trail native.
2. **Heterogeneous, honestly** - if LLMKube claims to manage a fleet of any accelerator, a dead node must be detectable and the metal path must be genuinely Kubernetes-native.
3. **Governance is enforcement, not a dashboard** - budgets, SLOs, and data-residency rules either gate behavior or they are not features.
4. **Measure before claiming** - controlled benchmarks, quality anchored to speed, honest caveats. A number that looks too good is usually a broken harness.
5. **Production-ready from day one** - observability and reliability are table stakes.

---

## How to contribute

Contributions are welcome, especially on the open milestone themes:

- Multi-cluster federation: cross-cluster model distribution (#1236) and the federation router (#1237)
- SLO contracts and SLO-aware routing (#629)
- GPU Dynamic Resource Allocation that keeps cross-pod GPU sharing (#1378)
- Apple Silicon and metal-agent test coverage, including a macOS CI runner (#455)
- Documentation, examples, and getting-started guides

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
