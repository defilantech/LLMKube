---
title: Architecture
description: How LLMKube's controller, runtime pods, metal-agent, and router-proxy work together. Two cooperating control planes (in-cluster controller plus optional host-native metal-agent) and one data plane (the runtime pods and, when ModelRouter is in use, the router-proxy).
---

# Architecture

LLMKube's architecture is shaped around a single guiding decision:
the *control plane* lives in Kubernetes, but the *workload* can
live wherever the GPU lives. That lets the same operator manage
NVIDIA pods running inside the cluster, Apple Silicon Macs running
native processes alongside the cluster, and external cloud
providers routed through `ModelRouter` — all surfaced as the same
CRDs to `kubectl`.

This page is the map of how those pieces fit together.

## The three custom resources

All three live in `inference.llmkube.dev/v1alpha1`.

- **`Model`** declares a model artifact: where the GGUF / HF
  weights come from (URL, PVC, file path, HuggingFace repo ID),
  what runtime can consume it, and any hardware preferences
  (Metal vs CUDA, multi-GPU sharding hints).
- **`InferenceService`** declares a serving deployment: a
  reference to a `Model`, a runtime (`llamacpp`, `vllm`, `tgi`,
  `ollama`), replicas, resources, an OpenAI-compatible endpoint.
  Two `InferenceService` objects can share a `Model`.
- **`ModelRouter`** declares a policy-aware HTTP router in front
  of one or more `InferenceService` and / or external
  provider backends. Optional; only needed when you want
  classification-aware routing, fail-closed semantics, or
  per-rule timeout budgets.

See the [`CRD reference`](./crds) for every field, and the
[`Model Router`](./model-router) concept doc for the full policy
model.

## The two control planes

```
┌─────────────────────────────────────────────────────────────────┐
│                     Kubernetes cluster                          │
│                                                                 │
│   ┌──────────────────────────────────────────────────────┐      │
│   │ LLMKube controller (in-cluster)                      │      │
│   │   • Reconciles Model / InferenceService / ModelRouter│      │
│   │   • Schedules runtime pods (NVIDIA path)             │      │
│   │   • Schedules router-proxy Deployment                │      │
│   │   • Owns CRD status, conditions, audit events        │      │
│   └──────────────────────────────────────────────────────┘      │
│                                                                 │
│   Runtime pods (NVIDIA / CPU)         router-proxy Deployment   │
│   ┌─────────────────┐                  ┌─────────────────┐      │
│   │ llama.cpp / vLLM│  ◄──policy──     │  ModelRouter    │      │
│   │ TGI / Ollama    │                  │  data plane     │      │
│   └─────────────────┘                  └─────────────────┘      │
└─────────────────────────────────────────────────────────────────┘
                                          ▲
                                          │ policy decisions per request
                                          │
                                  ┌─────────────────┐
                                  │ external models │
                                  │ (Anthropic /    │
                                  │  OpenAI /       │
                                  │  LiteLLM)       │
                                  └─────────────────┘

       ┌────────────────────────────────────────────────────┐
       │ Apple Silicon host (optional, on the LAN/VPN)      │
       │                                                    │
       │   metal-agent ──supervises──▶ llama-server          │
       │      (native macOS daemon)    (Metal GPU access)   │
       │                                                    │
       │   metal-agent ──registers──▶ Kubernetes Endpoints  │
       │       (host IP + allocated port)                   │
       └────────────────────────────────────────────────────┘
```

**The in-cluster controller** is the source of truth for desired
state. It watches the CRDs, schedules runtime pods (for NVIDIA /
CPU workloads), creates the `Service` / `ConfigMap` / `Deployment`
for `ModelRouter` data planes, and writes back status.

**The metal-agent** is a native macOS daemon that runs *outside*
the cluster on Apple Silicon hosts. It also watches the
Kubernetes API for `InferenceService` resources with
`accelerator: metal`, but instead of scheduling a Pod it spawns
`llama-server` natively on the Mac with full Metal GPU access,
then registers a Kubernetes `Endpoints` object pointing at the
Mac's host IP. Any pod in the cluster can route to the Mac via
the resulting `Service` URL.

