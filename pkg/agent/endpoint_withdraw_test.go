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
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// These tests cover #1918: an InferenceService this agent owns but cannot
// serve must not keep advertising a Ready endpoint left behind by an earlier
// successful start (same process or a previous agent process).

const inheritedPort = 50051

// inheritedEndpoint returns the Service + EndpointSlice a previous, successful
// start would have left behind: managed by the metal-agent and Ready.
func inheritedEndpoint(name string) (*corev1.Service, *discoveryv1.EndpointSlice) {
	labels := map[string]string{
		"app":                          name,
		"llmkube.ai/managed-by":        "metal-agent",
		"llmkube.ai/inference-service": name,
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{{Name: "http", Port: 8080}},
		},
	}
	sliceLabels := map[string]string{labelServiceName: name}
	for k, v := range labels {
		sliceLabels[k] = v
	}
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    sliceLabels,
			Annotations: map[string]string{
				inferencev1alpha1.AnnotationAgentHeartbeat: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{{
			Addresses:  []string{"10.0.0.1"},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		}},
		Ports: []discoveryv1.EndpointPort{{
			Name:     ptr.To("http"),
			Port:     ptr.To(int32(inheritedPort)),
			Protocol: ptr.To(corev1.ProtocolTCP),
		}},
	}
	return svc, slice
}

// endpointReady fetches the named EndpointSlice and reports whether its single
// endpoint advertises Ready. A missing slice fails the test.
func endpointReady(t *testing.T, c client.Client, name string) (bool, *discoveryv1.EndpointSlice) {
	t.Helper()
	slice := &discoveryv1.EndpointSlice{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, slice); err != nil {
		t.Fatalf("get endpointslice %s: %v", name, err)
	}
	if len(slice.Endpoints) != 1 {
		t.Fatalf("endpointslice %s has %d endpoints, want 1", name, len(slice.Endpoints))
	}
	ready := slice.Endpoints[0].Conditions.Ready
	return ready == nil || *ready, slice
}

// switchableExecutor fails StartProcess while err is set and otherwise
// returns a healthy process on port 9099.
type switchableExecutor struct {
	err error
}

func (e *switchableExecutor) StartProcess(_ context.Context, cfg ExecutorConfig) (*ManagedProcess, error) {
	if e.err != nil {
		return nil, e.err
	}
	return &ManagedProcess{
		Name:      cfg.Name,
		Namespace: cfg.Namespace,
		PID:       4242,
		Port:      9099,
		Healthy:   true,
		StartedAt: time.Now(),
	}, nil
}

func (e *switchableExecutor) StopProcess(_ int) error { return nil }

// newWithdrawTestAgent builds an agent with no allowed model roots beyond
// roots. Tests whose Model uses a local absolute source (real or fabricated)
// must pass its directory in roots, or checkModelPaths refuses it before
// ensureProcess ever reaches the behavior under test.
func newWithdrawTestAgent(t *testing.T, roots []string, objs ...client.Object) (*MetalAgent, client.Client) {
	t.Helper()
	scheme := newTestScheme()
	_ = corev1.AddToScheme(scheme)
	_ = discoveryv1.AddToScheme(scheme)
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&inferencev1alpha1.InferenceService{}).
		WithObjects(objs...).
		Build()
	agent := NewMetalAgent(MetalAgentConfig{
		K8sClient:         k8sClient,
		Namespace:         "default",
		MemoryProvider:    &mockMemoryProvider{totalBytes: 128 << 30, availableBytes: 120 << 30},
		AllowedModelRoots: roots,
	})
	agent.registry = NewServiceRegistry(k8sClient, "10.0.0.1", newNopLogger(), "")
	return agent, k8sClient
}

func ggufModel(t *testing.T, name string) *inferencev1alpha1.Model {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".gguf")
	if err := os.WriteFile(path, []byte("stub-weights"), 0o644); err != nil {
		t.Fatalf("write stub model: %v", err)
	}
	return &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   path,
			Format:   "gguf",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
}

