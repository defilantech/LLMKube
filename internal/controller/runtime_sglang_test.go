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

package controller

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// sglangFlagsNeverInBase is the list of SGLang-specific flags that must NOT
// appear in BuildArgs output when SGLangConfig is nil or empty. Shared across
// TestSGLangBuildArgs_NilConfig and TestSGLangBuildArgs_EmptyConfig.
var sglangFlagsNeverInBase = []string{
	"--tp", "--ep", "--dp", "--context-length", "--mem-fraction-static",
	"--chunked-prefill-size", "--max-running-requests", "--quantization",
	"--kv-cache-dtype", "--attention-backend", "--enable-prefix-caching",
	"--tool-call-parser", "--reasoning-parser", "--chat-template",
	"--speculative-algorithm", "--lora-modules", "--max-lora-rank",
	"--lora-target-modules", "--is-embedding", "--served-model-name",
}

// TestSGLangBackendDefaults locks in the trivial-method contracts that every
// runtime backend exposes (image, port, container name, model-init flag,
// HPA metric). Mirrors the structure of VLLMBackend tests.
func TestSGLangBackendDefaults(t *testing.T) {
	b := &SGLangBackend{}
	if got := b.ContainerName(); got != "sglang" {
		t.Errorf("ContainerName() = %q, want %q", got, "sglang")
	}
	if got := b.DefaultImage(); got != sglangCUDAImage {
		t.Errorf("DefaultImage() = %q, want %q", got, sglangCUDAImage)
	}
	if got := b.DefaultPort(); got != 30000 {
		t.Errorf("DefaultPort() = %d, want 30000", got)
	}
	if !b.NeedsModelInit() {
		t.Error("NeedsModelInit() = false, want true")
	}
	if got := b.DefaultHPAMetric(); got != "sglang:num_requests_running" {
		t.Errorf("DefaultHPAMetric() = %q, want %q", got, "sglang:num_requests_running")
	}
}

// TestSGLangBuildArgs_NilConfig asserts the base arg emission when no
// SGLangConfig is provided.
func TestSGLangBuildArgs_NilConfig(t *testing.T) {
	backend := &SGLangBackend{}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:  "sglang",
			ModelRef: "test-model",
		},
	}
	args := backend.BuildArgs(isvc, model, "/models/test", 30000)

	mustContain := []FlagCheck{
		{"--model-path", "/models/test"},
		{"--host", "::"},
		{"--port", "30000"},
	}
	for _, fc := range mustContain {
		if !containsArg(args, fc.flag, fc.value) {
			t.Errorf("expected %q %q in args, got: %v", fc.flag, fc.value, args)
		}
	}
	for _, f := range sglangFlagsNeverInBase {
		if containsArg(args, f, "") {
			t.Errorf("expected %q NOT in args, got: %v", f, args)
		}
	}
}

// TestSGLangBuildArgs_EmptyConfig asserts the same base flags when an empty
// (non-nil) SGLangConfig is provided.
func TestSGLangBuildArgs_EmptyConfig(t *testing.T) {
	backend := &SGLangBackend{}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "isvc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			Runtime:      "sglang",
			ModelRef:     "test-model",
			SGLangConfig: &inferencev1alpha1.SGLangConfig{},
		},
	}
	args := backend.BuildArgs(isvc, model, "/models/test", 30000)

	for _, fc := range []FlagCheck{{"--model-path", "/models/test"}, {"--host", "::"}, {"--port", "30000"}} {
		if !containsArg(args, fc.flag, fc.value) {
			t.Errorf("expected %q %q in args, got: %v", fc.flag, fc.value, args)
		}
	}
	for _, f := range sglangFlagsNeverInBase {
		if containsArg(args, f, "") {
			t.Errorf("expected %q NOT in args, got: %v", f, args)
		}
	}
}

// Placeholder to silence unused warnings until later tasks add to this file.
// (Remove after Task 3 is implemented.)
var _ = reflect.DeepEqual
var _ = strings.Contains
var _ = corev1.EnvVar{}
