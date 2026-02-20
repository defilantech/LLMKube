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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = inferencev1alpha1.AddToScheme(s)
	return s
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
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models")

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
	agent.watcher = NewInferenceServiceWatcher(k8sClient, "default")
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models")
	agent.registry = NewServiceRegistry(k8sClient)

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
	agent.executor = NewMetalExecutor("/fake/llama-server", "/tmp/models")

	err := agent.Shutdown(context.Background())
	if err != nil {
		t.Errorf("Shutdown with no processes returned error: %v", err)
	}
}