// TestEnsureProcess_FormatIncompatibleWithdrawsInheritedEndpoint is the live
// #1918 case: an mlx-format model resolved to llama-server can never start,
// and the slice from the previous start must stop advertising Ready.
func TestEnsureProcess_FormatIncompatibleWithdrawsInheritedEndpoint(t *testing.T) {
	const name = "mlx-on-llama"
	dir := t.TempDir()
	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: inferencev1alpha1.ModelSpec{
			Source:   filepath.Join(dir, name),
			Format:   "mlx",
			Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: "metal"},
		},
	}
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name},
	}
	svc, slice := inheritedEndpoint(name)
	agent, c := newWithdrawTestAgent(t, []string{dir}, model, isvc, svc, slice)
	agent.executors[runtimeLlamaServer] = &switchableExecutor{}

	err := agent.handleEvent(context.Background(), InferenceServiceEvent{Type: EventTypeCreated, InferenceService: isvc})
	if err == nil {
		t.Fatal("handleEvent should fail for an mlx model on llama-server")
	}

	ready, got := endpointReady(t, c, name)
	if ready {
		t.Errorf("endpoint still Ready after a format-incompatible start; kube-proxy keeps routing to a closed port")
	}
	if p := got.Ports[0].Port; p == nil || *p != inheritedPort {
		t.Errorf("withdrawal changed the slice port to %v, want %d", p, inheritedPort)
	}
}

// TestEnsureProcess_StartFailureWithdrawsThenRecovers covers a spawn error
// from the executor, then a successful start flipping the same slice back.
// The successful start is the failed start's retry (#2404): a watch event for
// the same spec is left to it, like a pending health restart.
func TestEnsureProcess_StartFailureWithdrawsThenRecovers(t *testing.T) {
	const name = "spawn-fails"
	model := ggufModel(t, name)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name},
	}
	svc, slice := inheritedEndpoint(name)
	agent, c := newWithdrawTestAgent(t, []string{filepath.Dir(model.Spec.Source)}, model, isvc, svc, slice)
	clk := &fakeClock{t: time.Now()}
	agent.now = clk.now
	exec := &switchableExecutor{err: errors.New("llama-server exited during startup")}
	agent.executors[runtimeLlamaServer] = exec

	if err := agent.ensureProcess(context.Background(), isvc); err == nil {
		t.Fatal("ensureProcess should surface the start failure")
	}
	if ready, _ := endpointReady(t, c, name); ready {
		t.Fatalf("endpoint still Ready after StartProcess failed")
	}

	exec.err = nil
	clk.t = clk.t.Add(healthRestartInitialBackoff)
	agent.runDueRestarts(context.Background())
	if p := agent.processes["default/"+name]; p == nil {
		t.Fatal("the retry after the backoff did not start the engine")
	}
	ready, got := endpointReady(t, c, name)
	if !ready {
		t.Errorf("endpoint not Ready after a successful start")
	}
	if p := got.Ports[0].Port; p == nil || *p != 9099 {
		t.Errorf("slice port = %v after successful start, want 9099", p)
	}
}

// TestEnsureProcess_StartFailureCreatesNoEndpoint guards the withdrawal from
// materializing a Service+EndpointSlice for a service that never started.
func TestEnsureProcess_StartFailureCreatesNoEndpoint(t *testing.T) {
	const name = "never-started"
	model := ggufModel(t, name)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name},
	}
	agent, c := newWithdrawTestAgent(t, []string{filepath.Dir(model.Spec.Source)}, model, isvc)
	agent.executors[runtimeLlamaServer] = &switchableExecutor{err: errors.New("boom")}

	if err := agent.ensureProcess(context.Background(), isvc); err == nil {
		t.Fatal("ensureProcess should surface the start failure")
	}
	key := types.NamespacedName{Namespace: "default", Name: name}
	err := c.Get(context.Background(), key, &discoveryv1.EndpointSlice{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("start failure created an EndpointSlice (get err = %v), want NotFound", err)
	}
	err = c.Get(context.Background(), key, &corev1.Service{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("start failure created a Service (get err = %v), want NotFound", err)
	}
}

// TestWithdrawInheritedEndpoints_RespectsAllowlist covers the agent restart
// case: slices a previous process left Ready are withdrawn for owned services
// only, never for a sibling Mac's allowlisted services (#524).
func TestWithdrawInheritedEndpoints_RespectsAllowlist(t *testing.T) {
	const owned, foreign = "owned-isvc", "foreign-isvc"
	ownedModel := ggufModel(t, owned)
	foreignModel := ggufModel(t, foreign)
	ownedISVC := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: owned, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: owned},
	}
	foreignISVC := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: foreign, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: foreign},
	}
	ownedSvc, ownedSlice := inheritedEndpoint(owned)
	foreignSvc, foreignSlice := inheritedEndpoint(foreign)
	agent, c := newWithdrawTestAgent(t, nil,
		ownedModel, foreignModel, ownedISVC, foreignISVC,
		ownedSvc, ownedSlice, foreignSvc, foreignSlice)
	agent.watcher = NewInferenceServiceWatcher(c, "default", newNopLogger())
	agent.watcher.SetNameAllowlist([]string{owned})

	agent.withdrawInheritedEndpoints(context.Background())

	if ready, _ := endpointReady(t, c, owned); ready {
		t.Errorf("owned endpoint inherited from the previous agent process is still Ready")
	}
	if ready, _ := endpointReady(t, c, foreign); !ready {
		t.Errorf("startup withdrawal touched a slice outside this agent's allowlist")
	}
}

