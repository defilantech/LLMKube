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
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
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
	if slice.Labels["endpointslice.kubernetes.io/managed-by"] != "metal-agent.llmkube.ai" {
		t.Errorf("EndpointSlice label endpointslice.kubernetes.io/managed-by = %q, want %q",
			slice.Labels["endpointslice.kubernetes.io/managed-by"], "metal-agent.llmkube.ai")
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
	// The endpoint is the Mac host; there is no Pod for a TargetRef to name.
	if slice.Endpoints[0].TargetRef != nil {
		t.Errorf("EndpointSlice endpoint TargetRef = %+v, want nil", slice.Endpoints[0].TargetRef)
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

	// Pre-create Service and Endpoints, labeled as agent-owned so the guarded
	// delete in UnregisterEndpoint actually removes them.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
			Labels:    map[string]string{managedByLabel: managedByValue},
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

	// Pre-create resources, labeled as agent-owned, so first cleanup does
	// actual deletes; second call should tolerate NotFound and still return nil.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-model",
			Namespace: "default",
			Labels:    map[string]string{managedByLabel: managedByValue},
		},
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-model",
			Namespace: "default",
			Labels:    map[string]string{managedByLabel: managedByValue},
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

// Deleting an InferenceService whose name matches an unrelated Service and
// EndpointSlice must not delete them.
func TestUnregisterEndpoint_KeepsUnownedObjects(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: "db", Namespace: "default"},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	c := newRegistryTestClient(t, svc, slice)
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")

	if err := r.UnregisterEndpoint(context.Background(), "default", "db"); err != nil {
		t.Fatalf("UnregisterEndpoint: %v", err)
	}
	dbKey := types.NamespacedName{Name: "db", Namespace: "default"}
	if err := c.Get(context.Background(), dbKey, &corev1.Service{}); err != nil {
		t.Errorf("unowned Service was deleted: %v", err)
	}
	if err := c.Get(context.Background(), dbKey, &discoveryv1.EndpointSlice{}); err != nil {
		t.Errorf("unowned EndpointSlice was deleted: %v", err)
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

func newRegistryTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// An InferenceService named after a Service the agent did not create must not
// modify that Service in any field, and must report a typed conflict.
func TestRegisterEndpoint_RefusesUnownedService(t *testing.T) {
	foreign := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default",
			Labels: map[string]string{"component": "apiserver"}},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: map[string]string{"app": "real"},
			Ports:    []corev1.ServicePort{{Name: "https", Port: 443, TargetPort: intstr.FromInt(6443)}},
		},
	}
	c := newRegistryTestClient(t, foreign)
	key := types.NamespacedName{Name: "kubernetes", Namespace: "default"}
	before := &corev1.Service{}
	if err := c.Get(context.Background(), key, before); err != nil {
		t.Fatal(err)
	}

	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"}}
	err := r.RegisterEndpoint(context.Background(), isvc, 9000)

	var conflict *EndpointNameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("RegisterEndpoint error = %v, want *EndpointNameConflictError", err)
	}
	if conflict.Kind != "Service" || conflict.Name != "kubernetes" {
		t.Errorf("conflict = %+v, want Kind=Service Name=kubernetes", conflict)
	}
	after := &corev1.Service{}
	if err := c.Get(context.Background(), key, after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("unowned Service was modified:\nbefore %+v\nafter  %+v", before, after)
	}
	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(context.Background(), key, slice); err == nil {
		t.Error("an EndpointSlice was created despite the Service conflict")
	}
}

// Same guard for an existing EndpointSlice the agent does not own, when the
// Service is free (or owned).
func TestRegisterEndpoint_RefusesUnownedEndpointSlice(t *testing.T) {
	foreign := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default",
			Labels: map[string]string{"kubernetes.io/service-name": "shared"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.9.9.9"}}},
	}
	c := newRegistryTestClient(t, foreign)
	before := &discoveryv1.EndpointSlice{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "shared", Namespace: "default"}, before)

	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default"}}
	err := r.RegisterEndpoint(context.Background(), isvc, 9000)

	var conflict *EndpointNameConflictError
	if !errors.As(err, &conflict) || conflict.Kind != "EndpointSlice" {
		t.Fatalf("RegisterEndpoint error = %v, want EndpointSlice conflict", err)
	}
	after := &discoveryv1.EndpointSlice{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "shared", Namespace: "default"}, after)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("unowned EndpointSlice was modified")
	}
	// The Service name was free; a Slice-only conflict must not leak a Service
	// the agent now owns (nothing would ever clean it up: the InferenceService
	// still exists, so ReconcileOrphanEndpoints skips it, and a conflict is
	// not retried).
	if err := c.Get(context.Background(), types.NamespacedName{Name: "shared", Namespace: "default"},
		&corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Errorf("a Service was created despite the EndpointSlice conflict (get err = %v), want NotFound", err)
	}
}

