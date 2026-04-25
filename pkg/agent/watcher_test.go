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
	"errors"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestNewInferenceServiceWatcher(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "test-ns", newNopLogger())

	if watcher == nil {
		t.Fatal("NewInferenceServiceWatcher returned nil")
	}
	if watcher.namespace != "test-ns" {
		t.Errorf("namespace = %q, want %q", watcher.namespace, "test-ns")
	}
}

func TestShouldWatch(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	metalModel := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "metal-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf",
			Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "metal",
			},
		},
	}
	cudaModel := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cuda-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf",
			Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cuda",
			},
		},
	}
	cpuModel := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cpu-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/model.gguf",
			Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{
				Accelerator: "cpu",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(metalModel, cudaModel, cpuModel).
		Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())

	tests := []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
		want bool
	}{
		{
			name: "metal model should be watched",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "metal-svc",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "metal-model",
				},
			},
			want: true,
		},
		{
			name: "cuda model should not be watched",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cuda-svc",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "cuda-model",
				},
			},
			want: false,
		},
		{
			name: "cpu model should not be watched",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cpu-svc",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "cpu-model",
				},
			},
			want: false,
		},
		{
			name: "missing model should not be watched",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "missing-svc",
					Namespace: "default",
				},
				Spec: inferencev1alpha1.InferenceServiceSpec{
					ModelRef: "nonexistent-model",
				},
			},
			want: false,
		},
		{
			name: "empty modelRef should not be watched",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "empty-ref-svc",
					Namespace: "default",
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := watcher.shouldWatch(context.Background(), tt.isvc)
			if got != tt.want {
				t.Errorf("shouldWatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseKey(t *testing.T) {
	tests := []struct {
		input         string
		wantNamespace string
		wantName      string
	}{
		{"default/my-model", "default", "my-model"},
		{"production/llama-3.2-3b", "production", "llama-3.2-3b"},
		{"ns/name/with/slashes", "ns", "name/with/slashes"},
		{"just-name", "", "just-name"},
		{"a/b", "a", "b"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ns, name := parseKey(tt.input)
			if ns != tt.wantNamespace {
				t.Errorf("parseKey(%q) namespace = %q, want %q", tt.input, ns, tt.wantNamespace)
			}
			if name != tt.wantName {
				t.Errorf("parseKey(%q) name = %q, want %q", tt.input, name, tt.wantName)
			}
		})
	}
}

func TestListExisting_EmptyCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	eventChan := make(chan InferenceServiceEvent, 10)

	err := watcher.listExisting(context.Background(), eventChan)
	if err != nil {
		t.Fatalf("listExisting returned error: %v", err)
	}
	if len(eventChan) != 0 {
		t.Errorf("listExisting produced %d events, want 0 for empty cluster", len(eventChan))
	}
}

func TestListExisting_WithServices(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	replicas := int32(1)

	// Create Model resources with metal accelerator
	modelA := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "model-a", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/a.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	modelB := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "model-b", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/b.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}

	svcA := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "model-a", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "model-a", Replicas: &replicas},
	}
	svcB := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "model-b", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "model-b", Replicas: &replicas},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(modelA, modelB, svcA, svcB).
		Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	eventChan := make(chan InferenceServiceEvent, 10)

	err := watcher.listExisting(context.Background(), eventChan)
	if err != nil {
		t.Fatalf("listExisting returned error: %v", err)
	}
	if len(eventChan) != 2 {
		t.Errorf("listExisting produced %d events, want 2", len(eventChan))
	}

	// Verify all events are CREATED type
	for range len(eventChan) {
		event := <-eventChan
		if event.Type != EventTypeCreated {
			t.Errorf("event type = %q, want %q", event.Type, EventTypeCreated)
		}
	}
}

func TestListExisting_NamespaceFiltering(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	replicas := int32(1)

	// Both models use metal accelerator
	modelDefault := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "model-default", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/default.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	modelProd := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "model-prod", Namespace: "production"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/prod.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}

	svcDefault := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "model-default", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "model-default", Replicas: &replicas},
	}
	svcProd := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "model-prod", Namespace: "production"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "model-prod", Replicas: &replicas},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(modelDefault, modelProd, svcDefault, svcProd).
		Build()

	// Watch only "default" namespace
	watcher := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	eventChan := make(chan InferenceServiceEvent, 10)

	err := watcher.listExisting(context.Background(), eventChan)
	if err != nil {
		t.Fatalf("listExisting returned error: %v", err)
	}
	if len(eventChan) != 1 {
		t.Errorf("listExisting with namespace filter produced %d events, want 1", len(eventChan))
	}
}

func TestPoll_DetectsNewService(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	replicas := int32(1)
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "new-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/new.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "new-model",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "new-model",
			Replicas: &replicas,
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(model, isvc).
		Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	eventChan := make(chan InferenceServiceEvent, 10)
	seen := make(map[string]string)

	err := watcher.poll(context.Background(), eventChan, seen)
	if err != nil {
		t.Fatalf("poll returned error: %v", err)
	}
	if len(eventChan) != 1 {
		t.Fatalf("poll produced %d events, want 1", len(eventChan))
	}

	event := <-eventChan
	if event.Type != EventTypeCreated {
		t.Errorf("event type = %q, want %q", event.Type, EventTypeCreated)
	}
	if event.InferenceService.Name != "new-model" {
		t.Errorf("event service name = %q, want %q", event.InferenceService.Name, "new-model")
	}
}

