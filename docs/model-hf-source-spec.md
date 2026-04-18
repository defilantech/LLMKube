# Spec: Model Controller Support for HuggingFace Repo IDs

Issue: #292

## Context

The Model CRD's `source` field regex accepts bare HuggingFace repo IDs like `TinyLlama/TinyLlama-1.1B-Chat-v1.0` (via the `^[a-zA-Z0-9][\w\-\.\/]+$` branch of the pattern). However the Model controller's reconcile flow only knows three source types:

- PVC sources (`pvc://...`) → special PVC reconcile path
- Local sources (`file://...` or `/absolute/path`) → copy via `io.Copy`
- Everything else → HTTP download via `http.Get`

When a HuggingFace repo ID falls into "everything else," `http.Get("TinyLlama/TinyLlama-1.1B-Chat-v1.0")` fails with `unsupported protocol scheme ""`. The model phase goes to `Failed` and any referencing InferenceService stays `Pending` forever.

This blocks the intended vLLM workflow: user creates a Model with a bare repo ID, sets `skipModelInit: true` on the InferenceService, vLLM receives the repo ID as `--model` and downloads the weights itself using `HF_TOKEN` at runtime.

The runtime side already supports this (`runtime_vllm.go:21-24` falls back to `model.Spec.Source` when `modelPath` is empty). Only the Model controller needs to learn that some sources are runtime-resolved.

## What to implement

Add a fourth source type to the Model controller: **runtime-resolved sources** (sources the runtime container will fetch itself). For these, the controller should skip download and mark the Model `Ready` immediately so referencing InferenceServices can proceed.

### 1. New helper in `internal/controller/source.go`

Add a helper function that detects a bare HuggingFace repo ID format:

```go
// isHFRepoSource reports whether source looks like a HuggingFace repo ID
// (e.g., "TinyLlama/TinyLlama-1.1B-Chat-v1.0", "Qwen/Qwen3.6-35B-A3B").
// These sources are downloaded by the runtime (vLLM) at startup, not by
// the Model controller.
//
// Criteria:
//   - Not a URL (no "://" scheme)
//   - Not an absolute path (doesn't start with "/")
//   - Not a PVC source (handled separately)
//   - Contains at least one "/" separator (HF convention: owner/repo)
//   - Matches Hugging Face's permitted character set
func isHFRepoSource(source string) bool
```

**Detection order** must respect existing dispatch:
1. `isPVCSource(source)` → PVC path (existing)
2. `isLocalSource(source)` → local copy (existing)
3. Strings matching `^https?://` → HTTP download (existing)
4. **new** `isHFRepoSource(source)` → runtime-resolved
5. Otherwise → fall back to HTTP download (the current behavior)

Put this helper in `source.go` near `isLocalSource`.

### 2. Reconcile path in `internal/controller/model_controller.go`

In the `Reconcile` function (around lines 91-95), after the PVC dispatch but before the general download flow, add a branch that detects runtime-resolved sources and calls a new `reconcileRuntimeResolvedSource` method.

The new method should:

- Log that the source is runtime-resolved and download is being skipped
- Set `model.Status.Phase = PhaseReady`
- Set `model.Status.Path = ""` (intentionally empty — runtime uses `Spec.Source` directly)
- Set `model.Status.CacheKey = ""` (not cached by the operator)
- Set `model.Status.Size = "0"` (field is a string)
- Set a `Ready` condition with reason like `RuntimeResolved` and a message like `"Source is runtime-resolved (e.g., HuggingFace repo ID); runtime will fetch at startup"`
- Emit a `ModelStatus` metric with a new status value (`runtime-resolved`) or reuse `ready`
- Return `ctrl.Result{}` (no requeue)

Mirror the status update pattern used by `reconcilePVCSource` (around lines 225-290) which also sets a virtual path and skips download.

### 3. InferenceService validation (optional but recommended)

In `inferenceservice_controller.go`, when a referenced Model has `Status.Phase = PhaseReady` but `Status.Path = ""`, the InferenceService must have `skipModelInit: true` — otherwise the init container will try to mount a nonexistent cached file.

Emit a `Warning` event if this condition is missed: reason `MissingSkipModelInit`, message like `"Model source is runtime-resolved but spec.skipModelInit is not set; init container will fail"`. Use the existing `Recorder` that was added for the hybrid offload feature (already on `InferenceServiceReconciler`).

This is optional but saves users from a confusing failure mode. Put the check in `Reconcile` right after the Model is fetched and verified Ready, before `reconcileDeployment`.