// Objects the agent created keep updating normally.
func TestRegisterEndpoint_UpdatesOwnedObjects(t *testing.T) {
	c := newRegistryTestClient(t)
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	if err := r.RegisterEndpoint(context.Background(), isvc, 9000); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterEndpoint(context.Background(), isvc, 9001); err != nil {
		t.Fatalf("second register of an owned object failed: %v", err)
	}
	svc := &corev1.Service{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "m", Namespace: "default"}, svc)
	if got := svc.Spec.Ports[0].TargetPort.IntValue(); got != 9001 {
		t.Errorf("targetPort = %d, want 9001", got)
	}
}

// TestRegisterEndpoint_RepairsSliceFromOlderAgent checks that registering over
// a slice an older agent wrote drops its targetRef to the <isvc>-metal Pod,
// which never exists, and adds the endpointslice managed-by label (#2407).
func TestRegisterEndpoint_RepairsSliceFromOlderAgent(t *testing.T) {
	old := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "m",
			Namespace: "default",
			Labels:    map[string]string{labelServiceName: "m", managedByLabel: managedByValue},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses: []string{"10.0.0.5"},
			TargetRef: &corev1.ObjectReference{Kind: "Pod", Name: "m-metal"},
		}},
	}
	c := newRegistryTestClient(t, old)
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	if err := r.RegisterEndpoint(context.Background(), isvc, 9000); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "m", Namespace: "default"}, slice); err != nil {
		t.Fatalf("get EndpointSlice: %v", err)
	}
	if got := slice.Labels["endpointslice.kubernetes.io/managed-by"]; got != "metal-agent.llmkube.ai" {
		t.Errorf("slice endpointslice.kubernetes.io/managed-by = %q, want %q", got, "metal-agent.llmkube.ai")
	}
	if len(slice.Endpoints) != 1 || slice.Endpoints[0].TargetRef != nil {
		t.Errorf("slice endpoints = %+v, want one endpoint with no TargetRef", slice.Endpoints)
	}
}

// A conflict is permanent: RegisterEndpointWithRetry must return it at once
// instead of backing off.
func TestRegisterEndpointWithRetry_StopsOnConflict(t *testing.T) {
	foreign := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "taken", Namespace: "default"}}
	c := newRegistryTestClient(t, foreign)
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	r.retryBackoff = wait.Backoff{Duration: time.Second, Factor: 2, Steps: 5}
	start := time.Now()
	err := r.RegisterEndpointWithRetry(context.Background(),
		&inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "taken", Namespace: "default"}}, 9000)
	var conflict *EndpointNameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("retried for %v; a conflict must not be retried", d)
	}
}

// controllerOwnedService returns a Service named name whose controller
// ownerReference points at an InferenceService with UID ownerUID, and which
// carries no managed-by label: the Service the controller creates for a CUDA
// InferenceService.
func controllerOwnedService(name string, ownerUID types.UID) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
			Labels: map[string]string{"app": name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: inferencev1alpha1.GroupVersion.String(), Kind: "InferenceService",
				Name: name, UID: ownerUID, Controller: ptr.To(true),
			}}},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports:    []corev1.ServicePort{{Name: "http", Port: 8080}},
		},
	}
}

// A Service controlled by the same InferenceService (left over from a CUDA
// Model before a switch to a Metal Model) is taken over: registration
// succeeds, stamps the managed-by label and keeps the ownerReference.
func TestRegisterEndpoint_AcceptsControllerOwnedService(t *testing.T) {
	c := newRegistryTestClient(t, controllerOwnedService("migrated", "uid-1"))
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "migrated", Namespace: "default", UID: "uid-1"}}

	if err := r.CheckEndpointName(context.Background(), isvc); err != nil {
		t.Fatalf("CheckEndpointName: %v", err)
	}
	if err := r.RegisterEndpoint(context.Background(), isvc, 9000); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "migrated", Namespace: "default"}, svc); err != nil {
		t.Fatal(err)
	}
	if svc.Labels[managedByLabel] != managedByValue {
		t.Errorf("labels = %v, want %s=%s", svc.Labels, managedByLabel, managedByValue)
	}
	if svc.Spec.Selector != nil {
		t.Errorf("selector = %v, want nil (agent-managed endpoints)", svc.Spec.Selector)
	}
	if ref := metav1.GetControllerOf(svc); ref == nil || ref.UID != "uid-1" {
		t.Errorf("controller ownerReference = %+v, want UID uid-1 kept", ref)
	}
}

