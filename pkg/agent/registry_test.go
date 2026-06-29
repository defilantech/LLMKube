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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	if registry == nil {
		t.Fatal("NewServiceRegistry returned nil")
	}
}

func TestRegisterEndpoint(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

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

	// Verify EndpointSlice was created
	slice := &discoveryv1.EndpointSlice{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, slice)
	if err != nil {
		t.Fatalf("Failed to get created EndpointSlice: %v", err)
	}

	// The service-name label is required for kube-proxy wiring and for
	// consumers to list the slice.
	if slice.Labels["kubernetes.io/service-name"] != "test-model" {
		t.Errorf("EndpointSlice label kubernetes.io/service-name = %q, want %q",
			slice.Labels["kubernetes.io/service-name"], "test-model")
	}
	if slice.Labels["llmkube.ai/managed-by"] != "metal-agent" {
		t.Errorf("EndpointSlice label llmkube.ai/managed-by = %q, want %q",
			slice.Labels["llmkube.ai/managed-by"], "metal-agent")
	}
	if slice.AddressType != discoveryv1.AddressTypeIPv4 {
		t.Errorf("EndpointSlice AddressType = %q, want %q", slice.AddressType, discoveryv1.AddressTypeIPv4)
	}
	if len(slice.Endpoints) != 1 {
		t.Fatalf("EndpointSlice has %d endpoints, want 1", len(slice.Endpoints))
	}
	if len(slice.Endpoints[0].Addresses) != 1 {
		t.Fatalf("EndpointSlice endpoint has %d addresses, want 1", len(slice.Endpoints[0].Addresses))
	}
	if slice.Endpoints[0].Conditions.Ready == nil || !*slice.Endpoints[0].Conditions.Ready {
		t.Errorf("EndpointSlice endpoint Conditions.Ready = %v, want true", slice.Endpoints[0].Conditions.Ready)
	}
	if len(slice.Ports) != 1 {
		t.Fatalf("EndpointSlice has %d ports, want 1", len(slice.Ports))
	}
	if slice.Ports[0].Port == nil || *slice.Ports[0].Port != 8080 {
		t.Errorf("EndpointSlice port = %v, want 8080", slice.Ports[0].Port)
	}
}

// TestWithdrawEndpoint_FreshIsvc verifies that WithdrawEndpoint on an
// InferenceService with no prior registration creates the Service and
// EndpointSlice with the endpoint marked NOT Ready, and still stamps a fresh
// heartbeat annotation (the agent is alive; only the runtime is unhealthy) (#662).
func TestWithdrawEndpoint_FreshIsvc(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	// Pin the clock so the heartbeat annotation value is deterministic.
	frozen := time.Date(2026, 6, 14, 9, 30, 0, 0, time.UTC)
	registry.now = func() time.Time { return frozen }

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "withdraw-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "withdraw-model"},
	}

	if err := registry.WithdrawEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("WithdrawEndpoint returned error: %v", err)
	}

	// The Service must be present (withdrawal keeps it, never deletes it).
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "withdraw-model", Namespace: "default"},
		&corev1.Service{}); err != nil {
		t.Fatalf("WithdrawEndpoint must create/keep the Service: %v", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "withdraw-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("WithdrawEndpoint must create/keep the EndpointSlice: %v", err)
	}

	if len(slice.Endpoints) != 1 {
		t.Fatalf("EndpointSlice has %d endpoints, want 1", len(slice.Endpoints))
	}
	if slice.Endpoints[0].Conditions.Ready == nil || *slice.Endpoints[0].Conditions.Ready {
		t.Errorf("EndpointSlice endpoint Conditions.Ready = %v, want false",
			slice.Endpoints[0].Conditions.Ready)
	}

	raw := slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat]
	if raw == "" {
		t.Fatalf("heartbeat annotation absent on withdrawn endpoint")
	}
	const want = "2026-06-14T09:30:00Z"
	if raw != want {
		t.Fatalf("heartbeat annotation = %q, want %q (fresh heartbeat on withdrawal)", raw, want)
	}
}

