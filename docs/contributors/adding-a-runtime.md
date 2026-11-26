# Adding a New Runtime Backend

LLMKube supports pluggable runtime backends for deploying different inference engines via the InferenceService CRD. This guide walks through adding a new runtime.

## Steps

### 1. Implement the RuntimeBackend interface

Create `internal/controller/runtime_yourengine.go`:

```go
package controller

type YourEngineBackend struct{}

func (b *YourEngineBackend) ContainerName() string { return "your-engine" }
func (b *YourEngineBackend) DefaultImage() string  { return "your-org/your-engine:latest" }
func (b *YourEngineBackend) DefaultPort() int32    { return 8000 }
func (b *YourEngineBackend) NeedsModelInit() bool  { return true }

func (b *YourEngineBackend) BuildArgs(isvc, model, modelPath, port) []string {
    // Return the container arguments for your engine
}

func (b *YourEngineBackend) BuildProbes(port) (startup, liveness, readiness) {
    // Return health check probes (HTTP, TCP, or exec)
}
```

### 2. Implement optional interfaces

- `CommandBuilder` — if your engine needs a custom entrypoint (e.g., `python -m server`)
- `EnvBuilder` — if your engine needs environment variables (e.g., HF_TOKEN)
- `HPAMetricProvider` — if your engine exposes a Prometheus metric for autoscaling

### 3. Add runtime-specific config (optional)

In `api/v1alpha1/inferenceservice_types.go`:

```go
type YourEngineConfig struct {
    // Your engine-specific fields
    SomeOption *int32 `json:"someOption,omitempty"`
}
```

Add the field to `InferenceServiceSpec`:

```go
YourEngineConfig *YourEngineConfig `json:"yourEngineConfig,omitempty"`
```

### 4. Register the backend

In `internal/controller/runtime.go`, add to `resolveBackend()`:

```go
case "yourengine":
    return &YourEngineBackend{}
```

### 5. Add the enum value

In `api/v1alpha1/inferenceservice_types.go`, update the runtime validation:

```go
// +kubebuilder:validation:Enum=llamacpp;llamacpp-router;personaplex;vllm;tgi;sglang;generic;yourengine;mlx-server;omlx;vllm-swift;ollama;tensorfold
```

Do not add a `+kubebuilder:default` to this field. An unset runtime must stay
empty: the controller treats it as `llamacpp`, and the metal-agent treats it as
"use my `--runtime` flag" (#525).

A runtime served only by the metal-agent (no in-cluster backend) also goes in
`IsMetalOnlyRuntime` in `api/v1alpha1/constants.go`, so the controller refuses
it on non-Metal Models instead of falling through to llama.cpp.

### 6. Regenerate and sync

```bash
make manifests generate
```

Sync CRDs to Helm chart (preserve `crds.install` and `crds.keep` guards):

```bash
for crd in inferenceservices models; do
  src="config/crd/bases/inference.llmkube.dev_${crd}.yaml"
  dst="charts/llmkube/templates/crds/${crd}.yaml"
  # See existing templates for the guard pattern
done
```

### 7. Add tests

In `internal/controller/inferenceservice_controller_test.go`:

- Backend defaults (ContainerName, DefaultImage, DefaultPort, NeedsModelInit)
- `resolveBackend()` returns your backend for the runtime string
- `BuildArgs()` with your config options
- `BuildProbes()` returns expected probe type

### 8. Update CLI (optional)

Add `--runtime yourengine` handling in `pkg/cli/deploy.go`.

## Existing Runtimes

| Runtime | Engine | Port | Probes | Model Init | HPA Metric |
|---------|--------|------|--------|------------|------------|
| `llamacpp` | llama.cpp | 8080 | HTTP /health | Yes (curl) | llamacpp:requests_processing |
| `personaplex` | PersonaPlex/Moshi | 8998 | TCP socket | No | — |
| `vllm` | vLLM | 8000 | HTTP /health | Yes | vllm:num_requests_running |
| `tgi` | TGI | 80 | HTTP /health | No (HF download) | tgi:queue_size |
| `sglang` | SGLang | 30000 | HTTP /health_generate | Yes (curl) | sglang:num_running_reqs |
| `generic` | Any container | 8080 | TCP socket | No (opt in with `spec.stageModel`) | — |

The metal-agent runtimes `mlx-server`, `omlx`, `vllm-swift`, `ollama`, and
`tensorfold` are served through the out-of-cluster `metal-agent` rather than
as pods, and need a Model with `hardware.accelerator: metal`. In LLMKube, the
agent executors for `mlx-server`, `omlx`, `vllm-swift`, and `tensorfold` run
on Apple Silicon hosts (macOS), while `ollama` talks to a host daemon on
macOS or Linux. See `deployment/macos/README.md`.

A new metal-agent runtime has no `RuntimeBackend`. Instead it needs a
`ProcessExecutor` in `pkg/agent/` (`executor_tensorfold.go` is a compact
template: per-process log, reaper-tracked child, health wait that ends when the
child exits), a registration in `MetalAgent.buildExecutors`, a case in
`validateRuntimeFormat`, a binary flag in `cmd/metal-agent/main.go`, and its
name in the runtime list of `TestCRDRuntimeContract`.