// A controller ownerReference to a DIFFERENT InferenceService is still a
// conflict, and the Service is left untouched.
func TestRegisterEndpoint_RefusesServiceControlledByOtherUID(t *testing.T) {
	c := newRegistryTestClient(t, controllerOwnedService("shared", "uid-other"))
	key := types.NamespacedName{Name: "shared", Namespace: "default"}
	before := &corev1.Service{}
	if err := c.Get(context.Background(), key, before); err != nil {
		t.Fatal(err)
	}
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "default", UID: "uid-mine"}}

	var conflict *EndpointNameConflictError
	if err := r.CheckEndpointName(context.Background(), isvc); !errors.As(err, &conflict) {
		t.Fatalf("CheckEndpointName error = %v, want *EndpointNameConflictError", err)
	}
	if err := r.RegisterEndpoint(context.Background(), isvc, 9000); !errors.As(err, &conflict) {
		t.Fatalf("RegisterEndpoint error = %v, want *EndpointNameConflictError", err)
	}
	if conflict.Kind != "Service" {
		t.Errorf("conflict kind = %q, want Service", conflict.Kind)
	}
	after := &corev1.Service{}
	if err := c.Get(context.Background(), key, after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("Service controlled by another InferenceService was modified")
	}
}

// The conflict message tells the user how to resolve it.
func TestEndpointNameConflictError_NamesRemedy(t *testing.T) {
	msg := (&EndpointNameConflictError{Kind: "Service", Namespace: "default", Name: "x"}).Error()
	if !strings.Contains(msg, "rename the InferenceService or remove that object") {
		t.Errorf("Error() = %q, want the remedy", msg)
	}
}

// Deletion stays label-only: a Service controlled by the InferenceService but
// not labelled by the agent is not deleted by UnregisterEndpoint.
func TestUnregisterEndpoint_KeepsControllerOwnedUnlabelledService(t *testing.T) {
	c := newRegistryTestClient(t, controllerOwnedService("migrated", "uid-1"))
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	if err := r.UnregisterEndpoint(context.Background(), "default", "migrated"); err != nil {
		t.Fatalf("UnregisterEndpoint: %v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "migrated", Namespace: "default"},
		&corev1.Service{}); err != nil {
		t.Errorf("controller-owned, unlabelled Service was deleted: %v", err)
	}
}

// --- Relay mode (EnableIngress): the agent registers <isvc>-agent pointing at
// its TLS ingress, annotated with the ingress SPKI pin, and never the <isvc>
// Service.

func relayKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: "default", Name: name + inferencev1alpha1.MetalAgentServiceSuffix}
}

func newRelayRegistry(c client.Client) *ServiceRegistry {
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "v1.2.3")
	if err := r.EnableIngress(9443, testIngressPin); err != nil {
		panic(err)
	}
	return r
}

func TestEnableIngress_RejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		port int
		pin  string
	}{
		{"zero port", 0, testIngressPin},
		{"negative port", -1, testIngressPin},
		{"empty pin", 9443, ""},
		{"not base64", 9443, "PIN!"},
		{"wrong length", 9443, "AAAA"},
		{"url encoding", 9443, strings.Repeat("_", 43) + "="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewServiceRegistry(newRegistryTestClient(t), "10.0.0.5", newNopLogger(), "")
			if err := r.EnableIngress(tc.port, tc.pin); err == nil {
				t.Errorf("EnableIngress(%d, %q) = nil, want error", tc.port, tc.pin)
			}
			if r.relayMode() {
				t.Error("a rejected EnableIngress left the registry in relay mode")
			}
		})
	}
	r := NewServiceRegistry(newRegistryTestClient(t), "10.0.0.5", newNopLogger(), "")
	if err := r.EnableIngress(9443, testIngressPin); err != nil || !r.relayMode() {
		t.Errorf("EnableIngress(valid) = %v, relay=%t; want nil, true", err, r.relayMode())
	}
}