The two cooperate without overlap: the in-cluster controller
manages everything *inside* Kubernetes (CRD status, container
pods, Services, ConfigMaps); the metal-agent manages everything
*outside* (native processes, Metal context, host memory
pressure). The CRD is the protocol between them.

See [`macOS Metal Agent`](../guides/macos-metal) for the
agent's install and tuning.

## The data plane

There are two data planes a request can hit, depending on whether
`ModelRouter` is in use:

1. **Direct InferenceService**: a client (your application, an
   agent runtime) talks straight to the `InferenceService`'s
   `Service` URL. That URL is OpenAI-compatible and points at
   either a Pod (NVIDIA / CPU) or a metal-agent-registered
   Endpoint (Apple Silicon). No policy layer.

2. **Through ModelRouter**: the client talks to the
   `ModelRouter`'s `Service` URL. The router-proxy pod
   resolves the request's classification, task complexity,
   capabilities, and headers against the router's rules, picks a
   backend, and dispatches to either an in-cluster
   `InferenceService` *or* an external provider (Anthropic /
   OpenAI / LiteLLM / Bedrock / Vertex). Streaming SSE
   passthrough; fail-closed semantics enforced at the proxy
   for regulated-data rules.

Pick (1) when an agent or app just needs one model. Pick (2) when
you have multiple models, mixed local + external providers, or
data classifications that require policy enforcement. The two
shapes compose: you can have direct clients hitting some
InferenceServices and policy-routed clients hitting the same
InferenceServices through a ModelRouter.

## Streaming, classification, and audit

The proxy emits a structured audit log entry per request:

```json
{
  "msg": "router.dispatch",
  "rule": "pii-stays-local",
  "backend": "local-coder",
  "backendTier": "local",
  "status": 200,
  "outcome": "ok",
  "latencyMs": 4271,
  "timeoutMs": 8000
}
```

Every routing decision is recoverable from the log. Compliance
audits, "why did this 503", and per-rule budget verification all
trace back to those records. Pair with Prometheus metrics
(planned in the next observability pass) and the OTel tracing the
controller already emits to Tempo for full visibility.

## Why this shape

A few decisions worth being explicit about:

- **The control plane is inside the cluster, not on a Mac.**
  Apple Silicon serves traffic; it doesn't manage state.
  Restarting your Mac doesn't lose any CRD or controller state.
- **The router-proxy is a managed Deployment, not a service-mesh
  Filter.** It runs as a normal pod the controller owns. No
  service-mesh dependency, air-gap-friendly, and policy stays in
  Go where we control it.
- **Cross-engine routing is a separate CRD, not a flag on
  `InferenceService`.** That keeps the simple case simple (just
  declare a model, get a serving endpoint) and lets the policy
  case grow without overloading the inference surface.
- **The data classification is read from a request header, not
  inferred.** Operators can opt in to classifier sidecars later;
  the v1alpha1 contract is "the caller asserts the
  classification" so the proxy never has to be the source of
  truth for what data is sensitive.

## Where things live in the repo

| Component | Source |
|---|---|
| Controller binary | `cmd/main.go`, `internal/controller/**` |
| CRD types | `api/v1alpha1/*.go` |
| metal-agent binary | `cmd/metal-agent/main.go`, `pkg/agent/**` |
| router-proxy binary | `cmd/router-proxy/main.go`, `internal/router/**` |
| Helm chart | `charts/llmkube/` |
| CRD manifests (generated) | `config/crd/bases/`, `charts/llmkube/templates/crds/` |
| Sample CRs | `config/samples/` |

## Next steps

- [`CRD reference`](./crds) for every field on every resource
- [`Model Router`](./model-router) for the policy / fail-closed
  semantics covered briefly above
- [`How LLMKube compares`](./comparison) versus vLLM, Ollama,
  KServe, LocalAI, KubeAI, llm-d, LiteLLM
- [`Install in 5 minutes`](../getting-started) to provision a
  cluster and try this end-to-end
