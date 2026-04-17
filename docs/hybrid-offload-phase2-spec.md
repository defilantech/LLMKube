# Hybrid Offloading Phase 2 Implementation Spec

Issue: #280 (Phase 2 section)

## What to implement

Add three new fields to `InferenceServiceSpec` and wire them through to llama.cpp args, following the exact patterns established in Phase 1 (PR #281).

### New Fields

#### 1. `tensorOverrides` (`[]string`)

- **Purpose**: Fine-grained tensor placement overrides for power users
- **Maps to**: `--override-tensor` flag in llama.cpp (one flag per entry)
- **Example values**: `["exps=CPU", "blk\\.(0|1)\\.ffn_.*=CUDA0"]`
- **CRD location**: `InferenceServiceSpec` (top-level, after `noKvOffload`)
- **Type**: `[]string` (string slice, same pattern as `ExtraArgs`)
- **Marker**: `// +optional` only (no enum or min/max needed)

Each entry becomes a separate `--override-tensor` flag:
```
tensorOverrides: ["exps=CPU", "token_embd=CUDA0"]
```
becomes:
```
--override-tensor exps=CPU --override-tensor token_embd=CUDA0
```

#### 2. `batchSize` (`*int32`)

- **Purpose**: Token batch size for prompt processing
- **Maps to**: `--batch-size N` flag in llama.cpp
- **CRD location**: `InferenceServiceSpec` (top-level, after `tensorOverrides`)
- **Type**: `*int32` (pointer, same pattern as `ContextSize`)
- **Markers**: `// +kubebuilder:validation:Minimum=1`, `// +kubebuilder:validation:Maximum=16384`, `// +optional`
- **Guard in helper**: only append when `> 0`

#### 3. `uBatchSize` (`*int32`)

- **Purpose**: Micro-batch size for decoding
- **Maps to**: `--ubatch-size N` flag in llama.cpp
- **CRD location**: `InferenceServiceSpec` (top-level, after `batchSize`)
- **Type**: `*int32` (pointer, same pattern as `ContextSize`)
- **Markers**: `// +kubebuilder:validation:Minimum=1`, `// +optional`
- **Guard in helper**: only append when `> 0`

## Files to modify

### 1. `api/v1alpha1/inferenceservice_types.go`

Add three fields to `InferenceServiceSpec` after the `NoKvOffload` field (around line 140) and before `ExtraArgs`.

Follow the comment/marker style of the surrounding fields. Example for reference — this is how Phase 1 fields look in the file:

```go
// MoeCPUOffload offloads all MoE expert layers to CPU for reduced VRAM usage.
// Enables running large MoE models (e.g., Qwen3-30B, Mixtral) on VRAM-constrained
// hardware by keeping attention layers on GPU while expert weights use system RAM.
// Maps to llama.cpp --cpu-moe flag. Requires sufficient system RAM via resources.memory.
// +optional
MoeCPUOffload *bool `json:"moeCPUOffload,omitempty"`
```

### 2. `internal/controller/inferenceservice_controller.go`

Add three helper functions after `appendNoKvOffloadArgs` (around line 997):

Follow these existing patterns exactly:

```go
// Bool flag pattern (reference: appendMoeCPUOffloadArgs)
func appendMoeCPUOffloadArgs(args []string, moeCPUOffload *bool) []string {
	if moeCPUOffload != nil && *moeCPUOffload {
		return append(args, "--cpu-moe")
	}
	return args
}

// Int flag pattern (reference: appendContextSizeArgs)
func appendContextSizeArgs(args []string, contextSize *int32) []string {
	if contextSize != nil && *contextSize > 0 {
		return append(args, "--ctx-size", fmt.Sprintf("%d", *contextSize))
	}
	return args
}
```

For `tensorOverrides`, the helper should loop and add each entry as a separate `--override-tensor` flag:
```go
func appendTensorOverrideArgs(args []string, overrides []string) []string {
	for _, override := range overrides {
		args = append(args, "--override-tensor", override)
	}
	return args
}
```

### 3. `internal/controller/runtime_llamacpp.go`

In `BuildArgs()`, call the three new helpers. Insert after the existing `appendNoKvOffloadArgs` call and before the `ExtraArgs` block. The current ordering in the function is:

```go
args = appendCacheTypeArgs(args, isvc.Spec.CacheTypeK, isvc.Spec.CacheTypeV)
args = appendMoeCPUOffloadArgs(args, isvc.Spec.MoeCPUOffload)
args = appendMoeCPULayersArgs(args, isvc.Spec.MoeCPULayers)
args = appendNoKvOffloadArgs(args, isvc.Spec.NoKvOffload)
// <-- INSERT NEW CALLS HERE -->
if len(isvc.Spec.ExtraArgs) > 0 {
    args = append(args, isvc.Spec.ExtraArgs...)
}
```

### 4. `internal/controller/inferenceservice_controller_test.go`

Add test contexts following the existing Phase 1 test patterns. The Phase 1 tests are located after the "when cache type is configured" context block (around line 1335).

**For each field, add three tests:**

For `tensorOverrides`:
- "should include --override-tensor flags when tensorOverrides is set" (set `["exps=CPU", "token_embd=CUDA0"]`, assert both `--override-tensor` entries appear)
- "should NOT include --override-tensor when tensorOverrides is empty"
- "should NOT include --override-tensor when tensorOverrides is nil"

For `batchSize`:
- "should include --batch-size flag with correct value when batchSize is set" (e.g., 2048)
- "should NOT include --batch-size when batchSize is not specified"
- "should NOT include --batch-size when batchSize is zero"

For `uBatchSize`:
- "should include --ubatch-size flag with correct value when uBatchSize is set" (e.g., 256)
- "should NOT include --ubatch-size when uBatchSize is not specified"
- "should NOT include --ubatch-size when uBatchSize is zero"

Also update the "GPU model with all llama.cpp options" integration test context (around line 4250) to include the new fields and verify the flags appear.

Use the same test fixture pattern as the Phase 1 tests — create a reconciler, model, and isvc in BeforeEach, then call `reconciler.constructDeployment(isvc, model, 1)` and assert on `container.Args`.

### 5. `config/samples/inference_v1alpha1_inferenceservice.yaml`

Add commented examples after the existing hybrid offloading comments:

```yaml
  # Tensor placement overrides for fine-grained GPU/CPU control (optional)
  # tensorOverrides:
  #   - "exps=CPU"
  #   - "token_embd=CUDA0"

  # Batch size for prompt processing (optional, default: llama.cpp default)
  # batchSize: 2048

  # Micro-batch size for decoding (optional)
  # uBatchSize: 256
```

### 6. `charts/llmkube/templates/crds/inferenceservices.yaml`

After running `make manifests`, the generated CRD at `config/crd/bases/inference.llmkube.dev_inferenceservices.yaml` will have the new fields. Copy the new field definitions into the Helm chart CRD file in alphabetical order (matching where controller-gen places them in the generated CRD).

### 7. Code generation (run these commands after code changes)

```bash
make generate    # Regenerates zz_generated.deepcopy.go
make manifests   # Regenerates CRD YAML and RBAC
make test        # Runs all unit tests
```

## Verification

After all changes:
1. `make generate && make manifests` should complete without errors
2. `make fmt && make vet` should be clean
3. `make test` should pass with all new tests
4. The generated CRD YAML should include the three new fields with correct types and validation
5. The Helm chart CRD should be synced with the generated CRD