func TestRegisterEndpoint_RelayWritesAgentService(t *testing.T) {
	c := newRegistryTestClient(t)
	r := newRelayRegistry(c)
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m.v1", Namespace: "default"}}
	if err := r.RegisterEndpoint(context.Background(), isvc, 51234); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	ctx := context.Background()
	key := relayKey("m-v1")

	svc := &corev1.Service{}
	if err := c.Get(ctx, key, svc); err != nil {
		t.Fatalf("get %s Service: %v", key.Name, err)
	}
	if len(svc.Spec.Ports) != 1 {
		t.Fatalf("Service ports = %d, want 1", len(svc.Spec.Ports))
	}
	p := svc.Spec.Ports[0]
	if p.Name != "https" || p.Port != inferencev1alpha1.MetalAgentServicePort || p.TargetPort.IntValue() != 9443 {
		t.Errorf("Service port = %s %d -> %s, want https 8443 -> 9443", p.Name, p.Port, p.TargetPort.String())
	}
	if svc.Spec.Selector != nil {
		t.Errorf("Service selector = %v, want nil", svc.Spec.Selector)
	}
	if svc.Labels[managedByLabel] != managedByValue || svc.Labels["llmkube.ai/inference-service"] != "m.v1" {
		t.Errorf("Service labels = %v, want managed-by and inference-service=m.v1", svc.Labels)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, key, slice); err != nil {
		t.Fatalf("get %s EndpointSlice: %v", key.Name, err)
	}
	if got := slice.Labels[labelServiceName]; got != key.Name {
		t.Errorf("slice %s = %q, want %q", labelServiceName, got, key.Name)
	}
	if slice.Labels[managedByLabel] != managedByValue || slice.Labels["llmkube.ai/inference-service"] != "m.v1" {
		t.Errorf("slice labels = %v, want managed-by and inference-service=m.v1", slice.Labels)
	}
	if got := slice.Labels["endpointslice.kubernetes.io/managed-by"]; got != "metal-agent.llmkube.ai" {
		t.Errorf("slice endpointslice.kubernetes.io/managed-by = %q, want %q", got, "metal-agent.llmkube.ai")
	}
	if len(slice.Ports) != 1 || slice.Ports[0].Port == nil || *slice.Ports[0].Port != 9443 ||
		slice.Ports[0].Name == nil || *slice.Ports[0].Name != "https" {
		t.Errorf("slice ports = %+v, want https 9443", slice.Ports)
	}
	if got := slice.Annotations[inferencev1alpha1.AnnotationAgentIngressSPKI]; got != testIngressPin {
		t.Errorf("slice SPKI annotation = %q, want %q", got, testIngressPin)
	}
	if got := slice.Annotations[inferencev1alpha1.AnnotationAgentEnginePort]; got != "51234" {
		t.Errorf("slice engine-port annotation = %q, want %q (the engine port passed to RegisterEndpoint)", got, "51234")
	}
	if slice.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat] == "" {
		t.Error("slice heartbeat annotation missing")
	}
	if len(slice.Endpoints) != 1 || slice.Endpoints[0].Addresses[0] != "10.0.0.5" ||
		slice.Endpoints[0].Conditions.Ready == nil || !*slice.Endpoints[0].Conditions.Ready ||
		slice.Endpoints[0].TargetRef != nil {
		t.Errorf("slice endpoints = %+v, want one Ready 10.0.0.5 with no TargetRef", slice.Endpoints)
	}

	legacy := types.NamespacedName{Namespace: "default", Name: "m-v1"}
	if err := c.Get(ctx, legacy, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Errorf("legacy <isvc> Service get err = %v, want NotFound", err)
	}
	if err := c.Get(ctx, legacy, &discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(err) {
		t.Errorf("legacy <isvc> EndpointSlice get err = %v, want NotFound", err)
	}
}

