# TurboQuant KV cache types

LLMKube's `cacheTypeK` and `cacheTypeV` fields on `InferenceService` validate against an enum of standard llama.cpp cache types: `f16`, `f32`, `q8_0`, `q4_0`, `q4_1`, `q5_0`, `q5_1`, `iq4_nl`. Custom llama.cpp builds add types beyond this set — most notably **TurboQuant** ([Google Research, ICLR 2026](https://research.google/blog/turboquant-redefining-ai-efficiency-with-extreme-compression/)) which adds `turbo2`, `turbo3`, `turbo4`, and several community variants (`tbq3`, `tbqp3`, etc.).

To use a non-enum cache type without expanding the CRD enum every time a new one appears upstream, set the **`cacheTypeCustomK`** / **`cacheTypeCustomV`** fields:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: qwen35-256k
spec:
  modelRef: qwen3-5-30b-a3b
  runtime: llamacpp
  contextSize: 262144
  flashAttention: true
  cacheTypeCustomK: turbo3
  cacheTypeCustomV: turbo3
```

LLMKube does not validate the string. The runtime binary must understand the value or `llama-server` will fail to start with a clear error.

## Precedence

When both `cacheTypeK` and `cacheTypeCustomK` are set on the same spec, **`cacheTypeCustomK` wins**. Same for V. This lets a manifest declare a standard type as a fallback while opting into a fork-specific type when one is available.

## When to use which field

| Field | Use when |
|---|---|
| `cacheTypeK` / `cacheTypeV` | The cache type is in the standard llama.cpp enum (q8_0, iq4_nl, etc.). Gives you API-server-side validation that catches typos. |
| `cacheTypeCustomK` / `cacheTypeCustomV` | The cache type is from a fork (TurboQuant turbo3, AmesianX tbqp3, etc.). No CRD validation, but lets you use any future cache format without an LLMKube release. |

## Required runtime

For TurboQuant types specifically, the `llama-server` binary must be built from a fork that ships the kernels:

| Backend | Fork | Branch |
|---|---|---|
| Metal (Apple Silicon) | [TheTom/llama-cpp-turboquant](https://github.com/TheTom/llama-cpp-turboquant) | `feature/turboquant-kv-cache` |
| CUDA (RTX 30/40/50-series) | [spiritbuun/llama-cpp-turboquant-cuda](https://github.com/spiritbuun/llama-cpp-turboquant-cuda) | upstream-tracking |
| Vulkan (AMD/Intel) | jesusmb1995's fork (see discussion below) | mixed K/V types |

For Mac, point the metal-agent at the fork via the `--llama-server` flag in your launchd plist.

## Memory savings

Approximate KV cache size per token at 35B-class models, fp16 reference = `~256 KB/token`:

| Cache type | bits/value | Compression | KV size at 256K context |
|---|---|---|---|
| `f16` (default) | 16 | 1.0× | ~64 GB |
| `q8_0` | 8 | 2.0× | ~32 GB |
| `turbo4` | 4.25 | 3.8× | ~17 GB |
| `turbo3` | 3.25 | 4.9× | ~13 GB |
| `turbo2` | 2.5 | ~6.4× | ~10 GB |

Perplexity penalty for `turbo3` is around +1% on Qwen 3.6-class models per the upstream community discussion.

## Pre-flight memory check

The Metal agent's pre-flight memory check uses the configured `cacheTypeK` and `cacheTypeV` (or their `Custom` variants) when estimating per-process memory. Without this, the check would always assume `f16` and reject TurboQuant configs that actually fit. The bytes-per-element table for each cache type lives next to the estimator at `pkg/agent/memory.go` (`cacheTypeBytesPerElement`); add a new entry there if you ship a fork with additional types. Unknown types fall back to `f16` so the estimate over-allocates rather than under, which is the safer default for a pre-flight gate.

## Asymmetric K and V

`cacheTypeCustomK` and `cacheTypeCustomV` are independent. Setting only one is supported; mixing is too. Community guidance (see [renjithvr11.medium.com on this](https://renjithvr11.medium.com/)) suggests `q8_0` for K and `turbo4` for V on agentic workloads, on the theory that the K side is more sensitive to quantization than V. We have not run this in our own benchmarks yet — the M5 Max numbers below are all symmetric K=V.

## References

- llama.cpp community discussion: <https://github.com/ggml-org/llama.cpp/discussions/20969>
- TurboQuant paper: <https://arxiv.org/abs/2504.19874>
- M5 Max benchmark across f16/q8_0/turbo3/turbo4 from 0 to 1M context: <https://llmkube.com/blog/turboquant-m5-max-long-context>
- LLMKube tracking issue: [#308](https://github.com/defilantech/LLMKube/issues/308)
- The CRD field decision: [#282](https://github.com/defilantech/LLMKube/issues/282)
