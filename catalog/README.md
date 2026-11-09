# Model catalog

`catalog.yaml` is embedded in the CLI. Entries without `runtime` keep the
`llamacpp` default and GGUF format. `catalog list` and `catalog info` show the
runtime. Rebuild the CLI after editing the catalog.

## Context and memory estimates

`context_size` is the deployment default, not necessarily the model's maximum
supported context. `llmkube deploy <model> --context <tokens>` overrides it;
larger contexts need more KV-cache memory.

For `qwen-2.5-14b`, the default is 16,384 tokens. The model is trained for
32,768 (`max_position_embeddings`, no `rope_scaling`); Qwen documents 128K only
with YaRN, which a catalog entry cannot set. Its
[Q5_K_M weights](https://huggingface.co/bartowski/Qwen2.5-14B-Instruct-GGUF/blob/main/Qwen2.5-14B-Instruct-Q5_K_M.gguf)
are about 10.51 GB. With 48 layers, 8 KV heads and a head dimension of 128,
the f16 KV cache uses `2 (K and V) × 48 × 8 × 128 × 2 bytes × context tokens`:
1.5 GiB at 8K, 3 GiB at 16K, or 6 GiB at the trained 32,768 tokens.
Weights plus the default cache total about 13.7 GB; the catalog estimates
14–16 GB including runtime overhead for full GPU offload. `--context 32768`,
the trained maximum, needs about 16.5 GB and leaves no headroom on a 16 GB card.
A context above 32,768 does not add usable window: llama.cpp allocates the
larger cache but caps each slot at the trained context.

For `llama-3.3-70b`, the default is 8,192 tokens even though the model is
trained for 131,072 (the GGUF reports `context_length: 131072` with llama3
RoPE scaling). Its
[Q4_K_M weights](https://huggingface.co/bartowski/Llama-3.3-70B-Instruct-GGUF/blob/main/Llama-3.3-70B-Instruct-Q4_K_M.gguf)
are about 42.52 GB. With 80 layers, 8 KV heads and a head dimension of 128,
the f16 KV cache uses `2 (K and V) × 80 × 8 × 128 × 2 bytes × context tokens`
= 327,680 bytes per token: 2.5 GiB at 8K, 10 GiB at 32K, or 40 GiB at
131,072 tokens. Weights plus the default cache total about 45 GB; the catalog
estimates 45–55 GB including runtime overhead for the multi-GPU setups the
entry names (2× A100 40GB or 1× A100 80GB). The full 131,072-token window
needs about 85 GB before runtime buffers. `--context 32768` costs about
53 GB and fits either setup with headroom.

For a vLLM or SGLang entry, use this schema (illustrative only, not a validated
model recommendation):

```yaml
example:
  name: Example checkpoint
  runtime: vllm # or sglang
  source: org/checkpoint
  files: ["*.safetensors", "*.json", "*.jinja"]
  quantization: NVFP4
  context_size: 8192
  gpu:
    count: 2
    tensor_parallel_size: 2
  served_model_name: chat
  parsers:
    tool_call: hermes
    reasoning: deepseek-r1
  extra_args: []
  resources:
    cpu: "4"
    memory: 16Gi
```

`deploy` creates a safetensors Model with the source and file set, and an
InferenceService with the selected runtime, GPU count, typed parser and tensor
parallel settings, and runtime-specific context limit. The controller selects
the default runtime image; `--image` overrides it. The served name uses
`--served-model-name` in `extraArgs`.

Runtime image selection also applies to non-catalog deployments using `--source`
with `--runtime vllm` or `--runtime sglang`: the CLI leaves `spec.image` unset
unless `--image` is supplied, so the controller uses the backend's default image.

vLLM/SGLang entries require a `gpu` block with `count >= 1`. These catalog
deployments reject `--gpu=false` and `--gpu-count=0` because their default images
require CUDA. Explicit `--gpu-count` and `--format` values override catalog
defaults; GPU counts below the tensor parallel size are rejected. `--extra-args` replaces
the catalog argument list, and an explicit served-name argument wins. A vLLM or
SGLang entry cannot be switched to another runtime with `--runtime`: its parser
names and arguments belong to its declared runtime. Use an explicit `--source`
for a separately configured deployment. Existing llama.cpp runtime overrides
remain supported.

`gpu_layers` belongs only to llama.cpp; parser, served-name, and tensor parallel
settings belong to vLLM/SGLang. File patterns follow `Model.spec.files` semantics.
`cache preload` preserves the format and file set. Catalog benchmark deployment
and context sweeps currently support only llama.cpp; deploy other runtimes first
and benchmark their endpoints.

Only add production entries after an end-to-end run on the intended hardware.
Include `validated: {hardware: "actual hardware", llmkube: "actual version"}` and
the existing descriptive fields (size, tags, use cases, VRAM estimate, homepage).
The current catalog retains its 17 GGUF entries; vLLM/SGLang seed entries still
need hardware validation. Unit and API-server tests do not establish that a
checkpoint loads or its parsers work on real requests.