// A re-register with a new engine port (a respawn onto a fresh loopback
// port) updates the annotation in place: foreman-agent's host-override
// lookup must see the current port, not the one from the process it
// replaced.
func TestRegisterEndpoint_RelayEnginePortAnnotationUpdatesOnReregister(t *testing.T) {
	c := newRegistryTestClient(t)
	r := newRelayRegistry(c)
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}

	if err := r.RegisterEndpoint(ctx, isvc, 51234); err != nil {
		t.Fatalf("first RegisterEndpoint: %v", err)
	}
	if err := r.RegisterEndpoint(ctx, isvc, 60177); err != nil {
		t.Fatalf("second RegisterEndpoint: %v", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, relayKey("m"), slice); err != nil {
		t.Fatalf("get EndpointSlice: %v", err)
	}
	if got := slice.Annotations[inferencev1alpha1.AnnotationAgentEnginePort]; got != "60177" {
		t.Errorf("slice engine-port annotation after re-register = %q, want %q", got, "60177")
	}
}

// Legacy mode (no EnableIngress call) never sets the engine-port annotation:
// the "<isvc>" slice's own Port already carries the engine port, so a
// foreman-agent talking to an older, non-relay metal-agent has no need for
// it and must not be misled by a stale one.
func TestRegisterEndpoint_LegacyModeOmitsEnginePortAnnotation(t *testing.T) {
	c := newRegistryTestClient(t)
	r := NewServiceRegistry(c, "10.0.0.5", newNopLogger(), "")
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	if err := r.RegisterEndpoint(context.Background(), isvc, 51234); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}

	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "m"}, slice); err != nil {
		t.Fatalf("get EndpointSlice: %v", err)
	}
	if _, ok := slice.Annotations[inferencev1alpha1.AnnotationAgentEnginePort]; ok {
		t.Errorf("legacy-mode slice annotations = %v, want no %s annotation",
			slice.Annotations, inferencev1alpha1.AnnotationAgentEnginePort)
	}
}

// Ruling 16 regression: WithdrawEndpointIfPresent and WithdrawEndpoint both
// reconstruct their port argument from the slice's own Ports field (or take
// a caller-supplied one) rather than the live engine port; in relay mode
// that field is the ingress port (e.g. 9443), never the engine's. Writing
// it into AnnotationAgentEnginePort on every withdrawal would clobber the
// last known-good engine port a foreman-agent host-override lookup depends
// on. upsertEndpoint must (re)write that annotation only on a ready
// registration and leave whatever value the slice already carries alone on
// a withdrawal.
func TestWithdraw_PreservesEnginePortAnnotation(t *testing.T) {
	c := newRegistryTestClient(t)
	r := newRelayRegistry(c)
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	key := relayKey("m")

	assertEnginePortAndReady := func(t *testing.T, wantPort string, wantReady bool) {
		t.Helper()
		slice := &discoveryv1.EndpointSlice{}
		if err := c.Get(ctx, key, slice); err != nil {
			t.Fatalf("get EndpointSlice: %v", err)
		}
		if got := slice.Annotations[inferencev1alpha1.AnnotationAgentEnginePort]; got != wantPort {
			t.Errorf("engine-port annotation = %q, want %q", got, wantPort)
		}
		if len(slice.Endpoints) != 1 || slice.Endpoints[0].Conditions.Ready == nil ||
			*slice.Endpoints[0].Conditions.Ready != wantReady {
			t.Errorf("endpoint = %+v, want Ready=%t", slice.Endpoints, wantReady)
		}
	}

	if err := r.RegisterEndpoint(ctx, isvc, 51234); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	assertEnginePortAndReady(t, "51234", true)

	if _, err := r.WithdrawEndpointIfPresent(ctx, isvc); err != nil {
		t.Fatalf("WithdrawEndpointIfPresent: %v", err)
	}
	assertEnginePortAndReady(t, "51234", false)

	// A direct WithdrawEndpoint call, e.g. from a failed-start path that
	// does not know the live engine port either, must not clobber it.
	if err := r.WithdrawEndpoint(ctx, isvc, 9443); err != nil {
		t.Fatalf("WithdrawEndpoint: %v", err)
	}
	assertEnginePortAndReady(t, "51234", false)
}

