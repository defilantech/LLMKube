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
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// MetalAgentConfig contains configuration for the Metal agent
type MetalAgentConfig struct {
	K8sClient      client.Client
	Namespace      string
	ModelStorePath string
	LlamaServerBin string
	Port           int
	HostIP         string // explicit IP to register in K8s endpoints; empty = auto-detect
}

// MetalAgent watches Kubernetes InferenceService resources and manages
// native llama-server processes with Metal acceleration
type MetalAgent struct {
	config    MetalAgentConfig
	watcher   *InferenceServiceWatcher
	executor  *MetalExecutor
	registry  *ServiceRegistry
	processes map[string]*ManagedProcess // namespacedName -> process
	mu        sync.RWMutex
}

// ManagedProcess represents a running llama-server process
type ManagedProcess struct {
	Name      string
	Namespace string
	PID       int
	Port      int
	ModelPath string
	StartedAt time.Time
	Healthy   bool
}

// NewMetalAgent creates a new Metal agent instance
func NewMetalAgent(config MetalAgentConfig) *MetalAgent {
	return &MetalAgent{
		config:    config,
		processes: make(map[string]*ManagedProcess),
	}
}

// Start begins watching for InferenceService resources and managing processes
func (a *MetalAgent) Start(ctx context.Context) error {
	// Initialize components
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, a.config.Namespace)
	a.executor = NewMetalExecutor(a.config.LlamaServerBin, a.config.ModelStorePath)
	a.registry = NewServiceRegistry(a.config.K8sClient, a.config.HostIP)

	// Start watcher
	eventChan := make(chan InferenceServiceEvent)
	go func() {
		if err := a.watcher.Watch(ctx, eventChan); err != nil {
			fmt.Printf("⚠️  Watcher error: %v\n", err)
		}
	}()

	// Process events
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-eventChan:
			if err := a.handleEvent(ctx, event); err != nil {
				fmt.Printf("⚠️  Error handling event: %v\n", err)
			}
		}
	}
}

// handleEvent processes InferenceService create/update/delete events
func (a *MetalAgent) handleEvent(ctx context.Context, event InferenceServiceEvent) error {
	key := types.NamespacedName{
		Namespace: event.InferenceService.Namespace,
		Name:      event.InferenceService.Name,
	}.String()

	switch event.Type {
	case EventTypeCreated, EventTypeUpdated:
		return a.ensureProcess(ctx, event.InferenceService)
	case EventTypeDeleted:
		return a.deleteProcess(ctx, key)
	}

	return nil
}

// ensureProcess ensures a llama-server process is running for the InferenceService
func (a *MetalAgent) ensureProcess(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	key := types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      isvc.Name,
	}.String()

	// Check if process already exists
	a.mu.RLock()
	existing, exists := a.processes[key]
	a.mu.RUnlock()

	if exists && existing.Healthy {
		// Process already running and healthy
		return nil
	}

	fmt.Printf("🚀 Starting inference service: %s/%s\n", isvc.Namespace, isvc.Name)

	// Get the Model resource
	model := &inferencev1alpha1.Model{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      isvc.Spec.ModelRef,
	}, model); err != nil {
		return fmt.Errorf("failed to get model %s: %w", isvc.Spec.ModelRef, err)
	}

	// Get GPU layers if specified
	gpuLayers := int32(0) // Default: auto-detect (executor will use 99)
	if model.Spec.Hardware.GPU != nil {
		gpuLayers = model.Spec.Hardware.GPU.Layers
	}

	// Get context size from InferenceService spec, default to 2048
	contextSize := 2048
	if isvc.Spec.ContextSize != nil && *isvc.Spec.ContextSize > 0 {
		contextSize = int(*isvc.Spec.ContextSize)
	}

	// Start the process
	process, err := a.executor.StartProcess(ctx, ExecutorConfig{
		Name:        isvc.Name,
		Namespace:   isvc.Namespace,
		ModelSource: model.Spec.Source,
		ModelName:   model.Name,
		GPULayers:   gpuLayers,
		ContextSize: contextSize,
	})
	if err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}

	// Store process
	a.mu.Lock()
	a.processes[key] = process
	a.mu.Unlock()

	// Register service endpoint in Kubernetes
	if err := a.registry.RegisterEndpoint(ctx, isvc, process.Port); err != nil {
		fmt.Printf("⚠️  Warning: Failed to register endpoint: %v\n", err)
	}

	fmt.Printf("✅ Started inference service %s/%s on port %d (PID: %d)\n",
		isvc.Namespace, isvc.Name, process.Port, process.PID)

	return nil
}

// deleteProcess stops a running llama-server process
func (a *MetalAgent) deleteProcess(_ context.Context, key string) error {
	a.mu.Lock()
	process, exists := a.processes[key]
	if !exists {
		a.mu.Unlock()
		return nil
	}
	delete(a.processes, key)
	a.mu.Unlock()

	fmt.Printf("🛑 Stopping inference service: %s\n", key)

	if err := a.executor.StopProcess(process.PID); err != nil {
		return fmt.Errorf("failed to stop process: %w", err)
	}

	fmt.Printf("✅ Stopped inference service: %s\n", key)
	return nil
}

// Shutdown gracefully shuts down all running processes
func (a *MetalAgent) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	fmt.Printf("🧹 Cleaning up %d running processes...\n", len(a.processes))

	var errors []error
	for key, process := range a.processes {
		if err := a.executor.StopProcess(process.PID); err != nil {
			errors = append(errors, fmt.Errorf("failed to stop %s: %w", key, err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("shutdown errors: %v", errors)
	}

	return nil
}

// HealthCheck returns the health status of all managed processes
func (a *MetalAgent) HealthCheck() map[string]bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	health := make(map[string]bool)
	for key, process := range a.processes {
		health[key] = process.Healthy
	}
	return health
}
