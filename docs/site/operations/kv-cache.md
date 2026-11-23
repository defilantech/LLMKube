---
title: "KV cache types"
description: "Pick a KV cache type for llama.cpp and vLLM: f16, q8_0, iq4_nl, TurboQuant turbo3 and turbo4, fp8, and the vLLM turboquant types, and the InferenceService fields that set them."
---

# KV cache types across runtimes

LLMKube exposes KV cache quantization for both supported runtimes via different fields, because the runtimes themselves expose different sets of cache types and use different flag conventions. This page is the single reference for picking a value.

## Why KV cache type matters

Long-context inference is bandwidth-bound on the KV cache, not the weights. At fp16 (the default), a 30B-class model's KV cache at 256K context is ~64 GB: over budget for most consumer GPUs and well over the M5 Max's effective per-process memory ceiling. Quantized cache types trade a small perplexity penalty for a large memory and bandwidth win, which directly translates into either higher concurrency at the same context or longer context at the same hardware.

| Backend | Cache type | bits/value | KV @ 128K (30B-class) | Use when |
|---|---|---|---|---|
| llama.cpp | f16 (default) | 16 | ~32 GB | Reference / baseline |
| llama.cpp | q8_0 | 8.5 | ~17 GB | Conservative quant, broadly supported |
| llama.cpp | iq4_nl | 4.5 | ~9 GB | Aggressive but still standard |
| llama.cpp | turbo4 (fork) | 4.25 | ~8.5 GB | Decode-heavy agentic workloads on Metal |
| llama.cpp | turbo3 (fork) | 3.25 | ~6.5 GB | Long-context prefill on Metal |
| vLLM | auto (default) | follows dtype | varies | Reference / baseline |
| vLLM | fp8 / fp8_e4m3 | 8 | ~16 GB | Production agentic on H100/L4 |
| vLLM | fp8_e5m2 | 8 | ~16 GB | Same scale, different exponent bias |
| vLLM | float16 | 16 | ~32 GB | Explicit 16-bit cache |
| vLLM | bfloat16 | 16 | ~32 GB | Opt-out for a model whose cache defaults to fp8 |
| vLLM | turboquant_k8v4 (v0.20+) | ~6 | ~12 GB | TurboQuant with the smallest accuracy cost |
| vLLM | turboquant_3bit_nc (v0.20+) | ~3.25 | ~6.5 GB | Maximum context per VRAM |

## Two runtimes, two fields each

Each runtime has both a standard enum-validated field and a custom-string escape hatch. The custom field always wins when both are set.

### llama.cpp

```yaml
spec:
  cacheTypeK: q8_0     # enum-validated: f16, f32, q8_0, q4_0, q4_1, q5_0, q5_1, iq4_nl
  cacheTypeV: q8_0     # same enum

  # Or the custom escape hatch (TurboQuant turbo3/turbo4, AmesianX tbqp3, etc.)
  cacheTypeCustomK: turbo3
  cacheTypeCustomV: turbo3
```

K and V are independent. Asymmetric `cacheTypeK: q8_0` + `cacheTypeCustomV: turbo4` is supported and is the recommended config from community benchmarks for some agentic workloads.

### vLLM

```yaml
spec:
  vllmConfig:
    kvCacheDtype: fp8              # enum: auto, fp8_e5m2, fp8_e4m3, fp8, float16, bfloat16
    # Or the custom escape hatch (the turboquant_* types from vLLM v0.20+, future variants)
    kvCacheCustomDtype: turboquant_3bit_nc
```

vLLM does not split K and V into separate flags: `--kv-cache-dtype` controls both.

