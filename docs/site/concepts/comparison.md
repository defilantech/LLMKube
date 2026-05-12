---
title: How LLMKube compares
description: Honest comparison of LLMKube against KubeAI, llm-d, Ollama, and NVIDIA NIM. Where each one fits and where they don't.
updated: 2026-05-03
---

# How LLMKube compares

LLMKube is the only Kubernetes operator we've found that runs native Apple Silicon processes alongside Linux GPU pods, with per-process memory-pressure protection built in. For everything else listed below, another project on this page is the better choice.

<DocCallout variant="note" title="Read this first">

Honest comparison, not a sales pitch. We use vLLM, llama.cpp, and TGI as runtimes ourselves: they're complementary, not competitors. The peers below are operators and platforms that, like us, sit one layer up.

</DocCallout>

## Quick orientation

**Inference engines** ([vLLM](https://github.com/vllm-project/vllm), [llama.cpp](https://github.com/ggerganov/llama.cpp), [TGI](https://github.com/huggingface/text-generation-inference), [SGLang](https://github.com/sgl-project/sglang), [MLX](https://github.com/ml-explore/mlx)) run a single model behind an HTTP server. They're the substrate; LLMKube wraps them.

**Single-node runners** ([Ollama](https://ollama.com/), LM Studio) are best-in-class for "run a model on your laptop." Not a fit when you need Kubernetes-native scheduling, NetworkPolicy, HPA, multi-tenant namespaces, or a fleet of GPU nodes.

**Kubernetes operators and platforms** ([KubeAI](https://www.kubeai.org/), [llm-d](https://llm-d.ai/), [NVIDIA NIM Operator](https://docs.nvidia.com/nim-operator/latest/)) are the actual peers. The table below focuses here.

## Capability matrix

| Project | K8s-native | Apple Silicon | Memory-pressure protection | Engines | Multi-GPU | License |
|---|---|---|---|---|---|---|
| **LLMKube** | ✓ operator + Model and InferenceService CRDs | ✓ native via the metal-agent (no containers) | ✓ watchdog, priority-based eviction, per-service opt-out | llama.cpp, vLLM, TGI, oMLX, Ollama | Layer-based sharding across GPUs on a node | Apache 2.0 |
| **KubeAI** | ✓ operator + Model CRD | — Linux containers only | — | vLLM, Ollama, Faster-Whisper, Infinity (embeddings) | Multi-GPU pods via `resourceProfile`, tensor-parallel args passed to vLLM | Apache 2.0 |
| **llm-d** | ✓ Helm + Gateway API + `InferencePool` | — datacenter accelerators only (NVIDIA, AMD, Intel, TPU) | — | vLLM (primary), SGLang | Wide expert parallelism + disaggregated multi-node serving | Apache 2.0 |
| **Ollama** | — single binary on a host | ✓ native (its sweet spot) | — | Forked llama.cpp (GGUF), MLX engine (Safetensors, macOS) | Automatic spreading across GPUs on one node | MIT |
| **NVIDIA NIM Operator** | ✓ CRDs + Helm + KServe integration | — | — | TensorRT-LLM (primary), vLLM (select models), Triton serving | Tensor parallelism via TRT-LLM, multi-node via the Operator | Operator: Apache 2.0. Runtime: NVIDIA AI Enterprise (free cloud tier on build.nvidia.com) |

Verified against project repos and official docs. If you spot anything inaccurate, please [open an issue](https://github.com/defilantech/LLMKube/issues/new?title=Comparison+page+correction).

## When to pick something else

**Pick Ollama if** you have one developer machine, you want chat in 90 seconds, and you don't need Kubernetes-style operations (RBAC, NetworkPolicy, multi-tenant namespaces, a Service mesh, a fleet of nodes). Ollama is the right answer here and we use it during development. It also now ships an MLX engine for Safetensor models on Apple Silicon and a hosted-cloud option for models that don't fit locally.

**Pick llm-d if** you're standing up a multi-node vLLM serving fleet with disaggregated prefill/decode, wide expert parallelism, or tiered KV-cache offload, and you want the Red Hat / IBM / Google production lineage. llm-d's prefill-decode disaggregation, KV-cache tiering, and inference-aware routing are deeper than what we offer for that specific topology. We're the better answer for mixed runtimes (llama.cpp + vLLM + Ollama under one CRD) and for Apple Silicon.

**Pick KubeAI if** you want a Kubernetes operator that ships with embeddings (Infinity), reranking, speech (Faster-Whisper), and LoRA hot-loading alongside LLM serving, and you don't need Apple Silicon or per-process memory protection. KubeAI's scale-to-zero with request queuing, prefix-hash load balancing for KV-cache locality, and Kafka-trigger autoscaling are more operationally polished than ours today.

**Pick the NIM Operator if** you're in an enterprise running NVIDIA AI Enterprise for self-hosted inference and want NVIDIA's optimized model containers with vendor support and a KServe-compatible deployment path. Individual developers can also use the free hosted API tier at [build.nvidia.com](https://build.nvidia.com) for prototyping. The proprietary self-hosted licensing and NVIDIA-only hardware are the tradeoff.

## What's distinctive

Three things LLMKube does that, as a combination, no other project on this list does:

1. **Apple Silicon participates in the same control plane as your GPU nodes.** The metal-agent runs as a native macOS process, supervises llama-server, oMLX, or Ollama directly on the host, and registers Endpoints back into the cluster. A Mac mini and an L4 node both look like `InferenceService` objects to `kubectl`. Ollama is great on a single Mac but doesn't speak Kubernetes; the K8s operators don't run native Mac processes.
2. **One operator, mixed runtimes across heterogeneous hosts.** The `runtime` field on InferenceService selects llama.cpp, vLLM, TGI, oMLX, or Ollama. Mix runtimes on the same cluster, mix Linux GPU nodes with Mac hosts, without standing up a different operator per topology.
3. **Per-process memory-pressure protection** with priority-aware eviction, per-service opt-out, and a friendly-fire guard so the watchdog doesn't kill your workloads when the pressure is from outside LLMKube. [See the live demo](/docs/memory-pressure-protection).

---

**Related:** [Architecture](/docs/concepts/architecture) · [CRD reference](/docs/concepts/crds) · [Install in 5 minutes](/docs/getting-started)
