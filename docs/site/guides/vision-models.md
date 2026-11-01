---
title: Serving vision models
description: Serve a multimodal (vision) GGUF model with llama.cpp by declaring the projector file with Model spec.mmproj. Covers how spec.source, spec.files and spec.mmproj fit together, the sources that support it, how to confirm the projector loaded, and an image request through the OpenAI-compatible API.
---

# Serving vision models

A vision GGUF model for llama.cpp ships as two files: the language model and a
multimodal projector, usually named `mmproj-*.gguf`, that turns an image into
input the language model can read. llama-server loads the first with
`--model` and the second with `--mmproj`. Without the projector the model
still serves text, and rejects images.

LLMKube stages both files for you. Name the projector in the Model's
`spec.mmproj` and the operator stages it with the language model and adds
`--mmproj` to the llama-server arguments. You do not need a helper pod,
an extra volume, or `LLAMA_ARG_MMPROJ` in `spec.env`.

## How the fields fit together

- `spec.source` names a repository or bucket prefix, not a single file.
- `spec.files` lists the files to stage from that source, as paths relative
  to it. `files[0]` is the primary model file, the one llama-server gets as
  `--model`.
- `spec.mmproj` names the projector, also relative to the source. It is
  staged into the same cache directory as the `files` entries, at that
  relative path.
- With the `llamacpp` runtime, the operator appends
  `--mmproj <path of the staged projector>` to the container arguments. If
  the InferenceService's `spec.extraArgs` already contains `--mmproj`, the
  operator does not add its own and yours is used.

One rule catches people: **`spec.mmproj` requires `spec.files`**. A Model
that sets `mmproj` without listing the primary model file in `files` goes to
`Failed` with reason `InvalidFileSet` and the message
`spec.files must include a primary model file when spec.mmproj is set`.

`spec.mmproj` must be a concrete path: no glob characters (`*`, `?`, `[`),
no leading `/`, and no `..`.

## Example: Gemma 3 4B with vision

