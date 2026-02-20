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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestNewInferenceServiceWatcher(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "test-ns")

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
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "default")

	// Currently shouldWatch returns true for all services (as per source code)
	tests := []struct {
		name string
		isvc *inferencev1alpha1.InferenceService
		want bool
	}{
		{
			name: "basic inference service",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-model",
					Namespace: "default",
				},
			},
			want: true,
		},
		{
			name: "service in different namespace",
			isvc: &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "other-model",
					Namespace: "production",
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := watcher.shouldWatch(tt.isvc)
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

	watcher := NewInferenceServiceWatcher(k8sClient, "default")
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
	existingServices := []inferencev1alpha1.InferenceService{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "model-a",
				Namespace: "default",
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "model-a",
				Replicas: &replicas,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "model-b",
				Namespace: "default",
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "model-b",
				Replicas: &replicas,
			},
		},
	}

	objs := make([]runtime.Object, len(existingServices))
	for i := range existingServices {
		objs[i] = &existingServices[i]
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "default")
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
	services := []inferencev1alpha1.InferenceService{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "model-default",
				Namespace: "default",
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "model-default",
				Replicas: &replicas,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "model-prod",
				Namespace: "production",
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "model-prod",
				Replicas: &replicas,
			},
		},
	}

	objs := make([]runtime.Object, len(services))
	for i := range services {
		objs[i] = &services[i]
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	// Watch only "default" namespace
	watcher := NewInferenceServiceWatcher(k8sClient, "default")
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
		WithRuntimeObjects(isvc).
		Build()

	watcher := NewInferenceServiceWatcher(k8sClient, "default")
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

	watcher := NewInferenceServiceWatcher(k8sClient, "default")
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

func TestWatch_ContextCancellation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	watcher := NewInferenceServiceWatcher(k8sClient, "default")
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
