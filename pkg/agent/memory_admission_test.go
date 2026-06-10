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

package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newAdmissionTestModel(source, statusSize string) *inferencev1alpha1.Model {
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.ModelSpec{
			Source: source,
		},
		Status: inferencev1alpha1.ModelStatus{
			Size: statusSize,
		},
	}
}

func newAdmissionTestISVC() *inferencev1alpha1.InferenceService {
	return &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-isvc",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "test-model",
		},
	}
}

// newAdmissionTestAgent builds a MetalAgent whose fake client already holds
// the given InferenceService (with the status subresource enabled so
// Status().Update works) and whose model store is an empty temp dir.
func newAdmissionTestAgent(t *testing.T, isvc *inferencev1alpha1.InferenceService, cfg MetalAgentConfig) *MetalAgent {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(newTestScheme())
	if isvc != nil {
		builder = builder.
			WithObjects(isvc).
			WithStatusSubresource(isvc)
	}
	cfg.K8sClient = builder.Build()
	if cfg.ModelStorePath == "" {
		cfg.ModelStorePath = t.TempDir()
	}
	return NewMetalAgent(cfg)
}

func TestRemoteModelSize_UsesContentLength(t *testing.T) {
	const wantSize = 14_000_000_000
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			t.Errorf("expected HEAD request, got %s", r.Method)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", wantSize))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	size, err := remoteModelSize(context.Background(), ts.URL+"/model.gguf")
	if err != nil {
		t.Fatalf("remoteModelSize returned error: %v", err)
	}
	if size != wantSize {
		t.Errorf("size = %d, want %d", size, wantSize)
	}
}

func TestRemoteModelSize_NonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	if _, err := remoteModelSize(context.Background(), ts.URL+"/gated.gguf"); err == nil {
		t.Fatal("expected error for 401 response, got nil")
	}
}

func TestRemoteModelSize_MissingContentLength(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Chunked response: no Content-Length header.
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	if _, err := remoteModelSize(context.Background(), ts.URL+"/model.gguf"); err == nil {
		t.Fatal("expected error for missing Content-Length, got nil")
	}
}

// The crash scenario from 2026-06-09: model not on disk yet (fresh boot wiped
// /tmp) and Model status.size is the literal string "0". The estimate must
// fall back to a HEAD probe of the source instead of erroring out (which the
// old call site then treated as "proceed without check").
func TestEstimateModelMemory_RemoteHEADFallback(t *testing.T) {
	const wantSize = uint64(20_000_000_000)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", wantSize))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel(ts.URL+"/model.gguf", "0")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err != nil {
		t.Fatalf("estimateModelMemory returned error: %v", err)
	}
	if estimate.WeightsBytes != wantSize {
		t.Errorf("WeightsBytes = %d, want %d", estimate.WeightsBytes, wantSize)
	}
}

func TestEstimateModelMemory_StatusSizePreferredOverRemote(t *testing.T) {
	// Valid status size: must be used without any HTTP traffic (source is
	// unreachable on purpose).
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "20.0 GiB")

	estimate, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err != nil {
		t.Fatalf("estimateModelMemory returned error: %v", err)
	}
	want := uint64(20 * 1024 * 1024 * 1024)
	if estimate.WeightsBytes != want {
		t.Errorf("WeightsBytes = %d, want %d", estimate.WeightsBytes, want)
	}
}

func TestEstimateModelMemory_AllSourcesExhausted(t *testing.T) {
	agent := newAdmissionTestAgent(t, nil, MetalAgentConfig{})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "0")

	_, err := agent.estimateModelMemory(context.Background(), model, 2048, "", "")
	if err == nil {
		t.Fatal("expected error when no size source is available, got nil")
	}
	if !strings.Contains(err.Error(), "cannot determine model size") {
		t.Errorf("error %q should mention being unable to determine model size", err)
	}
}

// Fail closed: when the model size cannot be determined at all, admission
// must reject the service instead of starting an unsized llama-server.
func TestCheckMemoryAdmission_FailsClosedWhenSizeUnknown(t *testing.T) {
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "0")

	err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", "")
	if err == nil {
		t.Fatal("expected admission to fail closed, got nil error")
	}

	updated := &inferencev1alpha1.InferenceService{}
	if getErr := agent.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "test-isvc"}, updated); getErr != nil {
		t.Fatalf("failed to re-fetch InferenceService: %v", getErr)
	}
	if updated.Status.SchedulingStatus != "MemoryCheckFailed" {
		t.Errorf("SchedulingStatus = %q, want %q", updated.Status.SchedulingStatus, "MemoryCheckFailed")
	}
	if updated.Status.SchedulingMessage == "" {
		t.Error("SchedulingMessage should explain why admission failed")
	}
}

func TestCheckMemoryAdmission_WarnModeProceeds(t *testing.T) {
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider:  &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
		MemoryCheckMode: MemoryCheckModeWarn,
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "0")

	if err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
		t.Fatalf("warn mode should preserve the legacy proceed-without-check behavior, got: %v", err)
	}
}

func TestCheckMemoryAdmission_RejectsOverBudget(t *testing.T) {
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 8 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "100.0 GiB")

	err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", "")
	if err == nil {
		t.Fatal("expected insufficient-memory rejection, got nil error")
	}
	if !strings.Contains(err.Error(), "insufficient memory") {
		t.Errorf("error %q should mention insufficient memory", err)
	}

	updated := &inferencev1alpha1.InferenceService{}
	if getErr := agent.config.K8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: "default", Name: "test-isvc"}, updated); getErr != nil {
		t.Fatalf("failed to re-fetch InferenceService: %v", getErr)
	}
	if updated.Status.SchedulingStatus != "InsufficientMemory" {
		t.Errorf("SchedulingStatus = %q, want %q", updated.Status.SchedulingStatus, "InsufficientMemory")
	}
}

func TestCheckMemoryAdmission_PassesWithinBudget(t *testing.T) {
	isvc := newAdmissionTestISVC()
	agent := newAdmissionTestAgent(t, isvc, MetalAgentConfig{
		MemoryProvider: &mockMemoryProvider{totalBytes: 128 * 1024 * 1024 * 1024},
		MemoryFraction: 0.75,
	})
	model := newAdmissionTestModel("https://model-host.invalid/model.gguf", "20.0 GiB")

	if err := agent.checkMemoryAdmission(context.Background(), isvc, model, 2048, "", ""); err != nil {
		t.Fatalf("model within budget should pass admission, got: %v", err)
	}
}
