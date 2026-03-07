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
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
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
	Logger         *zap.SugaredLogger

	// MemoryProvider supplies system memory info. Nil defaults to DarwinMemoryProvider.
	MemoryProvider MemoryProvider
	// MemoryFraction is the fraction of total memory to budget for models (0 = auto-detect).
	MemoryFraction float64

	// WatchdogConfig configures the memory pressure watchdog. Nil disables it.
	WatchdogConfig *MemoryWatchdogConfig
}

// MetalAgent watches Kubernetes InferenceService resources and manages
// native llama-server processes with Metal acceleration
type MetalAgent struct {
	config         MetalAgentConfig
	watcher        *InferenceServiceWatcher
	executor       *MetalExecutor
	registry       *ServiceRegistry
	processes      map[string]*ManagedProcess // namespacedName -> process
	logger         *zap.SugaredLogger
	mu             sync.RWMutex
	memoryProvider MemoryProvider
	memoryFraction float64
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
	logger := config.Logger
	if logger == nil {
		logger = zap.NewNop().Sugar()
	}

	// Resolve memory provider
	provider := config.MemoryProvider
	if provider == nil {
		provider = &DarwinMemoryProvider{}
	}

	// Resolve memory fraction
	fraction := config.MemoryFraction
	if fraction <= 0 {
		total, err := provider.TotalMemory()
		if err != nil {
			logger.Warnw("failed to detect total memory for fraction auto-detection, using 0.67", "error", err)
			fraction = 0.67
		} else {
			fraction = DefaultMemoryFraction(total)
		}
	}

	return &MetalAgent{
		config:         config,
		processes:      make(map[string]*ManagedProcess),
		logger:         logger.With("component", "metal-agent"),
		memoryProvider: provider,
		memoryFraction: fraction,
	}
}