// TestWithdrawEndpoint_AfterRegister verifies that WithdrawEndpoint flips an
// existing Ready endpoint to NotReady WITHOUT deleting the Service or
// EndpointSlice (#662).
func TestWithdrawEndpoint_AfterRegister(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "flip-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "flip-model"},
	}

	if err := registry.RegisterEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}

	// Sanity: registration left the endpoint Ready.
	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "flip-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get endpointslice after register: %v", err)
	}
	if slice.Endpoints[0].Conditions.Ready == nil || !*slice.Endpoints[0].Conditions.Ready {
		t.Fatalf("precondition: endpoint should be Ready after RegisterEndpoint")
	}

	if err := registry.WithdrawEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("WithdrawEndpoint: %v", err)
	}

	// Both Service and EndpointSlice must still exist.
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "flip-model", Namespace: "default"},
		&corev1.Service{}); err != nil {
		t.Fatalf("Service must survive withdrawal: %v", err)
	}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "flip-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("EndpointSlice must survive withdrawal: %v", err)
	}
	if slice.Endpoints[0].Conditions.Ready == nil || *slice.Endpoints[0].Conditions.Ready {
		t.Errorf("EndpointSlice endpoint Conditions.Ready = %v, want false after withdrawal",
			slice.Endpoints[0].Conditions.Ready)
	}
}

// TestRegisterEndpoint_AfterWithdraw verifies recovery: a RegisterEndpoint
// following a WithdrawEndpoint flips the endpoint back to Ready (#662).
func TestRegisterEndpoint_AfterWithdraw(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "recover-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "recover-model"},
	}

	if err := registry.WithdrawEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("WithdrawEndpoint: %v", err)
	}
	if err := registry.RegisterEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("RegisterEndpoint (recovery): %v", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "recover-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get endpointslice after recovery: %v", err)
	}
	if slice.Endpoints[0].Conditions.Ready == nil || !*slice.Endpoints[0].Conditions.Ready {
		t.Errorf("EndpointSlice endpoint Conditions.Ready = %v, want true after recovery",
			slice.Endpoints[0].Conditions.Ready)
	}
}

func TestRegisterEndpoint_SanitizedName(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

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
	_ = discoveryv1.AddToScheme(scheme)

	// Pre-create Service and Endpoints
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(svc, slice).
		Build()

	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

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
	_ = discoveryv1.AddToScheme(scheme)

	// Pre-create with sanitized name
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-v1-0",
			Namespace: "default",
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-v1-0",
			Namespace: "default",
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(svc, slice).
		Build()

	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	// Pass the dotted name — UnregisterEndpoint should sanitize it
	err := registry.UnregisterEndpoint(context.Background(), "default", "model.v1.0")
	if err != nil {
		t.Fatalf("UnregisterEndpoint with dotted name returned error: %v", err)
	}
}

func TestUnregisterEndpoint_Idempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	// Pre-create resources so first cleanup does actual deletes; second call should
	// tolerate NotFound and still return nil.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-model",
			Namespace: "default",
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-model",
			Namespace: "default",
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(svc, slice).
		Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	if err := registry.UnregisterEndpoint(context.Background(), "default", "idempotent-model"); err != nil {
		t.Fatalf("first UnregisterEndpoint returned error: %v", err)
	}
	if err := registry.UnregisterEndpoint(context.Background(), "default", "idempotent-model"); err != nil {
		t.Fatalf("second UnregisterEndpoint should be idempotent, got error: %v", err)
	}
}

