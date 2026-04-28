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

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(s)
	return s
}

func newNopLogger() *zap.SugaredLogger {
	return zap.NewNop().Sugar()
}

func TestNewMetalAgent(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := MetalAgentConfig{
		K8sClient:      k8sClient,
		Namespace:      "test-ns",
		ModelStorePath: "/tmp/test-models",
		LlamaServerBin: "/usr/local/bin/llama-server",
		Port:           9090,
	}

	agent := NewMetalAgent(config)

	if agent == nil {
		t.Fatal("NewMetalAgent returned nil")
	}
	if agent.config.Namespace != "test-ns" {
		t.Errorf("Namespace = %q, want %q", agent.config.Namespace, "test-ns")
	}
	if agent.config.ModelStorePath != "/tmp/test-models" {
		t.Errorf("ModelStorePath = %q, want %q", agent.config.ModelStorePath, "/tmp/test-models")
	}
	if agent.config.LlamaServerBin != "/usr/local/bin/llama-server" {
		t.Errorf("LlamaServerBin = %q, want %q", agent.config.LlamaServerBin, "/usr/local/bin/llama-server")
	}
	if agent.config.Port != 9090 {
		t.Errorf("Port = %d, want %d", agent.config.Port, 9090)
	}
	if agent.processes == nil {
		t.Error("processes map is nil, want initialized map")
	}
	if len(agent.processes) != 0 {
		t.Errorf("processes map has %d entries, want 0", len(agent.processes))
	}
}

func TestHealthCheck_Empty(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})
	health := agent.HealthCheck()

	if len(health) != 0 {
		t.Errorf("HealthCheck returned %d entries, want 0 for new agent", len(health))
	}
}

func TestHealthCheck_WithProcesses(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})
	agent.processes["default/model-a"] = &ManagedProcess{
		Name:    "model-a",
		Healthy: true,
	}
	agent.processes["default/model-b"] = &ManagedProcess{
		Name:    "model-b",
		Healthy: false,
	}

	health := agent.HealthCheck()

	if len(health) != 2 {
		t.Fatalf("HealthCheck returned %d entries, want 2", len(health))
	}
	if !health["default/model-a"] {
		t.Error("model-a should be healthy")
	}
	if health["default/model-b"] {
		t.Error("model-b should be unhealthy")
	}
}

func TestHandleEvent_DeleteNonExistent(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())

	event := InferenceServiceEvent{
		Type: EventTypeDeleted,
		InferenceService: &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nonexistent",
				Namespace: "default",
			},
		},
	}

	err := agent.handleEvent(context.Background(), event)
	if err != nil {
		t.Errorf("handleEvent(delete non-existent) returned error: %v", err)
	}
}

func TestHandleEvent_CreateMissingModel(t *testing.T) {
	scheme := newTestScheme()
	// No Model objects in the fake client
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := NewMetalAgent(MetalAgentConfig{
		K8sClient:      k8sClient,
		Namespace:      "default",
		ModelStorePath: "/tmp/models",
		LlamaServerBin: "/fake/llama-server",
	})
	agent.watcher = NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())
	agent.registry = NewServiceRegistry(k8sClient, "", newNopLogger())

	event := InferenceServiceEvent{
		Type: EventTypeCreated,
		InferenceService: &inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-isvc",
				Namespace: "default",
			},
			Spec: inferencev1alpha1.InferenceServiceSpec{
				ModelRef: "missing-model",
			},
		},
	}

	err := agent.handleEvent(context.Background(), event)
	if err == nil {
		t.Error("handleEvent(create with missing model) should return error")
	}
}

func TestShutdown_NoProcesses(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())

	err := agent.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown with no processes returned error: %v", err)
	}
}

