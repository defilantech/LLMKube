/*
Copyright 2026.

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
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
			got := watchDecision(t, watcher, tt.isvc)
			if got != tt.want {
				t.Errorf("shouldWatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSetNameAllowlist_FiltersInShouldWatch pins #524 partition behavior:
// when an allowlist is set, only InferenceServices whose name is on the
// list pass shouldWatch, regardless of accelerator. Empty / nil allowlist
// preserves the v0.1 behavior of accepting every metal-accelerator
// InferenceService.
func TestSetNameAllowlist_FiltersInShouldWatch(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	metalModel := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "metal-model", Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   "https://example.com/model.gguf",
			Format:   "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(metalModel).Build()

	mkISvc := func(name string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "metal-model"},
		}
	}

	t.Run("no allowlist: every metal ISvc passes (v0.1 behavior)", func(t *testing.T) {
		w := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
		for _, name := range []string{"qwen36-35b-a3b", "devstral-24b", "anything-else"} {
			if !watchDecision(t, w, mkISvc(name)) {
				t.Errorf("%q: want true, got false", name)
			}
		}
	})

	t.Run("allowlist set: only listed names pass", func(t *testing.T) {
		w := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
		w.SetNameAllowlist([]string{"qwen36-35b-a3b"})
		if !watchDecision(t, w, mkISvc("qwen36-35b-a3b")) {
			t.Error("qwen36-35b-a3b: want true (on allowlist), got false")
		}
		if watchDecision(t, w, mkISvc("devstral-24b")) {
			t.Error("devstral-24b: want false (not on allowlist), got true")
		}
	})

	t.Run("allowlist with multiple names", func(t *testing.T) {
		w := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
		w.SetNameAllowlist([]string{"qwen36-35b-a3b", "devstral-24b"})
		for _, name := range []string{"qwen36-35b-a3b", "devstral-24b"} {
			if !watchDecision(t, w, mkISvc(name)) {
				t.Errorf("%q: want true (on allowlist), got false", name)
			}
		}
		if watchDecision(t, w, mkISvc("third-model")) {
			t.Error("third-model: want false (not on allowlist), got true")
		}
	})

	t.Run("allowlist with empty/whitespace entries gets normalized", func(t *testing.T) {
		w := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
		w.SetNameAllowlist([]string{"", "  qwen36-35b-a3b  ", ""})
		if !watchDecision(t, w, mkISvc("qwen36-35b-a3b")) {
			t.Error("qwen36-35b-a3b: want true after whitespace trim, got false")
		}
		if watchDecision(t, w, mkISvc("other")) {
			t.Error("other: want false (not on allowlist), got true")
		}
	})

	t.Run("re-clearing allowlist with empty slice restores v0.1 behavior", func(t *testing.T) {
		w := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
		w.SetNameAllowlist([]string{"qwen36-35b-a3b"})
		w.SetNameAllowlist(nil)
		if !watchDecision(t, w, mkISvc("devstral-24b")) {
			t.Error("devstral-24b: want true after allowlist cleared, got false")
		}
	})

	t.Run("allowlist skips ISvc before the (more expensive) Model lookup", func(t *testing.T) {
		// Watcher with NO model in the fake client; if the allowlist
		// skip didn't short-circuit, shouldWatch would try (and fail)
		// to Get the Model. Confirms ordering: allowlist filter runs
		// before the API call.
		emptyClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		w := NewInferenceServiceWatcher(emptyClient, "default", newNopLogger())
		w.SetNameAllowlist([]string{"yes"})
		if watchDecision(t, w, mkISvc("no")) {
			t.Error("non-allowlisted name should return false without hitting the API")
		}
	})
}

// watchDecision calls shouldWatch and fails the test on a lookup error, for
// cases whose fake client answers every Model lookup (found or NotFound).
func watchDecision(t *testing.T, w *InferenceServiceWatcher, isvc *inferencev1alpha1.InferenceService) bool {
	t.Helper()
	watch, err := w.shouldWatch(context.Background(), isvc)
	if err != nil {
		t.Fatalf("shouldWatch(%s/%s) returned error: %v", isvc.Namespace, isvc.Name, err)
	}
	return watch
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

const (
	lookupModelName = "lookup-model"
	lookupISVCName  = "lookup-svc"
)

// modelLookupFixture is a watcher over one metal Model and one
// InferenceService that references it. While failModelGet is set, every Model
// Get through the watcher's client returns it; InferenceService lists and
// writes through base are untouched.
type modelLookupFixture struct {
	base         client.WithWatch
	watcher      *InferenceServiceWatcher
	failModelGet error
}

func newModelLookupFixture(t *testing.T) *modelLookupFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: lookupModelName, Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source: "https://example.com/lookup.gguf", Format: "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: lookupISVCName, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: lookupModelName},
	}
	f := &modelLookupFixture{}
	f.base = fake.NewClientBuilder().WithScheme(scheme).WithObjects(model, isvc).Build()
	c := interceptor.NewClient(f.base, interceptor.Funcs{
		Get: func(
			ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if _, isModel := obj.(*inferencev1alpha1.Model); isModel && f.failModelGet != nil {
				return f.failModelGet
			}
			return w.Get(ctx, key, obj, opts...)
		},
	})
	f.watcher = NewInferenceServiceWatcher(c, "default", newNopLogger())
	return f
}

// pollOnce runs one poll against seen and returns the events it emitted.
func (f *modelLookupFixture) pollOnce(t *testing.T, seen map[string]string) []InferenceServiceEvent {
	t.Helper()
	return f.pollWithContext(context.Background(), t, seen)
}

// pollWithContext is pollOnce with a caller-supplied context.
func (f *modelLookupFixture) pollWithContext(
	ctx context.Context, t *testing.T, seen map[string]string,
) []InferenceServiceEvent {
	t.Helper()
	eventChan := make(chan InferenceServiceEvent, 10)
	if err := f.watcher.poll(ctx, eventChan, seen); err != nil {
		t.Fatalf("poll returned error: %v", err)
	}
	events := make([]InferenceServiceEvent, 0, len(eventChan))
	for len(eventChan) > 0 {
		events = append(events, <-eventChan)
	}
	return events
}

// eventSummary renders events as "TYPE ns/name" for failure messages.
func eventSummary(events []InferenceServiceEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, string(e.Type)+" "+e.InferenceService.Namespace+"/"+e.InferenceService.Name)
	}
	return out
}

// TestPoll_ModelLookupErrorKeepsService pins #2076: a Model Get that fails
// with anything but NotFound must not emit Deleted for a running service
// (that stops its engine and deletes its Service and EndpointSlice), must not
// emit Updated for a change made meanwhile, and must not touch its seen entry.
// The next successful poll then compares against the recorded resourceVersion
// as usual: nothing when the service is unchanged, one Updated when it changed
// while the lookup failed.
func TestPoll_ModelLookupErrorKeepsService(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"transport error", errors.New("dial tcp 10.0.0.1:6443: i/o timeout")},
		{"API server unavailable", apierrors.NewServiceUnavailable("apiserver restarting")},
		{"429 Too Many Requests", apierrors.NewTooManyRequests("slow down", 1)},
	}
	for _, tc := range cases {
		for _, changed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/changed during failure=%t", tc.name, changed), func(t *testing.T) {
				f := newModelLookupFixture(t)
				seen := map[string]string{}

				events := f.pollOnce(t, seen)
				if len(events) != 1 || events[0].Type != EventTypeCreated {
					t.Fatalf("poll 1 events = %v, want one CREATED", eventSummary(events))
				}
				before := maps.Clone(seen)

				if changed {
					f.annotateISVC(t)
				}
				f.failModelGet = tc.err
				if events := f.pollOnce(t, seen); len(events) != 0 {
					t.Errorf("poll 2 (model lookup failing) events = %v, want none", eventSummary(events))
				}
				if !maps.Equal(seen, before) {
					t.Errorf("poll 2 (model lookup failing) changed seen to %v, want %v", seen, before)
				}

				f.failModelGet = nil
				events = f.pollOnce(t, seen)
				switch {
				case !changed && len(events) != 0:
					t.Errorf("poll 3 (lookup recovered, resourceVersion unchanged) events = %v, want none",
						eventSummary(events))
				case changed && (len(events) != 1 || events[0].Type != EventTypeUpdated):
					t.Errorf("poll 3 (lookup recovered, service changed during the failure) events = %v, "+
						"want one UPDATED", eventSummary(events))
				}
			})
		}
	}
}

// annotateISVC changes the fixture's InferenceService through the base
// client, bumping its resourceVersion the way any spec or metadata edit does.
func (f *modelLookupFixture) annotateISVC(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{}
	if err := f.base.Get(ctx, client.ObjectKey{Namespace: "default", Name: lookupISVCName}, isvc); err != nil {
		t.Fatalf("get isvc: %v", err)
	}
	isvc.Annotations = map[string]string{"example.com/touched": "true"}
	if err := f.base.Update(ctx, isvc); err != nil {
		t.Fatalf("update isvc: %v", err)
	}
}

// lookupLogCounts returns how many "referenced model lookup" log lines were
// written at each level since the last call, and clears them.
func lookupLogCounts(logs *observer.ObservedLogs) map[zapcore.Level]int {
	counts := map[zapcore.Level]int{}
	for _, e := range logs.FilterMessageSnippet("referenced model lookup").All() {
		counts[e.Level]++
	}
	logs.TakeAll()
	return counts
}

// TestPoll_ModelLookupFailureLogsTransitions pins the log volume side of
// #2076: a failing Model lookup logs one Warn (with the error) when it starts,
// Debug while it continues and one Info when it clears, and the
// watch_model_lookup_failing_services gauge counts the service only while it
// fails, including dropping it when the service is deleted mid-failure (which
// still emits Deleted).
func TestPoll_ModelLookupFailureLogsTransitions(t *testing.T) {
	f := newModelLookupFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	f.watcher.logger = zap.New(core).Sugar()
	seen := map[string]string{}
	lookupErr := apierrors.NewServiceUnavailable("apiserver restarting")
	// The gauge is process-wide and other tests leave it set: start from a
	// known value so every reading below comes from this watcher.
	watchModelLookupFailingServices.Set(0)
	gauge := func() float64 { return testutil.ToFloat64(watchModelLookupFailingServices) }

	f.pollOnce(t, seen)
	if got := lookupLogCounts(logs); len(got) != 0 {
		t.Fatalf("poll 1 (lookup ok) lookup log lines = %v, want none", got)
	}

	f.failModelGet = lookupErr
	f.pollOnce(t, seen)
	warns := logs.FilterMessageSnippet("referenced model lookup failed").FilterLevelExact(zap.WarnLevel).All()
	if len(warns) != 1 {
		t.Fatalf("poll 2 (lookup starts failing) Warn lines = %d, want 1 (all: %v)", len(warns), logs.All())
	}
	if got, _ := warns[0].ContextMap()["error"].(string); !strings.Contains(got, "apiserver restarting") {
		t.Errorf("poll 2 Warn error field = %q, want the lookup error", got)
	}
	if got := lookupLogCounts(logs); got[zap.WarnLevel] != 1 || got[zap.InfoLevel] != 0 {
		t.Errorf("poll 2 lookup log lines by level = %v, want exactly one Warn", got)
	}
	if g := gauge(); g != 1 {
		t.Errorf("poll 2 gauge = %v, want 1", g)
	}

	f.pollOnce(t, seen)
	if got := lookupLogCounts(logs); got[zap.WarnLevel] != 0 || got[zap.InfoLevel] != 0 || got[zap.DebugLevel] != 1 {
		t.Errorf("poll 3 (still failing) lookup log lines by level = %v, want exactly one Debug", got)
	}
	if g := gauge(); g != 1 {
		t.Errorf("poll 3 gauge = %v, want 1", g)
	}

	f.failModelGet = nil
	f.pollOnce(t, seen)
	if got := lookupLogCounts(logs); got[zap.InfoLevel] != 1 || got[zap.WarnLevel] != 0 {
		t.Errorf("poll 4 (recovered) lookup log lines by level = %v, want exactly one Info", got)
	}
	if g := gauge(); g != 0 {
		t.Errorf("poll 4 gauge = %v, want 0", g)
	}

	f.failModelGet = lookupErr
	f.pollOnce(t, seen)
	if got := lookupLogCounts(logs); got[zap.WarnLevel] != 1 {
		t.Errorf("poll 5 (fails again) lookup log lines by level = %v, want a new Warn", got)
	}
	if err := f.base.Delete(context.Background(), &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: lookupISVCName, Namespace: "default"},
	}); err != nil {
		t.Fatalf("delete isvc: %v", err)
	}
	events := f.pollOnce(t, seen)
	if len(events) != 1 || events[0].Type != EventTypeDeleted {
		t.Errorf("poll 6 (deleted while failing) events = %v, want one DELETED", eventSummary(events))
	}
	if g := gauge(); g != 0 {
		t.Errorf("poll 6 gauge = %v, want 0 once the failing service is gone", g)
	}
}

// TestShouldWatch_ConcurrentLookupFailuresTrackedOnce: shouldWatch may run on
// more than one goroutine (the poll loop, and callers outside the watcher that
// use it as a claim guard). Concurrent failures of one service must still log
// a single Warn and count once in the gauge, and concurrent recoveries a
// single Info. Run with -race, which reports unguarded tracking state.
func TestShouldWatch_ConcurrentLookupFailuresTrackedOnce(t *testing.T) {
	f := newModelLookupFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	f.watcher.logger = zap.New(core).Sugar()
	watchModelLookupFailingServices.Set(0)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: lookupISVCName, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: lookupModelName},
	}

	const callers = 8
	callConcurrently := func() (errs int32) {
		var n atomic.Int32
		var wg sync.WaitGroup
		for range callers {
			wg.Go(func() {
				if _, err := f.watcher.shouldWatch(context.Background(), isvc); err != nil {
					n.Add(1)
				}
			})
		}
		wg.Wait()
		return n.Load()
	}

	f.failModelGet = apierrors.NewServiceUnavailable("apiserver restarting")
	if errs := callConcurrently(); errs != callers {
		t.Errorf("failing lookup: %d of %d calls returned an error, want all", errs, callers)
	}
	if got := lookupLogCounts(logs); got[zap.WarnLevel] != 1 || got[zap.DebugLevel] != callers-1 {
		t.Errorf("failing lookup: log lines by level = %v, want one Warn and %d Debug", got, callers-1)
	}
	if g := testutil.ToFloat64(watchModelLookupFailingServices); g != 1 {
		t.Errorf("failing lookup: gauge = %v, want 1", g)
	}

	f.failModelGet = nil
	if errs := callConcurrently(); errs != 0 {
		t.Errorf("recovered lookup: %d of %d calls returned an error, want none", errs, callers)
	}
	if got := lookupLogCounts(logs); got[zap.InfoLevel] != 1 || got[zap.WarnLevel] != 0 {
		t.Errorf("recovered lookup: log lines by level = %v, want exactly one Info", got)
	}
	if g := testutil.ToFloat64(watchModelLookupFailingServices); g != 0 {
		t.Errorf("recovered lookup: gauge = %v, want 0", g)
	}
}

// TestPoll_ShutdownLookupFailureKeepsStateSilently covers a lookup that fails
// because the poll's context ended (agent shutdown): the service keeps its
// state like any other unknown answer, nothing is logged, and the failure is
// not recorded, so a later real failure still starts with a Warn.
func TestPoll_ShutdownLookupFailureKeepsStateSilently(t *testing.T) {
	f := newModelLookupFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	f.watcher.logger = zap.New(core).Sugar()
	seen := map[string]string{}
	f.pollOnce(t, seen)
	before := maps.Clone(seen)
	logs.TakeAll()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.failModelGet = context.Canceled
	if events := f.pollWithContext(ctx, t, seen); len(events) != 0 {
		t.Errorf("canceled poll events = %v, want none", eventSummary(events))
	}
	if !maps.Equal(seen, before) {
		t.Errorf("canceled poll changed seen to %v, want %v", seen, before)
	}
	if got := lookupLogCounts(logs); len(got) != 0 {
		t.Errorf("canceled poll lookup log lines by level = %v, want none", got)
	}

	f.failModelGet = apierrors.NewServiceUnavailable("apiserver restarting")
	f.pollOnce(t, seen)
	if got := lookupLogCounts(logs); got[zap.WarnLevel] != 1 {
		t.Errorf("first real failure after a canceled poll: lookup log lines by level = %v, want one Warn", got)
	}
}

// TestShouldWatch_DeadlineExceededLookupIsLoggedAndCounted: only cancellation
// (shutdown) silences a failed lookup. A caller's context whose deadline
// expired while the Get was outstanding points at a slow API server, so the
// failure is logged and counted like any other.
func TestShouldWatch_DeadlineExceededLookupIsLoggedAndCounted(t *testing.T) {
	f := newModelLookupFixture(t)
	core, logs := observer.New(zap.DebugLevel)
	f.watcher.logger = zap.New(core).Sugar()
	watchModelLookupFailingServices.Set(0)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: lookupISVCName, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: lookupModelName},
	}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	f.failModelGet = context.DeadlineExceeded
	if _, err := f.watcher.shouldWatch(ctx, isvc); err == nil {
		t.Fatal("shouldWatch returned no error for a failed lookup, want the lookup error")
	}
	if got := lookupLogCounts(logs); got[zap.WarnLevel] != 1 {
		t.Errorf("deadline-exceeded lookup: log lines by level = %v, want one Warn", got)
	}
	if g := testutil.ToFloat64(watchModelLookupFailingServices); g != 1 {
		t.Errorf("deadline-exceeded lookup: gauge = %v, want 1", g)
	}
}

// TestWatcher_ModelLookupErrorDoesNotAddUnseenService covers the other half of
// keeping the previous state (#2076): a service the watcher has not seen yet
// is not started while its Model lookup fails, by either the startup list or a
// poll, and the first poll whose lookup succeeds emits Created for it.
func TestWatcher_ModelLookupErrorDoesNotAddUnseenService(t *testing.T) {
	f := newModelLookupFixture(t)
	f.failModelGet = apierrors.NewServiceUnavailable("apiserver restarting")

	eventChan := make(chan InferenceServiceEvent, 10)
	if err := f.watcher.listExisting(context.Background(), eventChan); err != nil {
		t.Fatalf("listExisting returned error: %v", err)
	}
	if len(eventChan) != 0 {
		t.Errorf("listExisting emitted %d events while the model lookup fails, want 0", len(eventChan))
	}

	seen := map[string]string{}
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Errorf("poll (model lookup failing) events = %v, want none", eventSummary(events))
	}
	if len(seen) != 0 {
		t.Errorf("poll (model lookup failing) recorded seen = %v, want empty", seen)
	}

	f.failModelGet = nil
	if events := f.pollOnce(t, seen); len(events) != 1 || events[0].Type != EventTypeCreated {
		t.Errorf("poll after recovery events = %v, want one CREATED", eventSummary(events))
	}
}

// TestPoll_ModelGoneOrNotMetalStillDeletes keeps the #2076 fix from
// overreaching: a Model that stays deleted (NotFound), or that stops being a
// metal Model, still removes the service from this agent. A deleted Model
// does so once defaultModelGoneGrace has passed (#2411), so a
// delete-and-recreate keeps the engine; held is that case.
func TestPoll_ModelGoneOrNotMetalStillDeletes(t *testing.T) {
	modelKey := client.ObjectKey{Namespace: "default", Name: lookupModelName}
	cases := []struct {
		name   string
		held   bool
		change func(ctx context.Context, c client.Client) error
	}{
		{"model deleted", true, func(ctx context.Context, c client.Client) error {
			return c.Delete(ctx, &inferencev1alpha1.Model{
				ObjectMeta: metav1.ObjectMeta{Name: modelKey.Name, Namespace: modelKey.Namespace},
			})
		}},
		{"accelerator switched to cuda", false, func(ctx context.Context, c client.Client) error {
			model := &inferencev1alpha1.Model{}
			if err := c.Get(ctx, modelKey, model); err != nil {
				return err
			}
			model.Spec.Hardware.Accelerator = "cuda"
			return c.Update(ctx, model)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newModelLookupFixture(t)
			advance := f.withClock()
			seen := map[string]string{}
			if events := f.pollOnce(t, seen); len(events) != 1 || events[0].Type != EventTypeCreated {
				t.Fatalf("poll 1 events = %v, want one CREATED", eventSummary(events))
			}

			if err := tc.change(context.Background(), f.base); err != nil {
				t.Fatalf("change model: %v", err)
			}
			if tc.held {
				if events := f.pollOnce(t, seen); len(events) != 0 {
					t.Fatalf("poll inside the grace events = %v, want none", eventSummary(events))
				}
				advance(defaultModelGoneGrace)
			}

			events := f.pollOnce(t, seen)
			if len(events) != 1 || events[0].Type != EventTypeDeleted ||
				events[0].InferenceService.Name != lookupISVCName {
				t.Errorf("poll 2 events = %v, want one DELETED default/%s", eventSummary(events), lookupISVCName)
			}
			if len(seen) != 0 {
				t.Errorf("poll 2 left seen = %v, want empty", seen)
			}
		})
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

// scriptedListClient wraps a fake client holding objs and lets each test
// prescribe a failure pattern across successive List calls. failOnCall reports
// whether the n-th call (1-indexed) should fail.
func scriptedListClient(
	t *testing.T, failOnCall func(n int32) bool, objs ...client.Object,
) (client.Client, *atomic.Int32) {
	t.Helper()
	return scriptedListErrors(t, func(n int32) error {
		if failOnCall(n) {
			return errors.New("synthetic list failure")
		}
		return nil
	}, objs...)
}

// scriptedListErrors is scriptedListClient with the error itself prescribed:
// the n-th List call (1-indexed) returns errOnCall(n) when that is non-nil.
func scriptedListErrors(
	t *testing.T, errOnCall func(n int32) error, objs ...client.Object,
) (client.Client, *atomic.Int32) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := inferencev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	var calls atomic.Int32
	c := interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, w client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := errOnCall(calls.Add(1)); err != nil {
				return err
			}
			return w.List(ctx, list, opts...)
		},
	})
	return c, &calls
}

// metalServiceObjects returns a Metal Model and an InferenceService that uses
// it, so a watcher claims the service.
func metalServiceObjects() []client.Object {
	return []client.Object{
		&inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: lookupModelName, Namespace: "default"},
			Spec: inferencev1alpha1.ModelSpec{
				Source: "https://example.com/lookup.gguf", Format: "gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
			},
		},
		&inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: lookupISVCName, Namespace: "default"},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: lookupModelName},
		},
	}
}

// apiHookCounter counts the watcher's API hooks; see SetAPIHooks.
type apiHookCounter struct{ resets, reachable atomic.Int32 }

func (h *apiHookCounter) install(w *InferenceServiceWatcher) {
	w.SetAPIHooks(func() { h.resets.Add(1) }, func() { h.reachable.Add(1) })
}

// runWatch runs w.Watch on its own goroutine and returns a stop function that
// cancels it and waits for it to return.
func runWatch(t *testing.T, w *InferenceServiceWatcher, events chan InferenceServiceEvent) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Watch(ctx, events)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Watch did not return after its context was cancelled")
		}
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWatch_RidesOutAnOutagePastTheThreshold: more consecutive failed polls
// than the threshold do not end Watch (#2079). The API counts as unreachable
// from the threshold-th failure: that and the recovery are logged once each,
// the gauge follows, the reset hook runs after every failure from then on,
// nothing is treated as deleted, and the first successful poll carries on
// from the services seen before the outage.
func TestWatch_RidesOutAnOutagePastTheThreshold(t *testing.T) {
	const threshold, failures = 2, 8
	// List calls 1 (the initial list) and 2 (the first poll, which records
	// the service as seen) succeed, polls 3 to 10 fail, the rest succeed.
	c, calls := scriptedListClient(t, func(n int32) bool { return n >= 3 && n < 3+failures },
		metalServiceObjects()...)
	core, logs := observer.New(zap.DebugLevel)
	w := NewInferenceServiceWatcher(c, "default", zap.New(core).Sugar())
	w.SetPollInterval(2 * time.Millisecond)
	w.maxPollBackoff = 10 * time.Millisecond
	w.SetMaxConsecutiveFailures(threshold)
	var hooks apiHookCounter
	hooks.install(w)

	events := make(chan InferenceServiceEvent, 16)
	stop := runWatch(t, w, events)
	defer stop()

	waitUntil(t, "the API to count as unreachable", func() bool { return hooks.resets.Load() >= 1 })
	if g := testutil.ToFloat64(watchAPIUnreachable); g != 1 {
		t.Errorf("api_unreachable gauge during the outage = %v, want 1", g)
	}
	waitUntil(t, "the recovery", func() bool { return hooks.reachable.Load() >= 1 })
	if g := testutil.ToFloat64(watchAPIUnreachable); g != 0 {
		t.Errorf("api_unreachable gauge after the recovery = %v, want 0", g)
	}
	if g := testutil.ToFloat64(watchConsecutiveFailures); g != 0 {
		t.Errorf("watch_consecutive_failures after the recovery = %v, want 0", g)
	}
	after := calls.Load()
	waitUntil(t, "two more polls after the recovery", func() bool { return calls.Load() >= after+2 })
	stop()

	if got, want := hooks.resets.Load(), int32(failures-threshold+1); got != want {
		t.Errorf("reset hook ran %d times, want %d (from the threshold-th failure on)", got, want)
	}
	if got := hooks.reachable.Load(); got != 1 {
		t.Errorf("reachable hook ran %d times, want 1", got)
	}
	if n := logs.FilterMessageSnippet("Kubernetes API unreachable").Len(); n != 1 {
		t.Errorf("logged the API unreachable %d times, want once", n)
	}
	if n := logs.FilterMessageSnippet("Kubernetes API reachable again").Len(); n != 1 {
		t.Errorf("logged the API's return %d times, want once", n)
	}
	if n := logs.FilterMessage("polling error").Len(); n != threshold-1 {
		t.Errorf("logged %d per-failure warnings, want %d (the failures below the threshold)", n, threshold-1)
	}
	close(events)
	var got []EventType
	for ev := range events {
		got = append(got, ev.Type)
	}
	// The initial list and the first successful poll each deliver Created
	// (listExisting does not fill seen); an outage must never read as a
	// deletion.
	if len(got) == 0 || slices.Contains(got, EventTypeDeleted) {
		t.Errorf("events = %v, want the service delivered and never Deleted", got)
	}
}

// TestWatch_RetriesTheInitialListThroughAnOutage: an agent started while the
// API is down waits in Watch, counting the failed initial lists like failed
// polls, and lists the existing services as soon as the API answers.
func TestWatch_RetriesTheInitialListThroughAnOutage(t *testing.T) {
	const threshold, failures = 2, 4
	c, _ := scriptedListClient(t, func(n int32) bool { return n <= failures }, metalServiceObjects()...)
	w := NewInferenceServiceWatcher(c, "default", newNopLogger())
	w.SetPollInterval(2 * time.Millisecond)
	w.maxPollBackoff = 10 * time.Millisecond
	w.SetMaxConsecutiveFailures(threshold)
	var hooks apiHookCounter
	hooks.install(w)

	events := make(chan InferenceServiceEvent, 4)
	stop := runWatch(t, w, events)
	defer stop()

	select {
	case ev := <-events:
		if ev.Type != EventTypeCreated || ev.InferenceService.Name != lookupISVCName {
			t.Fatalf("first event = %s %s, want Created %s", ev.Type, ev.InferenceService.Name, lookupISVCName)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the existing service was never listed after the API came back")
	}
	if got, want := hooks.resets.Load(), int32(failures-threshold+1); got != want {
		t.Errorf("reset hook ran %d times, want %d", got, want)
	}
	if got := hooks.reachable.Load(); got != 1 {
		t.Errorf("reachable hook ran %d times, want 1", got)
	}
}

// TestWatch_RecoversBelowTheThreshold: failures that stop short of the
// threshold are a blip: the counter resets, and the API never counts as
// unreachable (no hook, no transition log).
func TestWatch_RecoversBelowTheThreshold(t *testing.T) {
	c, calls := scriptedListClient(t, func(n int32) bool { return n == 2 || n == 3 })
	core, logs := observer.New(zap.InfoLevel)
	w := NewInferenceServiceWatcher(c, "default", zap.New(core).Sugar())
	w.SetPollInterval(2 * time.Millisecond)
	w.SetMaxConsecutiveFailures(5)
	var hooks apiHookCounter
	hooks.install(w)

	stop := runWatch(t, w, make(chan InferenceServiceEvent, 1))
	waitUntil(t, "polls after the blip", func() bool { return calls.Load() >= 6 })
	stop()

	if r, ok := hooks.resets.Load(), hooks.reachable.Load(); r != 0 || ok != 0 {
		t.Errorf("hooks ran (reset %d, reachable %d) for failures below the threshold, want none", r, ok)
	}
	if n := logs.FilterMessageSnippet("Kubernetes API unreachable").Len(); n != 0 {
		t.Errorf("logged the API unreachable %d times for a blip, want never", n)
	}
	if n := logs.FilterMessage("polling recovered after errors").Len(); n != 1 {
		t.Errorf("logged the blip's recovery %d times, want once", n)
	}
}

// TestNextPollDelay_BacksOffPastTheThresholdWithJitter pins the retry
// schedule at the defaults: the poll interval below the threshold, then
// doubling from twice the interval to the cap, each plus up to 20 % jitter.
func TestNextPollDelay_BacksOffPastTheThresholdWithJitter(t *testing.T) {
	w := NewInferenceServiceWatcher(nil, "default", newNopLogger())
	tests := []struct {
		failures int
		base     time.Duration
		jitter   bool
	}{
		{0, 5 * time.Second, false},
		{DefaultMaxConsecutiveFailures - 1, 5 * time.Second, false},
		{DefaultMaxConsecutiveFailures, 10 * time.Second, true},
		{DefaultMaxConsecutiveFailures + 1, 20 * time.Second, true},
		{DefaultMaxConsecutiveFailures + 2, defaultMaxPollBackoff, true},
		{DefaultMaxConsecutiveFailures + 1000, defaultMaxPollBackoff, true},
	}
	for _, tc := range tests {
		w.listFailures = tc.failures
		distinct := map[time.Duration]bool{}
		for range 200 {
			d := w.nextPollDelay()
			distinct[d] = true
			limit := tc.base
			if tc.jitter {
				limit = time.Duration(float64(tc.base) * (1 + pollBackoffJitter))
			}
			if d < tc.base || d > limit {
				t.Fatalf("failures=%d: delay %v outside [%v, %v]", tc.failures, d, tc.base, limit)
			}
		}
		if tc.jitter && len(distinct) < 2 {
			t.Errorf("failures=%d: 200 delays were all %v, want jitter", tc.failures, tc.base)
		}
	}
}

func TestWatch_ContextCancellation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	watcher := NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	stop := runWatch(t, watcher, make(chan InferenceServiceEvent, 10))

	// Give the watcher a moment to start, then cancel; stop fails the test if
	// Watch does not return.
	time.Sleep(100 * time.Millisecond)
	stop()
}

// A key seeded before the first poll (an engine adopted at startup, #2079) is
// compared like a seen one: an owned service gets Updated, and a key the poll
// does not watch (here one whose service is gone) gets Deleted. The seed is
// used once.
func TestWatcher_SeededKeysAreUpdatedOrDeleted(t *testing.T) {
	f := newModelLookupFixture(t)
	owned, gone := "default/"+lookupISVCName, "default/gone"
	f.watcher.SeedSeen(map[string]bool{owned: true, gone: true})

	events := f.pollOnce(t, f.watcher.initialSeen())
	got := eventSummary(events)
	slices.Sort(got)
	want := []string{"DELETED " + gone, "UPDATED " + owned}
	if !slices.Equal(got, want) {
		t.Fatalf("first poll events = %v, want %v", got, want)
	}
	if again := f.watcher.initialSeen(); len(again) != 0 {
		t.Fatalf("second initialSeen = %v, want the seed used once", again)
	}
}

// While ownership is unknown, a seeded key stays out of the deletion pass, as
// a seen one does (#2076).
func TestWatcher_SeededKeyKeptWhileOwnershipUnknown(t *testing.T) {
	f := newModelLookupFixture(t)
	key := "default/" + lookupISVCName
	f.watcher.SeedSeen(map[string]bool{key: true})
	f.failModelGet = apierrors.NewServiceUnavailable("apiserver restarting")

	seen := f.watcher.initialSeen()
	if events := f.pollOnce(t, seen); len(events) != 0 {
		t.Fatalf("poll with the Model lookup failing emitted %v, want none", eventSummary(events))
	}
	if _, ok := seen[key]; !ok {
		t.Fatalf("seen = %v, want the seeded key kept", seen)
	}
}