// A slice that has never carried the annotation (an older agent's slice, or
// one written before a ready registration ever landed) stays without it
// after a withdrawal: upsertEndpoint must not introduce the annotation on a
// ready=false write.
func TestWithdrawEndpointIfPresent_NoAnnotationStaysAbsent(t *testing.T) {
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "m-agent",
			Namespace: "default",
			Labels: map[string]string{
				labelServiceName:               "m-agent",
				managedByLabel:                 managedByValue,
				"llmkube.ai/inference-service": "m",
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Port: ptr.To(int32(9443))}},
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.5"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
	}
	c := newRegistryTestClient(t, slice)
	r := newRelayRegistry(c)
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}

	ok, err := r.WithdrawEndpointIfPresent(ctx, isvc)
	if err != nil || !ok {
		t.Fatalf("WithdrawEndpointIfPresent = (%t, %v), want (true, nil)", ok, err)
	}

	after := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, relayKey("m"), after); err != nil {
		t.Fatalf("get EndpointSlice: %v", err)
	}
	if got, ok := after.Annotations[inferencev1alpha1.AnnotationAgentEnginePort]; ok {
		t.Errorf("engine-port annotation = %q, want absent", got)
	}
	if len(after.Endpoints) != 1 || after.Endpoints[0].Conditions.Ready == nil || *after.Endpoints[0].Conditions.Ready {
		t.Errorf("endpoint = %+v, want Ready=false", after.Endpoints)
	}
}

func TestRegisterEndpoint_RelayDeletesOwnedLegacySliceKeepsService(t *testing.T) {
	legacySvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default",
			Labels: map[string]string{managedByLabel: managedByValue, "llmkube.ai/inference-service": "m"}},
		Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 8080, TargetPort: intstr.FromInt(51234)}}},
	}
	legacySlice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default",
			Labels: map[string]string{managedByLabel: managedByValue, labelServiceName: "m",
				"llmkube.ai/inference-service": "m"}},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	c := newRegistryTestClient(t, legacySvc, legacySlice)
	ctx := context.Background()
	legacy := types.NamespacedName{Namespace: "default", Name: "m"}
	before := &corev1.Service{}
	if err := c.Get(ctx, legacy, before); err != nil {
		t.Fatal(err)
	}

	r := newRelayRegistry(c)
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	if err := r.RegisterEndpoint(ctx, isvc, 51234); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}

	if err := c.Get(ctx, legacy, &discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(err) {
		t.Errorf("agent-owned legacy slice get err = %v, want NotFound (deleted)", err)
	}
	after := &corev1.Service{}
	if err := c.Get(ctx, legacy, after); err != nil {
		t.Fatalf("legacy Service must survive relay registration: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("legacy Service resourceVersion %s -> %s; relay mode must never touch <isvc>",
			before.ResourceVersion, after.ResourceVersion)
	}
	// A heartbeat re-register must not touch it either.
	if err := r.RegisterEndpoint(ctx, isvc, 51234); err != nil {
		t.Fatalf("second RegisterEndpoint: %v", err)
	}
	again := &corev1.Service{}
	_ = c.Get(ctx, legacy, again)
	if again.ResourceVersion != before.ResourceVersion {
		t.Errorf("legacy Service modified on re-register")
	}
}

func TestRegisterEndpoint_RelayKeepsUnownedLegacySlice(t *testing.T) {
	foreign := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default",
			Labels: map[string]string{labelServiceName: "m"}},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	c := newRegistryTestClient(t, foreign)
	ctx := context.Background()
	legacy := types.NamespacedName{Namespace: "default", Name: "m"}
	before := &discoveryv1.EndpointSlice{}
	_ = c.Get(ctx, legacy, before)

	r := newRelayRegistry(c)
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	if err := r.RegisterEndpoint(ctx, isvc, 51234); err != nil {
		t.Fatalf("RegisterEndpoint: %v", err)
	}
	after := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, legacy, after); err != nil {
		t.Fatalf("unowned legacy slice was deleted: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Error("unowned legacy slice was modified")
	}
}

func TestRegisterEndpoint_RelayRefusesUnownedAgentObjects(t *testing.T) {
	for _, tc := range []struct {
		kind string
		obj  client.Object
	}{
		{"Service", &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "m-agent", Namespace: "default"}}},
		{"EndpointSlice", &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: "m-agent", Namespace: "default"},
			AddressType: discoveryv1.AddressTypeIPv4,
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			c := newRegistryTestClient(t, tc.obj)
			r := newRelayRegistry(c)
			isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
			err := r.RegisterEndpoint(context.Background(), isvc, 51234)
			var conflict *EndpointNameConflictError
			if !errors.As(err, &conflict) {
				t.Fatalf("RegisterEndpoint error = %v, want *EndpointNameConflictError", err)
			}
			if conflict.Kind != tc.kind || conflict.Name != "m-agent" {
				t.Errorf("conflict = %+v, want Kind=%s Name=m-agent", conflict, tc.kind)
			}
			if err := r.CheckEndpointName(context.Background(), isvc); !errors.As(err, &conflict) {
				t.Errorf("CheckEndpointName error = %v, want *EndpointNameConflictError", err)
			}
		})
	}
}

