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

func TestUnregisterEndpoint_Idempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	// Pre-create resources so first cleanup does actual deletes; second call should
	// tolerate NotFound and still return nil.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-model",
			Namespace: "default",
		},
	}
	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	endpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idempotent-model",
			Namespace: "default",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(svc, endpoints).
		Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

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
	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	orphanEndpoints := &corev1.Endpoints{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orphan-model",
			Namespace: "default",
			Labels: map[string]string{
				"llmkube.ai/managed-by":        "metal-agent",
				"llmkube.ai/inference-service": "orphan-model",
			},
		},
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
		WithRuntimeObjects(liveISVC, liveSvc, orphanSvc, orphanEndpoints, foreignSvc).
		Build()

	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

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

	// Orphan Endpoints must also be gone.
	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "orphan-model", Namespace: "default"},
		&corev1.Endpoints{}); err == nil {
		t.Error("orphan Endpoints should have been deleted")
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

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

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

// TestRegisterEndpoint_PortChange verifies that calling RegisterEndpoint
// twice for the same InferenceService with a new port (respawn scenario) updates
// both the Service targetPort and the Endpoints port rather than leaving stale values.
func TestRegisterEndpoint_PortChange(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())

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

	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	eps := &corev1.Endpoints{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: "test-model", Namespace: "default"}, eps); err != nil {
		t.Fatalf("get endpoints: %v", err)
	}
	if got := eps.Subsets[0].Ports[0].Port; got != 50099 {
		t.Fatalf("endpoints port = %d, want 50099 (stale port left behind)", got)
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
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())
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
	// Create for Endpoints on the winning attempt, totalling 4 calls.
	if calls != 4 {
		t.Fatalf("expected 4 Create calls (2 service failures + 1 service success + 1 endpoints), got %d", calls)
	}
}

func TestRegisterEndpointWithRetry_Exhausted(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				return apierrors.NewServerTimeout(corev1.Resource("services"), "create", 1)
			},
		}).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())
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

// TestRegisterEndpointWithRetry_ContextCancelled verifies that when the context
// is already cancelled before the first attempt, the returned error wraps
// context.Canceled rather than rendering as a garbled %!w(<nil>).
func TestRegisterEndpointWithRetry_ContextCancelled(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	registry := NewServiceRegistry(k8sClient, "", newNopLogger())
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
