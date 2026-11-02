# Model catalog

`catalog.yaml` is embedded in the CLI. Entries without `runtime` keep the
`llamacpp` default and GGUF format. `catalog list` and `catalog info` show the
runtime. Rebuild the CLI after editing the catalog.

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

Explicit `--gpu`, `--gpu-count`, and `--format` values override catalog defaults;
GPU counts below the tensor parallel size are rejected. `--extra-args` replaces
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