func TestReconcileOrphanEndpoints(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	// Live InferenceService whose Service+Endpoints should NOT be cleaned up.
	liveISVC := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "live-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "live-model"},
	}
	liveSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "live-model",
			Namespace: "default",
			Labels: map[string]string{
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": "live-model",
			},
		},
	}

	// Orphan: Service exists but no matching InferenceService.
	orphanSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orphan-model",
			Namespace: "default",
			Labels: map[string]string{
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": "orphan-model",
			},
		},
	}
	orphanSlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orphan-model",
			Namespace: "default",
			Labels: map[string]string{
				"kubernetes.io/service-name":   "orphan-model",
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": "orphan-model",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
	}

	// Foreign Service that we don't own — must be ignored entirely.
	foreignSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreign-svc",
			Namespace: "default",
			Labels:    map[string]string{"app": "something-else"},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(liveISVC, liveSvc, orphanSvc, orphanSlice, foreignSvc).
		Build()

	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	cleaned, err := registry.ReconcileOrphanEndpoints(context.Background(), "default")
	if err != nil {
		t.Fatalf("ReconcileOrphanEndpoints returned error: %v", err)
	}
	if cleaned != 1 {
		t.Errorf("cleaned = %d, want 1 (only orphan-model should be cleaned)", cleaned)
	}

	// Live InferenceService's Service must still exist.
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "live-model", Namespace: "default"},
		&corev1.Service{}); err != nil {
		t.Errorf("live Service was wrongly deleted: %v", err)
	}

	// Orphan Service must be gone.
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "orphan-model", Namespace: "default"},
		&corev1.Service{}); err == nil {
		t.Error("orphan Service should have been deleted")
	}

	// Orphan EndpointSlice must also be gone.
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "orphan-model", Namespace: "default"},
		&discoveryv1.EndpointSlice{}); err == nil {
		t.Error("orphan EndpointSlice should have been deleted")
	}

	// Foreign Service (not labeled managed-by us) must be untouched.
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "foreign-svc", Namespace: "default"},
		&corev1.Service{}); err != nil {
		t.Errorf("foreign Service was wrongly deleted: %v", err)
	}
}

func TestReconcileOrphanEndpoints_EmptyCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	cleaned, err := registry.ReconcileOrphanEndpoints(context.Background(), "default")
	if err != nil {
		t.Fatalf("ReconcileOrphanEndpoints on empty cluster returned error: %v", err)
	}
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0 on empty cluster", cleaned)
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
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "100.103.147.52", newNopLogger(), "")

	ip := registry.resolveHostIP()
	if ip != "100.103.147.52" {
		t.Errorf("resolveHostIP() = %q, want %q", ip, "100.103.147.52")
	}
}

func TestResolveHostIP_AutoDetect(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	ip := registry.resolveHostIP()
	if ip == "" {
		t.Error("resolveHostIP() with empty hostIP should fall back to auto-detect, got empty string")
	}
}

func TestRegisterEndpoint_ExplicitHostIP(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "10.0.0.42", newNopLogger(), "")

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

	// Verify the EndpointSlice uses the explicit host IP
	slice := &discoveryv1.EndpointSlice{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "remote-model",
		Namespace: "default",
	}, slice)
	if err != nil {
		t.Fatalf("Failed to get created EndpointSlice: %v", err)
	}

	if len(slice.Endpoints) != 1 || len(slice.Endpoints[0].Addresses) != 1 {
		t.Fatal("Expected exactly 1 endpoint with 1 address")
	}
	if slice.Endpoints[0].Addresses[0] != "10.0.0.42" {
		t.Errorf("EndpointSlice address = %q, want %q", slice.Endpoints[0].Addresses[0], "10.0.0.42")
	}
}

// TestRegisterEndpoint_PortChange verifies that calling RegisterEndpoint
// twice for the same InferenceService with a new port (respawn scenario) updates
// both the Service targetPort and the EndpointSlice port rather than leaving stale values.
func TestRegisterEndpoint_PortChange(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "test-model"},
	}

	if err := registry.RegisterEndpoint(context.Background(), isvc, 50051); err != nil {
		t.Fatalf("first RegisterEndpoint: %v", err)
	}
	// Respawn scenario: same service, new dynamic port.
	if err := registry.RegisterEndpoint(context.Background(), isvc, 50099); err != nil {
		t.Fatalf("second RegisterEndpoint (port change): %v", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "test-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get endpointslice: %v", err)
	}
	if slice.Ports[0].Port == nil || *slice.Ports[0].Port != 50099 {
		t.Fatalf("endpointslice port = %v, want 50099 (stale port left behind)", slice.Ports[0].Port)
	}
	svc := &corev1.Service{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "test-model", Namespace: "default"}, svc); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if got := svc.Spec.Ports[0].TargetPort.IntValue(); got != 50099 {
		t.Fatalf("service targetPort = %d, want 50099", got)
	}
}