// Start begins watching for InferenceService resources and managing processes
func (a *MetalAgent) Start(ctx context.Context) error {
	// Log effective memory budget and set gauge
	if total, err := a.memoryProvider.TotalMemory(); err == nil {
		budget := uint64(float64(total) * a.memoryFraction)
		a.logger.Infow("memory budget",
			"total", formatMemory(total),
			"fraction", a.memoryFraction,
			"budget", formatMemory(budget),
		)
		memoryBudgetBytes.Set(float64(budget))
	} else {
		a.logger.Warnw("unable to query total memory", "error", err)
	}

	// Initialize components
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, a.config.Namespace, a.logger.With("subsystem", "watcher"))
	a.executor = NewMetalExecutor(
		a.config.LlamaServerBin,
		a.config.ModelStorePath,
		a.logger.With("subsystem", "executor"),
	)
	a.registry = NewServiceRegistry(a.config.K8sClient, a.config.HostIP, a.logger.With("subsystem", "registry"))

	// Start health server
	if a.config.Port > 0 {
		healthSrv := NewHealthServer(a, a.config.Port, a.logger.With("subsystem", "health-server"))
		go func() {
			if err := healthSrv.Run(ctx); err != nil {
				a.logger.Warnw("health server exited with error", "error", err)
			}
		}()
	}

	// Start health monitor
	monitor := NewHealthMonitor(
		a,
		NewDefaultProcessHealthChecker(5*time.Second),
		30*time.Second,
		a.logger.With("subsystem", "health-monitor"),
	)
	go monitor.Run(ctx)

	// Start memory watchdog (if configured)
	if a.config.WatchdogConfig != nil {
		watchdog := NewMemoryWatchdog(
			a.memoryProvider,
			a.processMemInfoSnapshot,
			nil, // observe-only in PR A; eviction callback added in PR B
			*a.config.WatchdogConfig,
			a.logger.With("subsystem", "watchdog"),
		)
		go watchdog.Run(ctx)
	}

	// Start watcher
	eventChan := make(chan InferenceServiceEvent)
	go func() {
		if err := a.watcher.Watch(ctx, eventChan); err != nil {
			a.logger.Warnw("watcher exited with error", "error", err)
		}
	}()

	// Process events
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-eventChan:
			if err := a.handleEvent(ctx, event); err != nil {
				a.logger.Warnw("failed to handle event", "eventType", event.Type, "error", err)
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
		a.logger.Debugw("inference service already has a healthy process", "key", key)
		return nil
	}

	a.logger.Infow("starting inference service", "namespace", isvc.Namespace, "name", isvc.Name)

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

	// Pre-flight memory check
	if estimate, err := a.estimateModelMemory(model, contextSize); err != nil {
		a.logger.Warnw("memory estimation failed, proceeding without check", "error", err)
	} else {
		memoryEstimatedBytes.WithLabelValues(isvc.Name, isvc.Namespace).Set(float64(estimate.TotalBytes))

		resolved, resolveErr := ResolveMemoryBudget(model.Spec.Hardware, a.memoryFraction)
		if resolveErr != nil {
			a.logger.Warnw("memory budget resolution failed, proceeding without check", "error", resolveErr)
		} else {
			a.logger.Infow("resolved memory budget",
				"mode", resolved.Mode, "source", resolved.Source)

			var budget *MemoryBudget
			switch resolved.Mode {
			case BudgetModeAbsolute:
				budget = CheckMemoryBudgetAbsolute(resolved.Bytes, estimate)
				memoryBudgetBytes.Set(float64(resolved.Bytes))
			default: // BudgetModeFraction
				var budgetErr error
				budget, budgetErr = CheckMemoryBudget(a.memoryProvider, estimate, resolved.Fraction)
				if budgetErr != nil {
					a.logger.Warnw("memory budget check failed, proceeding without check", "error", budgetErr)
				}
			}

			if budget != nil && !budget.Fits {
				var msg string
				if resolved.Mode == BudgetModeAbsolute {
					msg = fmt.Sprintf("estimated %s required, budget %s (absolute from CRD)",
						formatMemory(budget.EstimateBytes),
						formatMemory(budget.BudgetBytes),
					)
				} else {
					msg = fmt.Sprintf("estimated %s required, budget %s (%s total * %.0f%%)",
						formatMemory(budget.EstimateBytes),
						formatMemory(budget.BudgetBytes),
						formatMemory(budget.TotalBytes),
						resolved.Fraction*100,
					)
				}
				a.logger.Warnw("model does not fit in memory budget",
					"estimate", formatMemory(budget.EstimateBytes),
					"budget", formatMemory(budget.BudgetBytes),
					"source", resolved.Source,
				)
				isvc.Status.SchedulingStatus = "InsufficientMemory"
				isvc.Status.SchedulingMessage = msg
				if updateErr := a.config.K8sClient.Status().Update(ctx, isvc); updateErr != nil {
					a.logger.Warnw("failed to update InferenceService status", "error", updateErr)
				}
				return fmt.Errorf("insufficient memory: %s", msg)
			} else if budget != nil {
				a.logger.Infow("memory check passed",
					"estimate", formatMemory(budget.EstimateBytes),
					"budget", formatMemory(budget.BudgetBytes),
					"headroom", formatMemory(budget.HeadroomBytes),
					"source", resolved.Source,
				)
			}
		}
	}

	// Start the process
	process, err := a.executor.StartProcess(ctx, ExecutorConfig{
		Name:        isvc.Name,
		Namespace:   isvc.Namespace,
		ModelSource: model.Spec.Source,
		ModelName:   model.Name,
		GPULayers:   gpuLayers,
		ContextSize: contextSize,
		Jinja:       isvc.Spec.Jinja != nil && *isvc.Spec.Jinja,
	})
	if err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}

	// Store process and update metrics
	a.mu.Lock()
	a.processes[key] = process
	managedProcesses.Set(float64(len(a.processes)))
	a.mu.Unlock()
	processHealthy.WithLabelValues(isvc.Name, isvc.Namespace).Set(1)

	// Register service endpoint in Kubernetes
	if err := a.registry.RegisterEndpoint(ctx, isvc, process.Port); err != nil {
		a.logger.Warnw(
			"failed to register endpoint",
			"namespace", isvc.Namespace,
			"name", isvc.Name,
			"port", process.Port,
			"error", err,
		)
	}

	a.logger.Infow(
		"started inference service",
		"namespace", isvc.Namespace,
		"name", isvc.Name,
		"port", process.Port,
		"pid", process.PID,
	)

	return nil
}

