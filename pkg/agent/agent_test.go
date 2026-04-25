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