// TestWithdrawInheritedEndpoints_ModelLookupErrorLeavesEndpoint pins the
// startup cleanup side of #2076: when the Model lookup fails with anything but
// NotFound, ownership is unknown, so the pass leaves that service's slice as it
// found it, the same "keep the previous state" the poll loop applies.
func TestWithdrawInheritedEndpoints_ModelLookupErrorLeavesEndpoint(t *testing.T) {
	const name = "model-lookup-fails"
	model := ggufModel(t, name)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: name},
	}
	svc, slice := inheritedEndpoint(name)
	agent, c := newWithdrawTestAgent(t, nil, model, isvc, svc, slice)
	failing := interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Get: func(
			ctx context.Context, w client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
		) error {
			if _, isModel := obj.(*inferencev1alpha1.Model); isModel {
				return apierrors.NewServiceUnavailable("apiserver restarting")
			}
			return w.Get(ctx, key, obj, opts...)
		},
	})
	agent.watcher = NewInferenceServiceWatcher(failing, "default", newNopLogger())

	agent.withdrawInheritedEndpoints(context.Background())

	if ready, _ := endpointReady(t, c, name); !ready {
		t.Errorf("startup withdrawal acted on a service whose Model lookup failed; want its slice left Ready")
	}
}

// TestWithdrawEndpointIfPresent_SkipsRecentWithdrawalAndForeignSlices pins the
// write-loop guard (an already-withdrawn slice with a fresh heartbeat is not
// rewritten on every failed retry) and the managed-by ownership check.
func TestWithdrawEndpointIfPresent_SkipsRecentWithdrawalAndForeignSlices(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	isvcFor := func(name string) *inferencev1alpha1.InferenceService {
		return &inferencev1alpha1.InferenceService{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}}
	}

	_, recent := inheritedEndpoint("recent")
	recent.Endpoints[0].Conditions.Ready = ptr.To(false)
	recent.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat] = now.Add(-5 * time.Second).Format(time.RFC3339)

	_, aged := inheritedEndpoint("aged")
	aged.Endpoints[0].Conditions.Ready = ptr.To(false)
	aged.Annotations[inferencev1alpha1.AnnotationAgentHeartbeat] = now.Add(-2 * time.Minute).Format(time.RFC3339)

	_, foreign := inheritedEndpoint("foreign")
	foreign.Labels["llmkube.ai/managed-by"] = "someone-else"

	_, c := newWithdrawTestAgent(t, nil, recent, aged, foreign)
	reg := NewServiceRegistry(c, "10.0.0.1", newNopLogger(), "")
	reg.now = func() time.Time { return now }

	cases := []struct {
		name      string
		wantWrite bool
	}{
		{"recent", false},
		{"aged", true},
		{"foreign", false},
	}
	for _, tc := range cases {
		wrote, err := reg.WithdrawEndpointIfPresent(context.Background(), isvcFor(tc.name))
		if err != nil {
			t.Fatalf("%s: WithdrawEndpointIfPresent: %v", tc.name, err)
		}
		if wrote != tc.wantWrite {
			t.Errorf("%s: wrote = %v, want %v", tc.name, wrote, tc.wantWrite)
		}
	}
	if ready, _ := endpointReady(t, c, "foreign"); !ready {
		t.Errorf("a slice not managed by the metal-agent was withdrawn")
	}
}