This uses the public, ungated
[`ggml-org/gemma-3-4b-it-GGUF`](https://huggingface.co/ggml-org/gemma-3-4b-it-GGUF)
repository: the Q4_K_M language model (2.49 GB) and its F16 projector
(0.85 GB).

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata:
  name: gemma-3-4b-vision
spec:
  source: hf://ggml-org/gemma-3-4b-it-GGUF
  format: gguf
  quantization: Q4_K_M
  files:
    - gemma-3-4b-it-Q4_K_M.gguf
  mmproj: mmproj-model-f16.gguf
---
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: gemma-3-4b-vision
spec:
  modelRef: gemma-3-4b-vision
  runtime: llamacpp
```

The same manifests are in
[`config/samples/model_vision_mmproj.yaml`](https://github.com/defilantech/LLMKube/blob/main/config/samples/model_vision_mmproj.yaml).

As written this runs on CPU. GPU settings (`spec.hardware` on the Model,
`spec.resources` on the InferenceService) work the same way as for a
text-only model; nothing about them changes for vision.

The operator does not look up plain file names in the repository; it lists
the repository only to expand a glob in `files`. A typo shows up as a failed
download in the inference pod's `model-downloader` init container (or in the
prefetch Job, with `spec.prefetch`), so copy the names from the repository's
file list.

## Supported sources

`spec.mmproj` uses the same multi-file staging as `spec.files`, so it works
with the sources that support it:

| Source | Form | Notes |
| --- | --- | --- |
| Hugging Face | `hf://org/repo`, `hf://org/repo@revision`, bare `org/repo`, or a `https://huggingface.co/org/repo` URL | Gated or private repositories need an `HF_TOKEN` in `spec.sourceSecretRef`; see [Gated and private Hugging Face repositories](/docs/guides/model-cache#gated-and-private-hugging-face-repositories). A `sourceSecretRef` that names an `HF_ENDPOINT` stages from that mirror instead. |
| S3-compatible object store | `s3://bucket/prefix` | Credentials and endpoint come from `spec.sourceSecretRef`. See Option D in [Air-gapped install](/docs/guides/air-gapped). |
| OCI artifact | `oci://registry/repo@sha256:...` | The artifact already carries every file, so nothing is downloaded. Use explicit paths; globs cannot be expanded against an artifact. |

The repo base on the `HF_ENDPOINT` mirror itself
(`<HF_ENDPOINT>/<org>/<repo>/resolve/<revision>/`) is accepted as the source
too. Any other source with `spec.mmproj` set is rejected and the Model goes
to `Failed`. That includes any other plain `https://` URL, `pvc://`, and
local paths. For a projector that already sits on a PVC next to
the model, point `spec.source` at the model file and pass the projector
yourself. A `pvc://<claim>/<path>` source mounts the whole claim at
`/model-source`, so:

```yaml
# Model
spec:
  source: pvc://model-store/gemma-3-4b/gemma-3-4b-it-Q4_K_M.gguf
---
# InferenceService
spec:
  modelRef: gemma-3-4b-vision
  extraArgs:
    - --mmproj
    - /model-source/gemma-3-4b/mmproj-model-f16.gguf
```

## Confirm the projector is wired

The Model lists both files in `status.stagedFiles`:

```bash
kubectl get model gemma-3-4b-vision -o jsonpath='{.status.stagedFiles}'
# ["gemma-3-4b-it-Q4_K_M.gguf","mmproj-model-f16.gguf"]
```

The llama-server container arguments carry `--mmproj`, pointing into the same
cache directory as `--model`. With the default persistent model cache, that
directory is `/models/<cache-key>`, where the key is derived from
`spec.source`:

```bash
kubectl get deployment gemma-3-4b-vision \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="llama-server")].args}'
# ["--model","/models/<cache-key>/gemma-3-4b-it-Q4_K_M.gguf",...,"--mmproj","/models/<cache-key>/mmproj-model-f16.gguf",...]
```

Without `spec.prefetch`, the Model reports `Ready` once the file set is
valid. The bytes are downloaded by the inference pod's `model-downloader`
init container, so watch that container on the first start:

```bash
kubectl logs deployment/gemma-3-4b-vision -c model-downloader
```

## Send an image

llama-server accepts images in the OpenAI chat format, as an `image_url`
content part. A base64 `data:` URL is the reliable form: llama-server also
accepts an `http(s)://` URL, but then the inference pod fetches the image
itself, which needs egress from the pod and is capped at 10 MB.

```bash
kubectl port-forward svc/gemma-3-4b-vision 8080:8080 &

IMG=$(base64 < photo.jpg | tr -d '\n')
curl -sS http://localhost:8080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d @- <<EOF
{
  "model": "gemma-3-4b-vision",
  "messages": [{
    "role": "user",
    "content": [
      {"type": "text", "text": "Describe this image in one sentence."},
      {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,${IMG}"}}
    ]
  }]
}
EOF
```

Use `data:image/png;base64,` for a PNG. A description of the image in the
response means the projector loaded. If the projector is missing, llama-server
returns an error whose message is
`image input is not supported - hint: if this is unexpected, you may need to provide the mmproj`;
check `status.stagedFiles` and the container arguments above.

## Memory

The projector is loaded alongside the language model, so budget for both. The
[model matrix](/docs/guides/model-matrix) lists projector sizes for its vision
picks in the "Vision / multimodal" table.

## Scope

- `spec.mmproj` is used by the in-cluster `llamacpp` runtime only. Other
  runtimes, including vLLM and SGLang, ignore it; they load a multimodal
  checkpoint's vision weights with the rest of the model.
- The macOS Metal agent does not read `spec.files` or `spec.mmproj` today.
  On Metal, pass the projector in `spec.extraArgs` (`--mmproj` and its path),
  as in the `pvc://` example above. By default the agent only accepts a path
  that resolves into its model store (`--model-store`); see
  [`--allowed-model-roots`](https://github.com/defilantech/LLMKube/blob/main/deployment/macos/README.md#--allowed-model-roots-flag-local-model-paths).
- `spec.mmproj` and `files` staging from Hugging Face are available since
  LLMKube 0.8.18; `s3://` sources since 0.9.16.