func TestReportHealthServerExit(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})

	t.Run("nil err is no-op", func(t *testing.T) {
		ch := make(chan error, 1)
		agent.reportHealthServerExit(context.Background(), nil, ch)
		select {
		case got := <-ch:
			t.Errorf("unexpected fatal signal: %v", got)
		default:
		}
	})

	t.Run("ctx cancelled is no-op", func(t *testing.T) {
		ch := make(chan error, 1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		agent.reportHealthServerExit(ctx, errors.New("listener died"), ch)
		select {
		case got := <-ch:
			t.Errorf("unexpected fatal signal during shutdown: %v", got)
		default:
		}
	})

	t.Run("real error pushes fatal", func(t *testing.T) {
		ch := make(chan error, 1)
		runErr := errors.New("listener died")
		agent.reportHealthServerExit(context.Background(), runErr, ch)
		select {
		case got := <-ch:
			if !errors.Is(got, runErr) {
				t.Errorf("fatal err = %v, want wrap of %v", got, runErr)
			}
		case <-time.After(100 * time.Millisecond):
			t.Error("expected fatal signal but channel was empty")
		}
	})

	t.Run("non-blocking when channel is full", func(t *testing.T) {
		// fatalErrChan in production is buffered for 2; if both slots are
		// already taken (e.g., watcher fatal already in flight) we must not
		// block forever. The select{default} branch is what guarantees this.
		ch := make(chan error, 1)
		ch <- errors.New("prior fatal")
		done := make(chan struct{})
		go func() {
			agent.reportHealthServerExit(context.Background(), errors.New("listener died"), ch)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			t.Error("reportHealthServerExit blocked when channel was full")
		}
	})
}

func TestMaybeStartApplePowerSampler_DisabledByDefault(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	a := NewMetalAgent(MetalAgentConfig{
		K8sClient: k8sClient,
		Logger:    newNopLogger(),
		// ApplePowerEnabled defaults false
	})

	got := a.maybeStartApplePowerSampler(context.Background())
	if got != nil {
		t.Errorf("expected nil sampler when ApplePowerEnabled=false, got %#v", got)
	}
}

func TestMaybeStartApplePowerSampler_Enabled_LaunchesViaFactory(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	// Swap the factory for a stub that records its construction arguments and
	// returns a sampler whose Run() signals it executed. The whole point of
	// the helper is the wiring between MetalAgentConfig and NewApplePowerSampler;
	// we exercise that wiring deterministically without forking powermetrics.
	origFactory := applePowerSamplerFactory
	t.Cleanup(func() { applePowerSamplerFactory = origFactory })

	type call struct {
		bin      string
		interval time.Duration
	}
	var got call
	ranCh := make(chan struct{}, 1)
	stub := &fakeApplePowerRunner{onRun: func(context.Context) {
		select {
		case ranCh <- struct{}{}:
		default:
		}
	}}
	applePowerSamplerFactory = func(bin string, interval time.Duration, _ *zap.SugaredLogger) applePowerRunner {
		got = call{bin: bin, interval: interval}
		return stub
	}

	a := NewMetalAgent(MetalAgentConfig{
		K8sClient:          k8sClient,
		Logger:             newNopLogger(),
		ApplePowerEnabled:  true,
		ApplePowerInterval: 2 * time.Second,
		PowermetricsBin:    "/usr/bin/powermetrics",
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := a.maybeStartApplePowerSampler(ctx)

	if returned != stub {
		t.Errorf("expected helper to return the constructed sampler, got %#v", returned)
	}
	if got.bin != "/usr/bin/powermetrics" {
		t.Errorf("factory bin = %q, want %q", got.bin, "/usr/bin/powermetrics")
	}
	if got.interval != 2*time.Second {
		t.Errorf("factory interval = %v, want %v", got.interval, 2*time.Second)
	}

	// Also verify Run was actually invoked by the goroutine — without this
	// check the helper could silently regress to building the sampler but
	// never starting it, and the missing power data would only surface in
	// production.
	select {
	case <-ranCh:
	case <-time.After(time.Second):
		t.Error("sampler.Run was never invoked")
	}
}

// fakeApplePowerRunner is a deterministic stand-in for ApplePowerSampler in
// tests. The real sampler shells out to sudo and would fail or hang in CI; the
// fake just records that Run was called.
type fakeApplePowerRunner struct {
	onRun func(context.Context)
}

func (f *fakeApplePowerRunner) Run(ctx context.Context) {
	if f.onRun != nil {
		f.onRun(ctx)
	}
}

func TestRunWatcherLoop_StalledPushesFatal(t *testing.T) {
	// Build a watcher whose Watch will return ErrWatchStalled on the first
	// poll cycle. The scriptedListClient pattern from watcher_test.go is
	// reused: succeed on listExisting, fail every poll, threshold=1.
	c, _ := scriptedListClient(t, func(n int32) bool { return n > 1 })
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})
	agent.watcher = NewInferenceServiceWatcher(c, "default", newNopLogger())
	agent.watcher.SetPollInterval(10 * time.Millisecond)
	agent.watcher.SetMaxConsecutiveFailures(1)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	eventChan := make(chan InferenceServiceEvent, 1)
	fatalErrChan := make(chan error, 2)

	done := make(chan struct{})
	go func() {
		agent.runWatcherLoop(ctx, eventChan, fatalErrChan)
		close(done)
	}()

	select {
	case got := <-fatalErrChan:
		if !errors.Is(got, ErrWatchStalled) {
			t.Errorf("fatalErrChan got %v, want ErrWatchStalled", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runWatcherLoop did not push to fatalErrChan within 2s")
	}

	// runWatcherLoop must also return after pushing — otherwise it would
	// keep retrying ErrWatchStalled and double-push (or burn CPU).
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Error("runWatcherLoop did not return after pushing fatal signal")
	}
}

func TestRunWatcherLoop_ContextCancellationExitsCleanly(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})
	agent.watcher = NewInferenceServiceWatcher(k8sClient, "default", newNopLogger())
	agent.watcher.SetPollInterval(10 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())

	eventChan := make(chan InferenceServiceEvent, 1)
	fatalErrChan := make(chan error, 2)

	done := make(chan struct{})
	go func() {
		agent.runWatcherLoop(ctx, eventChan, fatalErrChan)
		close(done)
	}()

	// Give the loop a moment to start, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runWatcherLoop did not exit on ctx cancellation")
	}

	// No fatal signal should have been pushed during clean shutdown.
	select {
	case got := <-fatalErrChan:
		t.Errorf("unexpected fatal signal during clean shutdown: %v", got)
	default:
	}
}