func TestRegisterEndpointWithRetry_TransientFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	var calls int
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				calls++
				if calls <= 2 {
					return apierrors.NewServerTimeout(corev1.Resource("services"), "create", 1)
				}
				return c.Create(ctx, obj, opts...)
			},
		}).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")
	registry.retryBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 2, Steps: 5}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "retry-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "retry-model"},
	}
	if err := registry.RegisterEndpointWithRetry(context.Background(), isvc, 50051); err != nil {
		t.Fatalf("expected retries to absorb 2 transient failures, got: %v", err)
	}
	// The interceptor fires on every Create call. RegisterEndpoint calls
	// CreateOrUpdate for Service (attempt 1: fail, attempt 2: fail, attempt 3:
	// success = 3 Create calls across 2 retry iterations + 1 success) plus one
	// Create for the EndpointSlice on the winning attempt, totalling 4 calls.
	if calls != 4 {
		t.Fatalf("expected 4 Create calls (2 service failures + 1 service success + 1 endpointslice), got %d", calls)
	}
}

func TestRegisterEndpointWithRetry_Exhausted(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return apierrors.NewServerTimeout(corev1.Resource("services"), "create", 1)
			},
		}).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")
	registry.retryBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 2, Steps: 3}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "doomed-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "doomed-model"},
	}
	err := registry.RegisterEndpointWithRetry(context.Background(), isvc, 50051)
	if err == nil {
		t.Fatal("expected error after exhausted retries, got nil")
	}
	// The underlying server-timeout cause must survive the fmt.Errorf wrap.
	if !apierrors.IsServerTimeout(errors.Unwrap(err)) {
		t.Fatalf("expected wrapped cause to be a server-timeout API error, got: %v", errors.Unwrap(err))
	}
}

// TestRegisterEndpoint_StampsHeartbeat verifies that RegisterEndpoint stamps the
// llmkube.ai/agent-heartbeat annotation on the EndpointSlice with a
// deterministic RFC3339 timestamp on every call (issue #663). The clock is
// pinned so the assertion is exact rather than "after test start".
func TestRegisterEndpoint_StampsHeartbeat(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	// Pin the clock so the annotation value is deterministic.
	frozen := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return frozen }

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hb-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "hb-model",
		},
	}

	if err := registry.RegisterEndpoint(context.Background(), isvc, 50051); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "hb-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get endpointslice: %v", err)
	}

	raw := slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat]
	if raw == "" {
		t.Fatalf("heartbeat annotation %q absent on endpointslice", inferencev1alpha1.AnnotationAgentHeartbeat)
	}
	const want = "2026-06-12T12:00:00Z"
	if raw != want {
		t.Fatalf("heartbeat annotation = %q, want %q", raw, want)
	}
}

// TestRegisterEndpoint_StampsAgentVersion verifies that RegisterEndpoint stamps
// the llmkube.ai/agent-version annotation on the EndpointSlice when the
// registry is created with a non-empty version string.
func TestRegisterEndpoint_StampsAgentVersion(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "v0.9.0")

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ver-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "ver-model",
		},
	}

	if err := registry.RegisterEndpoint(context.Background(), isvc, 50051); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "ver-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get endpointslice: %v", err)
	}

	got := slice.Annotations[inferencev1alpha1.AnnotationAgentVersion]
	if got != "v0.9.0" {
		t.Fatalf("agent-version annotation = %q, want %q", got, "v0.9.0")
	}
}

