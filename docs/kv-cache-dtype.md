# KV cache types across runtimes

LLMKube exposes KV cache quantization for both supported runtimes via different fields, because the runtimes themselves expose different sets of cache types and use different flag conventions. This page is the single reference for picking a value.

## Why KV cache type matters

Long-context inference is bandwidth-bound on the KV cache, not the weights. At fp16 (the default), a 30B-class model's KV cache at 256K context is ~64 GB — over budget for most consumer GPUs and well over the M5 Max's effective per-process memory ceiling. Quantized cache types trade a small perplexity penalty for a large memory and bandwidth win, which directly translates into either higher concurrency at the same context or longer context at the same hardware.

| Backend | Cache type | bits/value | KV @ 128K (30B-class) | Use when |
|---|---|---|---|---|
| llama.cpp | f16 (default) | 16 | ~32 GB | Reference / baseline |
| llama.cpp | q8_0 | 8.5 | ~17 GB | Conservative quant, broadly supported |
| llama.cpp | iq4_nl | 4.5 | ~9 GB | Aggressive but still standard |
| llama.cpp | turbo4 (fork) | 4.25 | ~8.5 GB | Decode-heavy agentic workloads on Metal |
| llama.cpp | turbo3 (fork) | 3.25 | ~6.5 GB | Long-context prefill on Metal |
| vLLM | auto (default) | follows dtype | varies | Reference / baseline |
| vLLM | fp8_e4m3 | 8 | ~16 GB | Production agentic on H100/L4 |
| vLLM | fp8_e5m2 | 8 | ~16 GB | Same scale, different exponent bias |
| vLLM | turbo2 (v0.20+) | 2 | ~4 GB | Maximum context per VRAM |

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
    kvCacheDtype: fp8_e4m3        # enum: auto, fp8_e5m2, fp8_e4m3
    # Or the custom escape hatch (turbo2 from vLLM v0.20+, future variants)
    kvCacheCustomDtype: turbo2
```

vLLM does not split K and V into separate flags — `--kv-cache-dtype` controls both.

## When to use which field

| You're using | Use |
|---|---|
| A standard llama.cpp cache type (q8_0, iq4_nl, etc.) | `cacheTypeK` / `cacheTypeV` (enum-validated, catches typos at API-server admission) |
| TurboQuant turbo3/turbo4 on Metal or AmesianX tbqp3 on CUDA | `cacheTypeCustomK` / `cacheTypeCustomV` |
| Standard vLLM auto/fp8 | `kvCacheDtype` (enum-validated) |
| TurboQuant turbo2 on vLLM v0.20+, or any future cache format | `kvCacheCustomDtype` |

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
| vLLM | CUDA (RTX 5060 Ti) | turbo2 | 0.125 |

The Metal-side numbers come from the [TurboQuant on M5 Max bench](https://llmkube.com/blog/turboquant-m5-max-long-context). The vLLM-side `turbo2` row will be backed by the ShadowStack bench tracked under issue #354.

## Required runtime images

For non-enum types, the runtime image must include the implementation:

| Backend | Cache type | Image / branch |
|---|---|---|
| Metal (Apple Silicon) | turbo3, turbo4 | [TheTom/llama-cpp-turboquant](https://github.com/TheTom/llama-cpp-turboquant) `feature/turboquant-kv-cache` |
| CUDA (NVIDIA) | tbqp3 | [spiritbuun/llama-cpp-turboquant-cuda](https://github.com/spiritbuun/llama-cpp-turboquant-cuda) |
| vLLM | turbo2 | `vllm/vllm-openai:v0.20.0` and later |

For Metal, point the metal-agent at the fork via the `--llama-server` flag in your launchd plist (see `docs/turboquant.md`).

## References

- llama.cpp KV cache discussion: <https://github.com/ggml-org/llama.cpp/discussions/20969>
- vLLM TurboQuant 2-bit PR: <https://github.com/vllm-project/vllm/pull/38479>
- vLLM v0.20.0 release notes: <https://github.com/vllm-project/vllm/releases/tag/v0.20.0>
- Llama.cpp-side details: [docs/turboquant.md](./turboquant.md)
- Cross-architecture bench: <https://llmkube.com/blog/turboquant-m5-max-long-context>