`fp8` is the spelling vLLM's own recipes use, and vLLM resolves it per platform: it is `fp8_e4m3` on CUDA and ROCm and `fp8_inc` on Intel Gaudi. LLMKube passes whichever value you set through unchanged rather than rewriting the alias. `float16` pins a 16-bit cache. `bfloat16` is not a general-purpose unquantized cache: it is how you opt out of an fp8 default on the models that set one (DeepSeek-V3.2 and its descendants), and vLLM documents it as an invalid option for models that do not default to fp8. An fp8 KV cache takes its scales from the checkpoint; without them vLLM assumes scales of 1.0, which can cost accuracy, so prefer a checkpoint whose scales are calibrated.

## When to use which field

| You're using | Use |
|---|---|
| A standard llama.cpp cache type (q8_0, iq4_nl, etc.) | `cacheTypeK` / `cacheTypeV` (enum-validated, catches typos at API-server admission) |
| TurboQuant turbo3/turbo4 on Metal or AmesianX tbqp3 on CUDA | `cacheTypeCustomK` / `cacheTypeCustomV` |
| Standard vLLM auto/fp8/fp8_*/float16/bfloat16 | `kvCacheDtype` (enum-validated) |
| `fp8_ds_mla`, `nvfp4`, the `turboquant_*` and `*_per_token_head` types, or any value newer than your vLLM image's enum | `kvCacheCustomDtype` |

The custom fields skip CRD enum validation. The runtime image / binary must understand the value or the underlying server fails to start with a clear error. LLMKube does not pre-validate the strings so it can support any fork-specific value without an LLMKube release.

## Cross-runtime memory comparison

These numbers are approximate. Real measurements depend on the model's attention pattern (DeltaNet, GQA ratio, head count) and the runtime's framing overhead.

| Runtime | Backend | Cache | Ratio vs f16 (lower = more compression) |
|---|---|---|---|
| llama.cpp | Metal (M5 Max) | f16 | 1.000 |
| llama.cpp | Metal (M5 Max) | q8_0 | 0.531 |
| llama.cpp | Metal (M5 Max) | turbo4 | 0.266 |
| llama.cpp | Metal (M5 Max) | turbo3 | 0.203 |
| vLLM | CUDA (RTX 5060 Ti) | fp8_e4m3 | 0.500 |

The Metal-side numbers come from the [TurboQuant on M5 Max bench](https://llmkube.com/blog/turboquant-m5-max-long-context). LLMKube has not measured vLLM's `turboquant_*` types; the vLLM rows in the first table derive from the compression ratios in [vLLM PR #38479](https://github.com/vllm-project/vllm/pull/38479).

## Required runtime images

For non-enum types, the runtime image must include the implementation:

| Backend | Cache type | Image / branch |
|---|---|---|
| Metal (Apple Silicon) | turbo3, turbo4 | [TheTom/llama-cpp-turboquant](https://github.com/TheTom/llama-cpp-turboquant) `feature/turboquant-kv-cache` |
| CUDA (NVIDIA) | tbqp3 | [spiritbuun/llama-cpp-turboquant-cuda](https://github.com/spiritbuun/llama-cpp-turboquant-cuda) |
| vLLM | turboquant_k8v4, turboquant_4bit_nc, turboquant_k3v4_nc, turboquant_3bit_nc | `vllm/vllm-openai:v0.20.0` and later |

For Metal, point the metal-agent at the fork via the `--llama-server` flag in your launchd plist (see [docs/contributors/turboquant.md](https://github.com/defilantech/LLMKube/blob/main/docs/contributors/turboquant.md)).

## References

- llama.cpp KV cache discussion: <https://github.com/ggml-org/llama.cpp/discussions/20969>
- vLLM TurboQuant PR: <https://github.com/vllm-project/vllm/pull/38479>
- vLLM v0.20.0 release notes: <https://github.com/vllm-project/vllm/releases/tag/v0.20.0>
- Llama.cpp-side details: [docs/contributors/turboquant.md](https://github.com/defilantech/LLMKube/blob/main/docs/contributors/turboquant.md)
- Cross-architecture bench: <https://llmkube.com/blog/turboquant-m5-max-long-context>