func TestDeleteProcess_StopFailureStillUnregistersEndpoint(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

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

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient})
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())
	agent.registry = NewServiceRegistry(k8sClient, "", newNopLogger())
	agent.processes["default/test-model"] = &ManagedProcess{
		Name:      "test-model",
		Namespace: "default",
		PID:       -99999, // invalid PID forces StopProcess error
	}

	err := agent.deleteProcess(context.Background(), "default/test-model")
	if err == nil {
		t.Fatal("deleteProcess should return error when StopProcess fails")
	}

	if _, exists := agent.processes["default/test-model"]; exists {
		t.Fatal("process entry should be removed from map")
	}

	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, &corev1.Service{})
	if err == nil {
		t.Fatal("service should be deleted even when StopProcess fails")
	}

	//nolint:staticcheck // SA1019: Endpoints API is still functional and matches production code under test
	err = k8sClient.Get(context.Background(), types.NamespacedName{
		Name:      "test-model",
		Namespace: "default",
	}, &corev1.Endpoints{})
	if err == nil {
		t.Fatal("endpoints should be deleted even when StopProcess fails")
	}
}

func TestComputeSpecHash_StableForSameSpec(t *testing.T) {
	ctx := int32(65536)
	isvc := &inferencev1alpha1.InferenceService{
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:    "test-model",
			ContextSize: &ctx,
		},
	}
	h1 := computeSpecHash(isvc)
	h2 := computeSpecHash(isvc)
	if h1 != h2 {
		t.Errorf("hash should be stable across calls with same input: %s vs %s", h1, h2)
	}
	if h1 == "" {
		t.Error("hash should not be empty for valid spec")
	}
}

func TestComputeSpecHash_ChangesWithContextSize(t *testing.T) {
	ctx1 := int32(65536)
	ctx2 := int32(131072)
	a := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "m", ContextSize: &ctx1}}
	b := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "m", ContextSize: &ctx2}}
	if computeSpecHash(a) == computeSpecHash(b) {
		t.Error("hash should differ when contextSize changes")
	}
}

func TestComputeSpecHash_ChangesWithCacheTypeCustom(t *testing.T) {
	a := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "m"}}
	b := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "m", CacheTypeCustomK: "turbo3"}}
	if computeSpecHash(a) == computeSpecHash(b) {
		t.Error("hash should differ when cacheTypeCustomK is set; this is the field added in #351 and the runtime arg builder uses it")
	}
}

func TestComputeSpecHash_ChangesWithExtraArgs(t *testing.T) {
	a := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "m"}}
	b := &inferencev1alpha1.InferenceService{Spec: inferencev1alpha1.InferenceServiceSpec{ModelRef: "m", ExtraArgs: []string{"--cache-type-k", "turbo3"}}}
	if computeSpecHash(a) == computeSpecHash(b) {
		t.Error("hash should differ when extraArgs is set")
	}
}

func TestComputeSpecHash_NilIsvc(t *testing.T) {
	if computeSpecHash(nil) != "" {
		t.Error("nil isvc should produce empty hash, not panic")
	}
}