### 4. Tests

Follow the established patterns from the Phase 1 / Phase 2 work.

**`internal/controller/source_test.go`** — add tests for `isHFRepoSource`:
- `"TinyLlama/TinyLlama-1.1B-Chat-v1.0"` → `true`
- `"Qwen/Qwen3.6-35B-A3B"` → `true`
- `"bartowski/Qwen_Qwen3.6-35B-A3B-GGUF"` → `true`
- `"https://example.com/model.gguf"` → `false` (URL)
- `"/models/local.gguf"` → `false` (absolute path)
- `"file:///models/local.gguf"` → `false` (file:// URL)
- `"pvc://my-claim/model.gguf"` → `false` (PVC)
- `"just-a-filename"` → `false` (no slash, not a repo ID)
- `""` → `false` (empty)
- `"multi/part/path/thing"` → `true` (HF supports nested paths in source field)

**`internal/controller/model_controller_test.go`** — add a Context "when source is a HuggingFace repo ID":
- Create a Model with `source: "TinyLlama/TinyLlama-1.1B-Chat-v1.0"`
- Trigger reconcile
- Assert `Status.Phase == PhaseReady`
- Assert `Status.Path == ""`
- Assert `Status.CacheKey == ""`
- Assert no download attempt was made (no file in cache dir, no HTTP request)
- Assert a `Ready` condition with reason `RuntimeResolved`

Keep the existing HTTP download tests passing unchanged.

### 5. Sample update

Update `config/samples/vllm-tinyllama.yaml` header comment (if it exists; create a header if not) to explain this flow:

```yaml
# This sample demonstrates running vLLM with a HuggingFace repo ID source.
# The Model controller recognizes runtime-resolved sources and skips download;
# vLLM fetches weights at pod startup using HF_TOKEN.
#
# Requires:
#   - A Secret named "hf-token" with key "HF_TOKEN" in the same namespace
#   - skipModelInit: true on the InferenceService spec
```

### 6. Generation and Helm sync

After code changes:
- Run `make generate` to update `zz_generated.deepcopy.go` (likely no-op for this feature)
- Run `make manifests` to regenerate CRD YAML (should also be no-op since we're not changing types)
- Confirm `make test` passes
- No Helm chart CRD sync needed since no CRD schema changes

## Acceptance criteria

1. `kubectl apply` a Model with `source: "TinyLlama/TinyLlama-1.1B-Chat-v1.0"` → `kubectl get model` shows Phase `Ready` within one reconcile cycle
2. Unit tests pass: `make test` clean, controller coverage holds or improves
3. `make fmt`, `make vet` clean
4. The four source types (PVC, local, HTTP, runtime-resolved) are all handled correctly; existing HTTP and local flows are unchanged
5. InferenceService referencing a runtime-resolved Model with `skipModelInit: true` produces a pod where vLLM's `--model` arg receives the raw source string

## Files to modify

- `internal/controller/source.go` — add `isHFRepoSource`
- `internal/controller/source_test.go` — add detection tests
- `internal/controller/model_controller.go` — add `reconcileRuntimeResolvedSource`, route in `Reconcile`
- `internal/controller/model_controller_test.go` — add runtime-resolved reconcile tests
- `internal/controller/inferenceservice_controller.go` — add validation warning (optional)
- `config/samples/vllm-tinyllama.yaml` — update header comment

## Out of scope

- Downloading HuggingFace weights into the cache PVC (that's option 2 from the issue; not needed for the vLLM workflow and adds significant complexity)
- Auth for HF_TOKEN in the Model controller (runtime handles this)
- Format validation against HF model type (leave to the runtime)
- Supporting HF revisions/branches (user can put `owner/repo@revision` syntax in source if HF supports it via vLLM; don't special-case here)

## Reference: existing code patterns to follow

- **PVC special-casing**: `reconcilePVCSource` in `model_controller.go:225-290` is the model for a non-download reconcile path that still sets Ready.
- **Helper function style**: `isPVCSource`, `isLocalSource`, `getLocalPath` in `source.go` — small, pure functions with focused tests.
- **Test fixture pattern**: `model_controller_test.go` uses `BeforeEach` for Model creation, `httptest.NewServer` for HTTP mocking. Runtime-resolved tests need no HTTP mock.
- **Phase 1 warning pattern**: `needsOffloadMemoryWarning` in `inferenceservice_controller.go` demonstrates the Event-emission pattern for advisory warnings that don't block reconciliation.
