# vLLM default image

LLMKube pins the default vLLM container image to a specific version rather than `:latest`. The current default is **`vllm/vllm-openai:v0.20.0`**.

## Why pin

`:latest` is reproducibility-hostile. A user who applies the same `InferenceService` manifest on two different days can land on two different vLLM versions with different defaults, different supported flags, and different model compatibility. Pinning gives the operator a deterministic baseline that we test against, document, and ship release notes for.

## Override

The default only applies when an `InferenceService` does not set `spec.image`. To run a different vLLM version, set the field explicitly:

```yaml
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata:
  name: my-vllm
spec:
  runtime: vllm
  image: vllm/vllm-openai:v0.21.0   # any tag the runtime image registry serves
```

User-set images bypass the default entirely. There is no version compatibility check at the operator level — if a tag exists in the registry, LLMKube will pull it. Whether the resulting pod boots is between you and vLLM.

## Bump cadence

We bump `DefaultImage()` per vLLM minor release (e.g., v0.20 → v0.21), once a smoke test on representative hardware confirms the new image deploys cleanly via the operator. The bump lands as a PR titled `chore(controller): pin vLLM default image to vX.Y.0` and goes into the next LLMKube minor or patch release.

The pin lives at [`internal/controller/runtime_vllm.go`](../internal/controller/runtime_vllm.go) on the `DefaultImage()` method. A change here is one line plus a test fixture update at [`internal/controller/inferenceservice_controller_test.go`](../internal/controller/inferenceservice_controller_test.go) that asserts the literal string.

## What changes between vLLM versions

vLLM minor releases routinely change CLI flag defaults, deprecate flags, add new model-architecture support, and shift the default CUDA / PyTorch / transformers versions. The LLMKube operator absorbs these where they intersect with the structured spec (e.g., `vllmConfig.kvCacheDtype`, `vllmConfig.attentionBackend`); user-provided `extraArgs` are passed through untouched.

When a vLLM bump introduces a deprecation that affects `BuildArgs` (see #357 for the v0.20 `--model` example), it lands as a separate PR ahead of the default-image bump so users on the old image are not forced to upgrade simultaneously.

## References

- [vLLM v0.20.0 release notes](https://github.com/vllm-project/vllm/releases/tag/v0.20.0)
- [LLMKube issue #354](https://github.com/defilantech/LLMKube/issues/354) — the v0.20.0 integration thread