func TestEnsureProcess_ReplicasZeroStopsExistingProcess(t *testing.T) {
	scheme := newTestScheme()
	_ = corev1.AddToScheme(scheme)

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "scale-down-isvc", Namespace: "default"}}
	//nolint:staticcheck // SA1019: Endpoints API matches production code under test
	endpoints := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "scale-down-isvc", Namespace: "default"}}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(svc, endpoints).
		Build()

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient, Namespace: "default"})
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())
	agent.registry = NewServiceRegistry(k8sClient, "", newNopLogger())
	agent.processes["default/scale-down-isvc"] = &ManagedProcess{
		Name:      "scale-down-isvc",
		Namespace: "default",
		PID:       -99999,
		Healthy:   true,
		SpecHash:  "old-hash",
	}

	zeroReplicas := int32(0)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-down-isvc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "any-model",
			Replicas: &zeroReplicas,
		},
	}

	// ensureProcess with replicas=0 + existing process must call deleteProcess.
	// We expect an error because StopProcess(-99999) fails on the fake PID,
	// but the process map entry should still be removed (deleteProcess
	// removes-then-stops, mirroring TestDeleteProcess_StopFailureStillUnregistersEndpoint).
	_ = agent.ensureProcess(context.Background(), isvc)

	if _, exists := agent.processes["default/scale-down-isvc"]; exists {
		t.Error("process entry should be removed when replicas=0")
	}
}

func TestEnsureProcess_ReplicasZeroNoOpWhenNoProcess(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient, Namespace: "default"})
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())
	agent.registry = NewServiceRegistry(k8sClient, "", newNopLogger())

	zeroReplicas := int32(0)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "never-existed", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: "any-model",
			Replicas: &zeroReplicas,
		},
	}

	if err := agent.ensureProcess(context.Background(), isvc); err != nil {
		t.Errorf("replicas=0 with no existing process should be a no-op (no error), got: %v", err)
	}
}

func TestEnsureProcess_HealthyAndSpecMatchesIsNoOp(t *testing.T) {
	scheme := newTestScheme()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient, Namespace: "default"})
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())
	agent.registry = NewServiceRegistry(k8sClient, "", newNopLogger())

	ctx := int32(65536)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "noop-isvc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:    "any-model",
			ContextSize: &ctx,
		},
	}

	// Pre-seed a healthy process whose SpecHash matches the incoming spec.
	// ensureProcess should fast-path return nil without consulting K8s for
	// the Model — proving the no-op happens before the model lookup that
	// would otherwise fail (no Model in the fake client).
	agent.processes["default/noop-isvc"] = &ManagedProcess{
		Name:      "noop-isvc",
		Namespace: "default",
		Healthy:   true,
		SpecHash:  computeSpecHash(isvc),
	}

	if err := agent.ensureProcess(context.Background(), isvc); err != nil {
		t.Errorf("healthy + matching specHash should no-op without error, got: %v", err)
	}
	if _, exists := agent.processes["default/noop-isvc"]; !exists {
		t.Error("process entry should remain when no-op fast path triggers")
	}
}

func TestEnsureProcess_SpecDriftCallsDeleteBeforeRespawn(t *testing.T) {
	scheme := newTestScheme()
	_ = corev1.AddToScheme(scheme)

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "drift-isvc", Namespace: "default"}}
	//nolint:staticcheck // SA1019: Endpoints API matches production code under test
	endpoints := &corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "drift-isvc", Namespace: "default"}}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(svc, endpoints).
		Build()

	agent := NewMetalAgent(MetalAgentConfig{K8sClient: k8sClient, Namespace: "default"})
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models", newNopLogger())
	agent.registry = NewServiceRegistry(k8sClient, "", newNopLogger())

	// Existing process recorded with a stale spec hash.
	agent.processes["default/drift-isvc"] = &ManagedProcess{
		Name:      "drift-isvc",
		Namespace: "default",
		PID:       -99999, // forces StopProcess error so respawn path won't run further
		Healthy:   true,
		SpecHash:  "stale-hash-from-old-spec",
	}

	ctx := int32(131072)
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{Name: "drift-isvc", Namespace: "default"},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef:    "any-model",
			ContextSize: &ctx,
		},
	}

	// We expect an error because deleteProcess returns one (StopProcess of
	// fake PID fails). What matters is the process map entry is gone — proves
	// the drift-detection branch ran the delete before the failed respawn.
	_ = agent.ensureProcess(context.Background(), isvc)

	if _, exists := agent.processes["default/drift-isvc"]; exists {
		t.Error("process entry should be removed during spec-drift respawn flow")
	}
}