func TestUnregisterEndpoint_RelayDeletesAgentObjectsOnly(t *testing.T) {
	owned := map[string]string{managedByLabel: managedByValue, "llmkube.ai/inference-service": "m"}
	objs := []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default", Labels: owned}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default", Labels: owned},
			AddressType: discoveryv1.AddressTypeIPv4},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "m-agent", Namespace: "default", Labels: owned}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "m-agent", Namespace: "default", Labels: owned},
			AddressType: discoveryv1.AddressTypeIPv4},
	}
	c := newRegistryTestClient(t, objs...)
	r := newRelayRegistry(c)
	ctx := context.Background()
	if err := r.UnregisterEndpoint(ctx, "default", "m"); err != nil {
		t.Fatalf("UnregisterEndpoint: %v", err)
	}
	agent := relayKey("m")
	if err := c.Get(ctx, agent, &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Errorf("m-agent Service get err = %v, want NotFound", err)
	}
	if err := c.Get(ctx, agent, &discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(err) {
		t.Errorf("m-agent EndpointSlice get err = %v, want NotFound", err)
	}
	legacy := types.NamespacedName{Namespace: "default", Name: "m"}
	if err := c.Get(ctx, legacy, &corev1.Service{}); err != nil {
		t.Errorf("legacy <isvc> Service must survive relay unregister: %v", err)
	}
	if err := c.Get(ctx, legacy, &discoveryv1.EndpointSlice{}); err != nil {
		t.Errorf("legacy <isvc> EndpointSlice must survive relay unregister: %v", err)
	}
}

func TestWithdrawEndpointIfPresent_RelayUsesAgentSlice(t *testing.T) {
	c := newRegistryTestClient(t)
	r := newRelayRegistry(c)
	ctx := context.Background()
	isvc := &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	if err := r.RegisterEndpoint(ctx, isvc, 51234); err != nil {
		t.Fatal(err)
	}
	ok, err := r.WithdrawEndpointIfPresent(ctx, isvc)
	if err != nil || !ok {
		t.Fatalf("WithdrawEndpointIfPresent = %v, %v; want true, nil", ok, err)
	}
	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(ctx, relayKey("m"), slice); err != nil {
		t.Fatal(err)
	}
	if slice.Endpoints[0].Conditions.Ready == nil || *slice.Endpoints[0].Conditions.Ready {
		t.Error("m-agent slice still Ready after withdrawal")
	}
	if *slice.Ports[0].Port != 9443 {
		t.Errorf("withdrawn slice port = %d, want 9443", *slice.Ports[0].Port)
	}
	legacy := types.NamespacedName{Namespace: "default", Name: "m"}
	if err := c.Get(ctx, legacy, &discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(err) {
		t.Errorf("withdrawal created a legacy <isvc> slice (err = %v)", err)
	}
}

func TestReconcileOrphanEndpoints_RelayRemovesAgentObjects(t *testing.T) {
	owned := map[string]string{managedByLabel: managedByValue, "llmkube.ai/inference-service": "gone"}
	objs := []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gone-agent", Namespace: "default", Labels: owned}},
		&discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Name: "gone-agent", Namespace: "default", Labels: owned},
			AddressType: discoveryv1.AddressTypeIPv4},
	}
	c := newRegistryTestClient(t, objs...)
	r := newRelayRegistry(c)
	ctx := context.Background()
	n, err := r.ReconcileOrphanEndpoints(ctx, "default")
	if err != nil || n != 1 {
		t.Fatalf("ReconcileOrphanEndpoints = %d, %v; want 1, nil", n, err)
	}
	if err := c.Get(ctx, relayKey("gone"), &corev1.Service{}); !apierrors.IsNotFound(err) {
		t.Errorf("orphan gone-agent Service get err = %v, want NotFound", err)
	}
	if err := c.Get(ctx, relayKey("gone"), &discoveryv1.EndpointSlice{}); !apierrors.IsNotFound(err) {
		t.Errorf("orphan gone-agent EndpointSlice get err = %v, want NotFound", err)
	}
}
