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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func TestSanitizeServiceName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple-name", "simple-name"},
		{"name.with.dots", "name-with-dots"},
		{"no-dots-here", "no-dots-here"},
		{"a.b.c.d", "a-b-c-d"},
		{"", ""},
		{"llama-3.2-3b", "llama-3-2-3b"},
		{"model.v1.0.0", "model-v1-0-0"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := sanitizeServiceName(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeServiceName(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestNewServiceRegistry(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

	if registry == nil {
		t.Fatal("NewServiceRegistry returned nil")
	}
}

func TestRegisterEndpoint(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "test-model",
		},
	}

	err := registry.RegisterEndpoint(context.Background(), isvc, 8080)
	if err != nil {
		t.Fatalf("RegisterEndpoint returned error: %v", err)
	}

	// Verify Service was created
	svc := &corev1.Service{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, svc)
	if err != nil {
		t.Fatalf("Failed to get created Service: %v", err)
	}

	// Verify labels
	if svc.Labels["llmkube.ai/managed-by"] != "metal-agent" {
		t.Errorf("Service label llmkube.ai/managed-by = %q, want %q",
			svc.Labels["llmkube.ai/managed-by"], "metal-agent")
	}
	if svc.Labels["llmkube.ai/inference-service"] != "test-model" {
		t.Errorf("Service label llmkube.ai/inference-service = %q, want %q",
			svc.Labels["llmkube.ai/inference-service"], "test-model")
	}

	// Verify annotations
	if svc.Annotations["llmkube.ai/metal-accelerated"] != "true" {
		t.Errorf("Service annotation llmkube.ai/metal-accelerated = %q, want %q",
			svc.Annotations["llmkube.ai/metal-accelerated"], "true")
	}

	// Verify port configuration
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Service has %d ports, want 1", len(svc.Spec.Ports))
	}
	if svc.Spec.Ports[0].Port != 8080 {
		t.Errorf("Service port = %d, want 8080", svc.Spec.Ports[0].Port)
	}
	if svc.Spec.Ports[0].Name != "http" {
		t.Errorf("Service port name = %q, want %q", svc.Spec.Ports[0].Name, "http")
	}
	if svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Errorf("Service type = %q, want ClusterIP", svc.Spec.Type)
	}

	// Verify Endpoints was created
	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	endpoints := &corev1.Endpoints{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, endpoints)
	if err != nil {
		t.Fatalf("Failed to get created Endpoints: %v", err)
	}

	if len(endpoints.Subsets) != 1 {
		t.Fatalf("Endpoints has %d subsets, want 1", len(endpoints.Subsets))
	}
	if len(endpoints.Subsets[0].Addresses) != 1 {
		t.Fatalf("Endpoints has %d addresses, want 1", len(endpoints.Subsets[0].Addresses))
	}
	if len(endpoints.Subsets[0].Ports) != 1 {
		t.Fatalf("Endpoints has %d ports, want 1", len(endpoints.Subsets[0].Ports))
	}
	if endpoints.Subsets[0].Ports[0].Port != 8080 {
		t.Errorf("Endpoint port = %d, want 8080", endpoints.Subsets[0].Ports[0].Port)
	}
}

func TestRegisterEndpoint_SanitizedName(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "llama-3.2-3b",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "llama-3.2-3b",
		},
	}

	err := registry.RegisterEndpoint(context.Background(), isvc, 8081)
	if err != nil {
		t.Fatalf("RegisterEndpoint returned error: %v", err)
	}

	// Service name should have dots replaced with dashes
	svc := &corev1.Service{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "llama-3-2-3b",
		Namespace: "default",
	}, svc)
	if err != nil {
		t.Fatalf("Failed to get Service with sanitized name: %v", err)
	}
}

func TestUnregisterEndpoint(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Pre-create Service and Endpoints
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
	}
	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(svc, endpoints).
		Build()

	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

	err := registry.UnregisterEndpoint(context.Background(), "default", "test-model")
	if err != nil {
		t.Fatalf("UnregisterEndpoint returned error: %v", err)
	}

	// Verify Service was deleted
	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, &corev1.Service{})
	if err == nil {
		t.Error("Service should have been deleted")
	}
}

func TestUnregisterEndpoint_SanitizedName(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Pre-create with sanitized name
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-v1-0",
			Namespace: "default",
		},
	}
	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-v1-0",
			Namespace: "default",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(svc, endpoints).
		Build()

	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

	// Pass the dotted name — UnregisterEndpoint should sanitize it
	err := registry.UnregisterEndpoint(context.Background(), "default", "model.v1.0")
	if err != nil {
		t.Fatalf("UnregisterEndpoint with dotted name returned error: %v", err)
	}
}

func TestGetHostIP(t *testing.T) {
	// getHostIP should return a non-empty string regardless of environment
	ip := getHostIP()
	if ip == "" {
		t.Error("getHostIP returned empty string")
	}
}

func TestResolveHostIP_Explicit(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "100.103.147.52", newNopLogger())

	ip := registry.resolveHostIP()
	if ip != "100.103.147.52" {
		t.Errorf("resolveHostIP() = %q, want %q", ip, "100.103.147.52")
	}
}

func TestResolveHostIP_AutoDetect(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

	ip := registry.resolveHostIP()
	if ip == "" {
		t.Error("resolveHostIP() with empty hostIP should fall back to auto-detect, got empty string")
	}
}

func TestRegisterEndpoint_ExplicitHostIP(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "10.0.0.42", newNopLogger())

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "remote-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "remote-model",
		},
	}

	err := registry.RegisterEndpoint(context.Background(), isvc, 8082)
	if err != nil {
		t.Fatalf("RegisterEndpoint returned error: %v", err)
	}

	// Verify the Endpoint uses the explicit host IP
	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	endpoints := &corev1.Endpoints{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "remote-model",
		Namespace: "default",
	}, endpoints)
	if err != nil {
		t.Fatalf("Failed to get created Endpoints: %v", err)
	}

	if len(endpoints.Subsets) != 1 || len(endpoints.Subsets[0].Addresses) != 1 {
		t.Fatal("Expected exactly 1 subset with 1 address")
	}
	if endpoints.Subsets[0].Addresses[0].IP != "10.0.0.42" {
		t.Errorf("Endpoint IP = %q, want %q", endpoints.Subsets[0].Addresses[0].IP, "10.0.0.42")
	}
}