// TestRegisterEndpoint_OmitsAgentVersionWhenEmpty verifies that the
// llmkube.ai/agent-version annotation is absent when the registry is
// created without a version string (older-agent compatibility).
func TestRegisterEndpoint_OmitsAgentVersionWhenEmpty(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "") // empty version

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nover-model",
			Namespace: "default",
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "nover-model",
		},
	}

	if err := registry.RegisterEndpoint(context.Background(), isvc, 50051); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "nover-model", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get endpointslice: %v", err)
	}

	if v, ok := slice.Annotations[inferencev1alpha1.AnnotationAgentVersion]; ok {
		t.Fatalf("agent-version annotation should be absent, got %q", v)
	}
}

// TestRegisterEndpointWithRetry_ContextCancelled verifies that when the context
// is already cancelled before the first attempt, the returned error wraps
// context.Canceled rather than rendering as a garbled %!w(<nil>).
func TestRegisterEndpointWithRetry_ContextCancelled(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")
	registry.retryBackoff = wait.Backoff{Duration: time.Millisecond, Factor: 2, Steps: 5}

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "cancelled-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "cancelled-model"},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the first attempt

	err := registry.RegisterEndpointWithRetry(ctx, isvc, 50051)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled in error chain, got: %v", err)
	}
}

// TestRegisterEndpoint_ReapsLegacyEndpoints verifies that a legacy core/v1
// Endpoints object the agent (or a prior version) left behind is deleted on
// registration, so the built-in EndpointSliceMirroring controller stops
// regenerating a stale mirror slice that blackholes traffic (issue #891).
func TestRegisterEndpoint_ReapsLegacyEndpoints(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	// A legacy Endpoints object carrying the agent's own managed-by label,
	// same name as the Service the agent manages.
	legacy := &corev1.Endpoints{ //nolint:staticcheck // SA1019: simulating a legacy artifact under test
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
			Labels: map[string]string{
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": "test-model",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(legacy).
		Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "test-model"},
	}

	if err := registry.RegisterEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("RegisterEndpoint returned error: %v", err)
	}

	// The legacy Endpoints object must be gone.
	err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, &corev1.Endpoints{}) //nolint:staticcheck // SA1019: asserting the legacy artifact is deleted
	if !apierrors.IsNotFound(err) {
		t.Errorf("legacy Endpoints should have been deleted; Get returned err=%v", err)
	}

	// The live EndpointSlice must still be present.
	slice := &discoveryv1.EndpointSlice{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, slice); err != nil {
		t.Fatalf("EndpointSlice should be present after registration: %v", err)
	}
}

// TestRegisterEndpoint_LeavesUnrelatedEndpoints verifies the reaper only
// deletes the agent's own legacy artifact: an Endpoints object that does not
// carry the agent's managed-by label (e.g. a user's own selector-less Service
// endpoints that happens to share the name) must be left untouched.
func TestRegisterEndpoint_LeavesUnrelatedEndpoints(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	// An Endpoints object NOT managed by the agent.
	unrelated := &corev1.Endpoints{ //nolint:staticcheck // SA1019: simulating a user-owned object under test
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "someone-else",
			},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(unrelated).
		Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "test-model"},
	}

	if err := registry.RegisterEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("RegisterEndpoint returned error: %v", err)
	}

	// The unrelated Endpoints object must still exist.
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, &corev1.Endpoints{}); err != nil { //nolint:staticcheck // SA1019: asserting the user object survived
		t.Errorf("unrelated Endpoints should NOT have been deleted; Get returned err=%v", err)
	}
}

// TestRegisterEndpoint_NoLegacyEndpoints verifies that registration with no
// pre-existing legacy Endpoints object succeeds: a NotFound from the reaper's
// lookup must be swallowed and never fail registration.
func TestRegisterEndpoint_NoLegacyEndpoints(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger(), "")

	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "test-model", Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "test-model"},
	}

	if err := registry.RegisterEndpoint(context.Background(), isvc, 8080); err != nil {
		t.Fatalf("RegisterEndpoint with no legacy Endpoints returned error: %v", err)
	}

	// The EndpointSlice must still be created.
	if err := k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, &discoveryv1.EndpointSlice{}); err != nil {
		t.Fatalf("EndpointSlice should be present after registration: %v", err)
	}
}