func TestPoll_DetectsDeletedService(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	// Empty cluster — no services exist
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	eventChan := make(chan InferenceServiceEvent, 10)

	// Pre-populate "seen" as if we previously saw this service
	seen := map[string]string{
		"default/old-model": "1",
	}

	err := watcher.poll(context.Background(), eventChan, seen)
	if err != nil {
		t.Fatalf("poll returned error: %v", err)
	}
	if len(eventChan) != 1 {
		t.Fatalf("poll produced %d events, want 1", len(eventChan))
	}

	event := <-eventChan
	if event.Type != EventTypeDeleted {
		t.Errorf("event type = %q, want %q", event.Type, EventTypeDeleted)
	}
	if event.InferenceService.Name != "old-model" {
		t.Errorf("event service name = %q, want %q", event.InferenceService.Name, "old-model")
	}
}

func TestSetMaxConsecutiveFailures(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	w := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())

	if w.maxConsecutiveFailures != DefaultMaxConsecutiveFailures {
		t.Errorf("default = %d, want %d", w.maxConsecutiveFailures, DefaultMaxConsecutiveFailures)
	}
	w.SetMaxConsecutiveFailures(7)
	if w.maxConsecutiveFailures != 7 {
		t.Errorf("after Set(7) = %d, want 7", w.maxConsecutiveFailures)
	}
	// Non-positive coerces back to default.
	w.SetMaxConsecutiveFailures(0)
	if w.maxConsecutiveFailures != DefaultMaxConsecutiveFailures {
		t.Errorf("after Set(0) = %d, want %d", w.maxConsecutiveFailures, DefaultMaxConsecutiveFailures)
	}
}

func TestSetPollInterval(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	w := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())

	if w.pollInterval != defaultPollInterval {
		t.Errorf("default poll interval = %v, want %v", w.pollInterval, defaultPollInterval)
	}
	w.SetPollInterval(50 * time.Millisecond)
	if w.pollInterval != 50*time.Millisecond {
		t.Errorf("after Set(50ms) = %v", w.pollInterval)
	}
	// Non-positive ignored.
	w.SetPollInterval(0)
	if w.pollInterval != 50*time.Millisecond {
		t.Errorf("Set(0) should be ignored, got %v", w.pollInterval)
	}
}

// scriptedListClient wraps a fake client and lets each test prescribe a
// failure pattern across successive List calls. failBetween reports whether
// the n-th call (1-indexed) should fail.
func scriptedListClient(t *testing.T, failOnCall func(n int32) bool) (client.Client, *atomic.Int32) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).Build()
	var calls atomic.Int32
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			n := calls.Add(1)
			if failOnCall(n) {
				return errors.New("synthetic list failure")
			}
			return w.List(ctx, list, opts...)
		},
	})
	return c, &calls
}

func TestWatch_StalledExitsAfterThreshold(t *testing.T) {
	// Call 1 (listExisting) succeeds; calls 2+ all fail. With threshold=2 the
	// watcher should bail out after two consecutive poll failures.
	c, calls := scriptedListClient(t, func(n int32) bool { return n > 1 })
	w := NewInferenceServiceWatcher(c, "default", newNopLogger())
	w.SetPollInterval(10 * time.Millisecond)
	w.SetMaxConsecutiveFailures(2)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	eventChan := make(chan InferenceServiceEvent, 1)
	err := w.Watch(ctx, eventChan)
	if !errors.Is(err, ErrWatchStalled) {
		t.Fatalf("Watch returned %v, want ErrWatchStalled", err)
	}
	// Sanity: 1 listExisting + 2 failed polls = exactly 3 List calls. Anything
	// higher means we burned past the threshold without bailing out.
	if got := calls.Load(); got != 3 {
		t.Errorf("List call count = %d, want 3 (1 list-existing + 2 failed polls)", got)
	}
}

func TestWatch_RecoversAndResetsCounter(t *testing.T) {
	// listExisting (call 1) succeeds; polls 2 and 3 fail; everything after
	// succeeds. Threshold of 5 means the recovery on call 4 should reset the
	// counter and Watch should never return ErrWatchStalled — only the
	// context timeout exits the loop.
	c, calls := scriptedListClient(t, func(n int32) bool { return n == 2 || n == 3 })
	w := NewInferenceServiceWatcher(c, "default", newNopLogger())
	w.SetPollInterval(10 * time.Millisecond)
	w.SetMaxConsecutiveFailures(5)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	eventChan := make(chan InferenceServiceEvent, 1)
	err := w.Watch(ctx, eventChan)

	if errors.Is(err, ErrWatchStalled) {
		t.Errorf("Watch returned ErrWatchStalled despite mid-run recovery; counter did not reset")
	}
	// We should see at least 4 calls (list-existing + the two failures + at
	// least one recovery) within the 500ms test window at a 10ms tick.
	if got := calls.Load(); got < 4 {
		t.Errorf("List call count = %d, want >= 4 to prove recovery exercised", got)
	}
}

func TestWatch_ContextCancellation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	watcher := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	eventChan := make(chan InferenceServiceEvent, 10)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- watcher.Watch(ctx, eventChan)
	}()

	// Give the watcher a moment to start, then cancel
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Watch returned error on cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Watch did not return after context cancellation")
	}
}