// deleteProcess stops a running llama-server process
func (a *MetalAgent) deleteProcess(ctx context.Context, key string) error {
	a.mu.Lock()
	process, exists := a.processes[key]
	if !exists {
		a.mu.Unlock()
		return nil
	}
	delete(a.processes, key)
	managedProcesses.Set(float64(len(a.processes)))
	a.mu.Unlock()

	a.logger.Infow("stopping inference service", "key", key)
	namespace, name := parseKey(key)

	// Clean up per-process metrics
	processHealthy.DeleteLabelValues(name, namespace)
	memoryEstimatedBytes.DeleteLabelValues(name, namespace)
	healthCheckDuration.DeleteLabelValues(name, namespace)
	processRestarts.DeleteLabelValues(name, namespace)

	var deleteErrors []error
	if err := a.executor.StopProcess(process.PID); err != nil {
		deleteErrors = append(deleteErrors, fmt.Errorf("failed to stop process: %w", err))
	}

	// Unregister after the process has stopped. UnregisterEndpoint is idempotent
	// (tolerates 404), so this is safe even if a prior cleanup attempt already
	// removed the resources.
	if err := a.registry.UnregisterEndpoint(ctx, namespace, name); err != nil {
		deleteErrors = append(deleteErrors, fmt.Errorf("failed to unregister endpoint for %s: %w", key, err))
	}

	if len(deleteErrors) > 0 {
		return fmt.Errorf("delete process cleanup errors: %w", errors.Join(deleteErrors...))
	}

	a.logger.Infow("stopped inference service", "key", key)
	return nil
}

// scheduleRestart increments the restart counter and re-runs ensureProcess
// for the named InferenceService. It is called by HealthMonitor when a process
// becomes unhealthy.
func (a *MetalAgent) scheduleRestart(ctx context.Context, name, namespace string) {
	processRestarts.WithLabelValues(name, namespace).Inc()

	isvc := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, isvc); err != nil {
		a.logger.Warnw("failed to fetch InferenceService for restart", "name", name, "namespace", namespace, "error", err)
		return
	}

	if err := a.ensureProcess(ctx, isvc); err != nil {
		a.logger.Warnw("failed to restart process", "name", name, "namespace", namespace, "error", err)
	}
}

// Shutdown gracefully shuts down all running processes
func (a *MetalAgent) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.logger.Infow("cleaning up running processes", "count", len(a.processes))

	var shutdownErrors []error
	for key, process := range a.processes {
		if err := a.executor.StopProcess(process.PID); err != nil {
			shutdownErrors = append(shutdownErrors, fmt.Errorf("failed to stop %s: %w", key, err))
		}
	}

	if len(shutdownErrors) > 0 {
		return fmt.Errorf("shutdown errors: %w", errors.Join(shutdownErrors...))
	}

	return nil
}

// processMemInfoSnapshot returns a snapshot of process names and PIDs for the watchdog.
func (a *MetalAgent) processMemInfoSnapshot() []processMemInfo {
	a.mu.RLock()
	defer a.mu.RUnlock()

	infos := make([]processMemInfo, 0, len(a.processes))
	for _, p := range a.processes {
		infos = append(infos, processMemInfo{
			Name: p.Name,
			PID:  p.PID,
		})
	}
	return infos
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

// estimateModelMemory builds a MemoryEstimate for a model using the file on disk
// (preferred) or the Status.Size string, plus GGUF metadata when available.
func (a *MetalAgent) estimateModelMemory(model *inferencev1alpha1.Model, contextSize int) (MemoryEstimate, error) {
	var fileSizeBytes uint64

	// Try to stat the model file on disk
	filename := filepath.Base(model.Spec.Source)
	localPath := filepath.Join(a.config.ModelStorePath, model.Name, filename)
	if info, err := os.Stat(localPath); err == nil {
		fileSizeBytes = uint64(info.Size())
	} else if model.Status.Size != "" {
		// Fall back to parsing the human-readable size from model status
		parsed, err := parseSize(model.Status.Size)
		if err != nil {
			return MemoryEstimate{}, fmt.Errorf(
				"cannot determine model size: file not found at %s and failed to parse status size %q: %w",
				localPath, model.Status.Size, err,
			)
		}
		fileSizeBytes = parsed
	} else {
		return MemoryEstimate{}, fmt.Errorf(
			"cannot determine model size: file not found at %s and no status size available",
			localPath,
		)
	}

	var layerCount, embeddingSize uint64
	if model.Status.GGUF != nil {
		layerCount = model.Status.GGUF.LayerCount
		embeddingSize = model.Status.GGUF.EmbeddingSize
	}

	return EstimateModelMemory(fileSizeBytes, layerCount, embeddingSize, contextSize), nil
}
