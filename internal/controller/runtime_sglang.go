/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controller — sglang runtime backend. Mirrors the vLLM backend
// (runtime_vllm.go + runtime_vllm_args.go) but emits SGLang's CLI flags.
// SGLang is GPU-only; CPU-only deployments will fail to start the container.
// The `sglangLog` package logger carries construction-time warnings from
// BuildArgs that aren't fatal to reconciliation.

package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

var sglangLog = logf.Log.WithName("runtime.sglang")

// RuntimeSGLANG is the InferenceService.Spec.Runtime value that selects the
// SGLang backend.
const RuntimeSGLANG = "sglang"

// ConditionSGLangSpecValid is the metav1.Condition type set by the reconciler
// when the SGLangConfig portion of the spec is structurally invalid but not
// fatal to reconciliation (e.g., speculative enabled without an algorithm).
const ConditionSGLangSpecValid = "SGLangSpecValid"

// sglangCUDAImage is the pinned default CUDA image. Tag verified at PR time
// against Docker Hub; bump in a follow-up if a newer SGLang release is required.
const sglangCUDAImage = "lmsysorg/sglang:v0.4.10.post2-cu126"

// sglangROCmImage is the AMD ROCm variant. Selected by resolveRuntimeImage
// when model.Spec.Hardware.GPU.Vendor == "amd".
const sglangROCmImage = "lmsysorg/sglang:v0.4.10.post2-rocm630"

// SGLangBackend generates container configuration for the SGLang inference
// server (https://github.com/sgl-project/sglang).
type SGLangBackend struct{}

func (b *SGLangBackend) ContainerName() string    { return "sglang" }
func (b *SGLangBackend) DefaultImage() string     { return sglangCUDAImage }
func (b *SGLangBackend) DefaultPort() int32       { return 30000 }
func (b *SGLangBackend) NeedsModelInit() bool     { return true }
func (b *SGLangBackend) DefaultHPAMetric() string { return "sglang:num_requests_running" }

// BuildArgs generates the SGLang server CLI arguments. Order:
//  1. --model-path, --host, --port (always)
//  2. --served-model-name (auto-derived from ModelRef / model name)
//  3. typed SGLangConfig flags in declaration order
//  4. --tp auto-derived from gpuCount when unset
//  5. --is-embedding when spec.mode == "embedding" (skip if user set it)
//  6. spec.extraArgs last (user wins on collision)
//
// GPU-only flags (memFractionStatic) log a warning when set on a CPU model.
func (b *SGLangBackend) BuildArgs(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model, modelPath string, port int32) []string {
	source := modelPath
	if source == "" {
		source = normalizeHFSource(model.Spec.Source)
	}
	args := []string{
		"--model-path", source,
		// Bind the dual-stack wildcard so pods are reachable on IPv6-only
		// clusters (#972). SGLang keeps the last occurrence of a repeated
		// flag, so users can override via extraArgs ("--host", "0.0.0.0").
		"--host", "::",
		"--port", fmt.Sprintf("%d", port),
	}
	// Mode handling and typed SGLangConfig flags land here in Task 3.
	_ = isvc
	_ = model
	return args
}

// ValidateSGLangConfig checks the SGLangConfig for structurally invalid
// combinations. Returns (reason, message) when invalid; empty strings when
// fine. Caller translates into a metav1.Condition. Implemented in Task 7.
func ValidateSGLangConfig(isvc *inferencev1alpha1.InferenceService) (reason, message string) {
	_ = isvc
	return "", ""
}

// BuildCommand returns the entrypoint for the SGLang container. SGLang
// launches via a Python module rather than a bare binary. Implemented in
// Task 4.
func (b *SGLangBackend) BuildCommand() []string { return nil }

// BuildProbes returns startup, liveness, and readiness probes. SGLang exposes
// /health (cheap liveness) and /health_generate (runs a token, accurate
// readiness but slow on cold start). Implemented in Task 5.
func (b *SGLangBackend) BuildProbes(port int32) (*corev1.Probe, *corev1.Probe, *corev1.Probe) {
	_ = port
	return nil, nil, nil
}

// BuildEnv returns HF_TOKEN from a Secret ref when configured. Implemented
// in Task 6.
func (b *SGLangBackend) BuildEnv(isvc *inferencev1alpha1.InferenceService) []corev1.EnvVar {
	_ = isvc
	return nil
}

// Forward-declared for later tasks; silenced to satisfy unused linter.
var (
	_ = sglangLog
	_ = sglangROCmImage
)
