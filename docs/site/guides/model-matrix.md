# Consumer-hardware model matrix

> **Snapshot dated 2026-05-11. Not actively maintained.**
>
> The local LLM landscape changes by the week. New model releases routinely supersede the ones in this table within a month or two, file sizes shift as quanters re-upload with better imatrix calibration, and license terms occasionally tighten. Treat this document as a starting point, not a source of truth. Before you download anything large, click through to the upstream HuggingFace model card and verify the current size, context length, and license for yourself.
>
> If you spot something out of date, a pull request is welcome. We do not promise to keep this current.

A practical guide to picking a GGUF model for [LLMKube](https://llmkube.com) based on the hardware you actually have. Every file size in this doc was verified directly from HuggingFace on 2026-05-11. Capability claims come from the upstream model cards (linked throughout), not third-party quanters.

## How to read the tables

**File size** is the raw GGUF size on disk. To actually run the model with a usable context you need additional headroom for the KV cache, runtime overhead, and OS. The rough rule:

```
required memory = gguf_file_size * 1.20  +  kv_cache(ctx_length)
```

For most chat workloads at 8K to 16K context, **add 20 to 30 percent to the file size** and you will be close. Long context windows (128K+) or fp16 KV cache push this much higher. The KV cache headroom section below covers the math, and the LLMKube [KV cache types](/docs/operations/kv-cache) and [Memory-pressure protection](/docs/memory-pressure-protection) docs document the runtime knobs.

**Quant choice.** `Q4_K_M` and `IQ4_XS` are the sweet spot for most users: good quality, smallest size that still feels like the original. `Q5_K_M` and `Q6_K` cost more memory but get closer to fp16 quality. `Q8_0` is near lossless at roughly 1 byte per parameter. The Unsloth `UD-*_XL` variants are dynamic quants that mix bit widths smartly; usually preferred when available.

**MoE models** (Qwen3 Coder, Qwen3.5/3.6 A3B, gpt-oss-20b, gpt-oss-120b) are bigger on disk than their throughput suggests, because only a small subset of experts is active per token. They still need the full file resident in memory, but they **generate as fast as a much smaller dense model**. A 35B MoE with 3B active params loads like a 22 GB model at Q4 and generates like a 3B dense.

## Hardware tiers

| Tier | Usable memory | Example hardware |
|------|---------------|------------------|
| 1: Edge | up to 8 GB | RTX 3060 8GB, RTX 4060 8GB, MacBook Air M2/M3 8GB, Jetson Orin Nano, Steam Deck, integrated graphics |
| 2: Entry | 12 to 16 GB | RTX 3060 12GB, RTX 4060 Ti 16GB, RTX 4070 12GB, MacBook Pro M2/M3 Pro 16/18GB |
| 3: Enthusiast | 24 to 32 GB | RTX 3090 24GB, RTX 4090 24GB, RTX 5090 32GB, MacBook Pro M2/M3 Max 32GB, Mac Mini M4 24GB |
| 4: Pro single-node | 48 to 64 GB | RTX 6000 Ada 48GB, 2x RTX 3090, MacBook Pro M3/M4 Max 48/64GB, Mac Studio M2 Max 64GB |
| 5: Workstation | 96 to 128 GB | MacBook Pro M3/M4 Max 96/128GB, Mac Studio M2 Ultra, 2x RTX 4090/5090, RTX 6000 Pro 96GB |
| 6: Multi-GPU / Ultra | 192 GB and up | Mac Studio M3/M4 Ultra 192/256/512GB, 4x RTX 5090, multi-RTX 6000 Pro, DGX-class |

In LLMKube specifically, **multi-GPU sharding** lets you span tiers by adding GPUs. A 70B at Q4_K_M (about 40 GB) does not fit a single 24 GB RTX 4090, but it shards cleanly across 2x RTX 4090 with `gpuCount: 2`. See [Multi-GPU sharding](/docs/guides/multi-gpu).

## Mac unified memory: usable vs advertised

Apple Silicon shares one memory pool between CPU and GPU. That is an advantage (no copy, no VRAM split) but means macOS and background processes are eating into the same pool you want to load a model into.

| Advertised RAM | Usable for the model + KV | Realistic Q4_K_M ceiling |
|----------------|--------------------------|-------------------------|
| 8 GB | ~5 to 6 GB | 3B to 4B models |
| 16 GB | ~12 to 13 GB | up to 12B to 14B |
| 24 GB | ~20 to 21 GB | up to 20B to 27B |
| 32 GB | ~28 to 29 GB | up to a 35B MoE |
| 48 GB | ~44 to 45 GB | 35B at Q8, some 70B at extreme quants |
| 64 GB | ~60 GB | 70B at Q4_K_M (tight), 35B at Q8 |
| 96 GB+ | ~90 GB and up | gpt-oss-120b MXFP4, 70B at Q6, room for 128K context |

**Inference speed on Apple Silicon is bandwidth-limited.** Memory bandwidth, not core count, is the bottleneck:

| Chip | Memory bandwidth | Notes |
|------|------------------|-------|
| M3 / M4 base | ~100 to 120 GB/s | Fine for 4B to 7B at Q4 |
| M3 / M4 Pro | ~270 to 300 GB/s | Comfortable through 13B to 14B |
| M3 / M4 Max | ~400 to 540 GB/s | The sweet spot for 30B MoE workloads |
| M3 / M4 Ultra | ~800 GB/s+ | Frontier-class single-node hardware |

**Rough token/sec ballparks at Q4_K_M** (interactive use):

| Model size | M2/M3 base | M2/M3 Pro | M3/M4 Max |
|------------|-----------|-----------|-----------|
| 4B dense | 25 to 35 | 35 to 50 | 50 to 80 |
| 8B dense | 15 to 25 | 25 to 40 | 40 to 60 |
| 14B dense | 8 to 15 | 12 to 20 | 20 to 35 |
| 30B MoE / 3B active | 15 to 25 | 25 to 35 | 30 to 50 |
| 70B dense | does not fit | does not fit | 5 to 10 |

These are ballparks. Real numbers depend on context length, llama.cpp build flags, and other apps competing for memory bandwidth. Treat them as "is this interactive" not as benchmarks.

## KV cache headroom

The KV cache grows roughly linearly with context length. As a back-of-envelope at Q4_K_M weights:

| Model class | 8K context | 32K context | 128K context |
|-------------|-----------|-------------|--------------|
| 7B to 8B dense | ~0.5 to 1 GB | ~2 to 4 GB | ~8 to 16 GB |
| 13B to 14B dense | ~1 to 2 GB | ~4 to 8 GB | ~16 to 24 GB |
| 30B MoE | ~1 to 2 GB | ~4 to 8 GB | ~16 to 24 GB |
| 70B dense | ~2 to 4 GB | ~8 to 16 GB | does not fit on consumer hw |

**Practical examples:**

- A 17 GB model on a 24 GB GPU leaves about 7 GB for KV cache. Fine for 8K to 16K context. Not enough for 128K.
- gpt-oss-20b (10.8 GB) on a 16 GB card leaves about 5 GB. Comfortable at 16K. Tight at 32K.
- Llama 3.3 70B Q4_K_M (39.6 GB) sharded across 2x RTX 4090 (48 GB total) leaves about 8 GB for KV. Stay under 32K context unless you switch to a Q4 KV cache type.

LLMKube exposes the KV cache dtype as a Model CRD field; see [KV cache types](/docs/operations/kv-cache) for the trade-off (Q4 cache halves memory at a small quality cost).

## Multi-GPU notes

llama.cpp supports multi-GPU inference via layer splitting. LLMKube wraps that with `gpuCount` on the Model CRD.

- **Two 12 GB cards** can run a 20B to 24B model (10 to 12 GB of weights per GPU plus KV).
- **Two 24 GB cards** comfortably hold a 70B at Q4_K_M (about 40 GB of weights split across two devices).
- **PCIe communication adds latency.** For a given total VRAM budget, **one bigger card beats two smaller cards** in tokens/sec. Pick multi-GPU when you need the model to fit, not for speed.
- **Sharding shape matters.** Layer-split (the default in llama.cpp and LLMKube) is simpler than tensor-parallel and works well for inference. Tensor-parallel needs NVLink-class interconnect to actually help.

## The matrix, by hardware tier

### Tier 1: Edge devices (up to 8 GB)

| Model | Best for | Recommended quant | File size | License | Context | Notes |
|-------|----------|-------------------|-----------|---------|---------|-------|
| [Llama 3.2 1B Instruct](https://huggingface.co/meta-llama/Llama-3.2-1B-Instruct) | Classification, summarization, simple Q&A, autocomplete | Q8_0 | 1.23 GB | Llama 3.2 | 128K | Multilingual (8 languages), good for on-device. |
| [Llama 3.2 3B Instruct](https://huggingface.co/meta-llama/Llama-3.2-3B-Instruct) | Lightweight chat, RAG retrieval, function-call routing | Q4_K_M | 1.88 GB | Llama 3.2 | 128K | The smallest model that still feels like an assistant. Q6_K (2.46 GB) if you have room. |
| [Gemma 3 4B IT](https://huggingface.co/google/gemma-3-4b-it) | Vision + text on edge, multilingual chat | Q4_K_M | 2.32 GB | Gemma | 128K | Multimodal (vision); 140+ languages. Add ~0.85 GB for the mmproj file. |
| [Qwen3.5 4B](https://huggingface.co/Qwen/Qwen3.5-4B) | Long-context summarization, vision, edge agents | UD-Q4_K_XL | 2.91 GB | Apache 2.0 | 256K native, 1M with YaRN | Vision-language, thinking mode, hybrid MoE + DeltaNet. Add mmproj ~0.67 GB for vision. |
| [Phi-4-mini Instruct](https://huggingface.co/microsoft/Phi-4-mini-instruct) | On-device math, function calling, structured output | Q4_K_M | ~2.5 GB | MIT | 128K | Strongest small model for tool use. |
| [Granite 4.1 3B](https://huggingface.co/ibm-granite/granite-4.1-3b) | Enterprise RAG / tool use on tight memory | Q4_K_M | ~2 GB | Apache 2.0 | 128K | Mamba-Transformer hybrid; cheap KV cache at long context. |

### Tier 2: Entry GPUs / laptops (12 to 16 GB)

| Model | Best for | Recommended quant | File size | License | Context | Notes |
|-------|----------|-------------------|-----------|---------|---------|-------|
| [Llama 3.1 8B Instruct](https://huggingface.co/meta-llama/Llama-3.1-8B-Instruct) | General-purpose chat baseline, RAG, instruction following | Q4_K_M | 4.58 GB | Llama 3.1 | 128K | The default workhorse if in doubt. Q5_K_M (5.34 GB) for higher quality. |
| [Granite 4.1 8B](https://huggingface.co/ibm-granite/granite-4.1-8b) | Enterprise tool use, RAG, function calling, fill-in-the-middle code | Q4_K_M | 4.98 GB | Apache 2.0 | 128K | BFCL v3 of 68.27, HumanEval 85.4. Strong for agentic backends. |
| [Qwen3.5 9B](https://huggingface.co/Qwen/Qwen3.5-9B) | Vision + reasoning + long context in one package | Q4_K_M | 5.29 GB | Apache 2.0 | 256K native, 1M with YaRN | Thinking mode by default; vision via mmproj; 201 languages. |
| [Gemma 3 12B IT](https://huggingface.co/google/gemma-3-12b-it) | Vision-language tasks, multilingual chat, document QA | Q4_K_M | 7.30 GB | Gemma | 128K | 140+ languages. Add ~0.85 GB for mmproj. |
| [DeepSeek R1 Distill Qwen 14B](https://huggingface.co/deepseek-ai/DeepSeek-R1-Distill-Qwen-14B) | Step-by-step reasoning, math, multi-step problems | Q4_K_M | 8.99 GB | MIT | 128K | Produces visible thinking traces. Q5_K_M (10.51 GB) fits 16 GB. |
| [Phi-4 14B](https://huggingface.co/microsoft/phi-4) | Math, code, STEM reasoning on a tight memory budget | Q4_K_M | 8.43 GB | MIT | 16K | MATH 80.4, HumanEval 82.6, GPQA 56.1. Shorter context than peers. |
| [gpt-oss-20b](https://huggingface.co/openai/gpt-oss-20b) | Agentic reasoning, function calling, Python execution | Q4_K_M | 10.83 GB | Apache 2.0 | 128K | MoE: 20B total / 3.6B active. SWE-bench 60.7 at high reasoning. Note: Q8_0 is only 11.27 GB (model is natively MXFP4), so Q8 also fits 16 GB cards. |

### Tier 3: Enthusiast (24 to 32 GB)

| Model | Best for | Recommended quant | File size | License | Context | Notes |
|-------|----------|-------------------|-----------|---------|---------|-------|
| [Mistral Small 3.2 24B](https://huggingface.co/mistralai/Mistral-Small-3.2-24B-Instruct-2506) | Vision + tools + chat, function calling, instruction following | Q4_K_M | 14.33 GB | Apache 2.0 | 128K | HumanEval+ 92.9, MBPP+ 78.3. Vision via mmproj. |
| [Devstral Small 2 24B](https://huggingface.co/mistralai/Devstral-Small-2-24B-Instruct-2512) | Software-engineering agents, multi-file edits, code exploration | Q4_K_M | 14.33 GB | Apache 2.0 | 256K | SWE-Bench Verified 68.0. Designed for Cline / agent IDEs. |
| [Qwen3.5 27B](https://huggingface.co/Qwen/Qwen3.5-27B) | Dense multimodal model, vision + reasoning | Q4_K_M | 15.59 GB | Apache 2.0 | 256K | Vision-language; thinking mode; 201 languages. |
| [Qwen3 Coder 30B A3B](https://huggingface.co/Qwen/Qwen3-Coder-30B-A3B-Instruct) | Agentic coding, repo-scale understanding, browser-use | Q4_K_M | 17.29 GB | Apache 2.0 | 256K native, 1M with YaRN | MoE: 30.5B / 3.3B active. Non-thinking, function-call native. Compatible with Qwen Code, CLINE. |
| [DeepSeek R1 Distill Qwen 32B](https://huggingface.co/deepseek-ai/DeepSeek-R1-Distill-Qwen-32B) | Heavy reasoning, math, long chain-of-thought | Q4_K_M | 18.48 GB | MIT | 128K | Visible reasoning traces. Q5_K_M (21.65 GB) fits 32 GB cards. |
| [Qwen3.6 35B A3B](https://huggingface.co/Qwen/Qwen3.6-35B-A3B) | Best balance of speed + quality + vision in this tier | UD-Q4_K_XL | 20.83 GB | Apache 2.0 | 256K | MoE: 35B / 3B active. SWE-bench Verified 73.4, AIME 2026 92.7, MMLU-Pro 85.2. Strong vision. |
| [Llama 3.3 70B Instruct](https://huggingface.co/meta-llama/Llama-3.3-70B-Instruct) | Production chat / multilingual at 70B quality | UD-IQ2_M | 22.62 GB | Llama 3.3 | 128K | Aggressive 2-bit dynamic quant. Quality dips vs Q4 but lands on a 24/32 GB device. Prefer Tier 4 if you have it. |

### Tier 4: Pro single-node (48 to 64 GB)

| Model | Best for | Recommended quant | File size | License | Context | Notes |
|-------|----------|-------------------|-----------|---------|---------|-------|
| [Llama 3.3 70B Instruct](https://huggingface.co/meta-llama/Llama-3.3-70B-Instruct) | Frontier-class general chat, multilingual, tool use | Q4_K_M | 39.61 GB | Llama 3.3 | 128K | The 70B everyone benchmarks against. Shards 2x 24 GB GPUs. Q5_K_M (46.53 GB) on 48 GB cards / 64 GB Macs. |
| [DeepSeek R1 Distill Llama 70B](https://huggingface.co/deepseek-ai/DeepSeek-R1-Distill-Llama-70B) | Heavy reasoning at 70B scale | Q4_K_M | ~42 GB | Llama 3.3 | 128K | Same shape as Llama 3.3 70B with R1-style thinking traces. |
| [Qwen3.6 35B A3B](https://huggingface.co/Qwen/Qwen3.6-35B-A3B) | High-quality multimodal MoE with headroom for context | Q6_K | 27.30 GB | Apache 2.0 | 256K | Comfortable here; can run 128K+ context without IQ tricks. |
| [Mistral Large 2411 (123B)](https://huggingface.co/mistralai/Mistral-Large-Instruct-2411) | Multilingual frontier-class on Apple Ultra or multi-GPU | UD-IQ2_M | ~40 GB | Mistral Research / Commercial | 128K | Tight at 48 GB; 64 GB Mac is the natural home with extreme quants. Note this license is **not** Apache 2.0. |

### Tier 5: Workstation (96 to 128 GB)

| Model | Best for | Recommended quant | File size | License | Context | Notes |
|-------|----------|-------------------|-----------|---------|---------|-------|
| [gpt-oss-120b](https://huggingface.co/openai/gpt-oss-120b) | Frontier-class reasoning, agentic, 80GB+ single-host | MXFP4_MOE | 59.02 GB (sharded 2 files) | Apache 2.0 | 128K | MoE: 117B / 5.1B active. Built to run on a single 80 GB H100. Fits one 96 GB Mac or sharded across 2x 48 GB GPUs. SWE-bench 62.4, GPQA Diamond 80.9 at high reasoning. |
| [Llama 3.3 70B Instruct](https://huggingface.co/meta-llama/Llama-3.3-70B-Instruct) | Highest-quality 70B you can run locally | Q6_K | ~58 GB (community) | Llama 3.3 | 128K | Q8_0 (~70 GB) comfortably fits with KV cache headroom. |
| [Devstral 2 123B](https://huggingface.co/mistralai/Devstral-Small-2-24B-Instruct-2512) (larger sibling) | Multi-file agentic SWE at scale | UD-IQ4_XS | ~68 GB | Apache 2.0 | 256K | For serious code agents. Verify the specific sibling repo for your machine. |

### Tier 6: Multi-GPU / Ultra (192 GB and up)

| Model | Best for | Recommended quant | File size | License | Context | Notes |
|-------|----------|-------------------|-----------|---------|---------|-------|
| [gpt-oss-120b](https://huggingface.co/openai/gpt-oss-120b) | Max-quality reasoning with long context | Q8_0 | 60.79 GB (sharded) | Apache 2.0 | 128K | Quality-near-fp16 in this tier; leaves room for 128K KV cache. |
| DeepSeek V4 Flash (very large MoE) | Frontier coding / reasoning, long context | community IQ2/IQ4 | 200 GB+ | MIT | 128K+ | Designed for Apple Silicon Metal with extreme quants per community quanters. Verify the specific quant against your machine. |
| Multiple Llama 3.3 70B replicas | High-throughput serving | Q4_K_M | 39.61 GB each | Llama 3.3 | 128K | Use LLMKube `replicas: N` to scale horizontally across GPUs / nodes. |

## The matrix, by task

Same models, sorted by what you want to do. Numbers cited come from the upstream model cards.

### General assistant / chat / RAG

| Tier | First pick | Backup |
|------|-----------|--------|
| 1 | Llama 3.2 3B (1.88 GB) | Gemma 3 4B (2.32 GB) |
| 2 | Llama 3.1 8B (4.58 GB) | Qwen3.5 9B (5.29 GB) |
| 3 | Mistral Small 3.2 24B (14.33 GB) | Qwen3.5 27B (15.59 GB) |
| 4 | Llama 3.3 70B Q4_K_M (39.61 GB) | Qwen3.6 35B A3B Q6_K (27.30 GB) |
| 5+ | gpt-oss-120b MXFP4 (59 GB) | Llama 3.3 70B Q8_0 |

### Coding / agentic SWE

| Tier | First pick | Notes |
|------|-----------|-------|
| 1 | Qwen3.5 4B (2.74 GB) | Smallest model with usable code skills. |
| 2 | Granite 4.1 8B Q4_K_M (4.98 GB) | HumanEval 85.4, MBPP 87.3, native FIM. |
| 2 | gpt-oss-20b Q4_K_M (10.83 GB) | SWE-bench 60.7 at high reasoning. |
| 3 | Qwen3 Coder 30B A3B (17.29 GB) | Best open agent-IDE model in this size class; 256K context. |
| 3 | Devstral Small 2 24B (14.33 GB) | SWE-Bench Verified 68.0; built for multi-file edits. |
| 4 | Qwen3 Coder 30B A3B Q6_K (~23 GB) | Same model, more quality headroom. |
| 5+ | gpt-oss-120b | SWE-bench 62.4, broader knowledge. |

### Reasoning / math / STEM

| Tier | First pick | Notes |
|------|-----------|-------|
| 1 | Phi-4-mini reasoning (~2.5 GB) | Tiny MIT-licensed reasoner. |
| 2 | Phi-4 14B Q4_K_M (8.43 GB) | MATH 80.4, GPQA 56.1, MIT. |
| 2 | DeepSeek R1 Distill Qwen 14B (8.99 GB) | Visible chain-of-thought. |
| 3 | DeepSeek R1 Distill Qwen 32B (18.48 GB) | Deeper reasoning, longer traces. |
| 3 | Qwen3.6 35B A3B (20.83 GB) | AIME 2026 92.7, MMLU-Pro 85.2 with thinking on. |
| 5+ | gpt-oss-120b (high reasoning) | GPQA Diamond up to 80.9. |

### Vision / multimodal

llama.cpp loads the vision projector from a separate `mmproj-*.gguf` file alongside the language GGUF. Account for both.

| Tier | First pick | mmproj size |
|------|-----------|-------------|
| 1 | Gemma 3 4B (2.32 GB) | ~0.85 GB |
| 1 | Qwen3.5 4B (2.74 GB) | ~0.67 GB |
| 2 | Gemma 3 12B (7.30 GB) | ~0.85 GB |
| 2 | Qwen3.5 9B (5.29 GB) | ~0.85 GB |
| 3 | Mistral Small 3.2 24B (14.33 GB) | ~0.88 GB |
| 3 | Qwen3.5 27B (15.59 GB) | ~0.86 GB |
| 3 | Qwen3.6 35B A3B (20.83 GB) | ~0.84 GB |

### Function calling / tool use / agents

| Tier | First pick | Notes |
|------|-----------|-------|
| 1 | Phi-4-mini Instruct | Strong tool use at edge sizes. |
| 2 | Granite 4.1 8B | BFCL v3 of 68.27, dedicated tool-call template. |
| 2 | gpt-oss-20b | Native function calling, web/python tools. |
| 3 | Qwen3 Coder 30B A3B | Native function-call format, designed for agent loops. |
| 3 | Mistral Small 3.2 24B | Mistral function-call template, vLLM tool parser. |
| 5+ | gpt-oss-120b | Production-grade tool use at frontier quality. |

### Long context (100K+ tokens)

| Tier | First pick | Native context |
|------|-----------|----------------|
| 1 | Qwen3.5 4B | 256K (1M with YaRN) |
| 2 | Qwen3.5 9B | 256K (1M with YaRN) |
| 2 | Granite 4.1 8B | 128K, cheap KV cache (Mamba hybrid) |
| 3 | Qwen3 Coder 30B A3B | 256K native, 1M with YaRN |
| 3 | Qwen3.6 35B A3B | 256K native, 1M with YaRN |
| 4+ | Devstral Small 2 24B | 256K |

Phi-4 14B is **only 16K context**; do not pick it for long-document workloads.

## What this doc deliberately does not include

- **Uncensored / abliterated / "heretic" variants.** They show up high in HuggingFace trending but I want this list to be reproducible.
- **Hobbyist frankenmerges** (anything labeled "X-Distilled-GGUF" from non-vendor authors). The base models above are the inputs to most of those; pick the base unless you have a specific reason.
- **Pure base models** (no `-Instruct` / `-it` suffix). LLMKube users almost always want instruct variants.
- **Embedding / reranker GGUFs.** Those belong in a separate matrix for RAG infrastructure.
- **GGUFs marked `imatrix`-only** (calibration files, not runnable models).
- **CPU-only inference benchmarks.** Possible but typically 1 to 5 tok/s for a 7B; this doc assumes GPU or Apple Silicon.

## Methodology and sources

Every file size in this doc was fetched from the HuggingFace tree API on 2026-05-11. Capability claims (context length, license, benchmarks) come from the upstream `meta-llama/`, `Qwen/`, `mistralai/`, `google/`, `microsoft/`, `openai/`, `deepseek-ai/`, and `ibm-granite/` model cards, not from third-party quanters. The Unsloth and Bartowski GGUF repos are referenced because they are the most-downloaded community quants with reproducible naming, but the underlying weights and licenses follow the upstream publisher.

When in doubt, prefer Q4_K_M as the starting quant, measure quality on your own task, and step up to Q5_K_M or Q6_K only if you see issues. Going below Q4 (IQ3, Q2, IQ2, IQ1) trades real quality for memory; only do it if the model otherwise will not fit.

### A reminder that this will go stale

The local LLM ecosystem is one of the fastest-moving spaces in software. Between when this was written and when you are reading it, expect that:

- New model generations (Llama 4.x, Qwen3.7, Gemma 5, etc.) have probably shifted the recommendations.
- Quanters have re-uploaded files with better imatrix calibration; sizes shift by a few percent.
- Tools like llama.cpp and llmkube have added new quant types, faster KV cache modes, or runtime backends.
- License terms occasionally change (especially Llama and Gemma).

Use this as a structured starting point. **Always click through to the upstream HuggingFace model card before downloading a large file.** If you find this doc is meaningfully wrong, a PR with the corrected row is appreciated, but please do not assume any single number here is current the day you read it.

## Runtime backends

### sglang

GPU-only. Choose when:

- Your workload re-sends a large shared prefix every turn (foreman tool loops,
  multi-turn chat with a stable system prompt + tool definitions). SGLang's
  RadixAttention (`--enable-prefix-caching`) serves the shared prefix from a
  tree-structured cache rather than reprocessing it.
- You want speculative decoding (EAGLE / EAGLE3) or tool-call / reasoning
  parsers that vLLM doesn't ship with first-class CRD fields.

Unlike llama.cpp, there's no meaningful CPU path — even small embedding models
require a GPU. AMD is supported via the ROCm image (`lmsysorg/sglang:...-rocm630`),
picked automatically by the controller when `model.spec.hardware.gpu.vendor`
is `amd`.

Defaults to the OpenAI-compatible `/v1` surface, so downstream tooling
(litellm, the foreman agents) needs no change.
