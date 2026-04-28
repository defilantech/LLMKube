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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Inference runtime identifiers used by MetalAgentConfig.Runtime.
const (
	runtimeOMLX   = "omlx"
	runtimeOllama = "ollama"
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

	// Runtime selects the inference backend: "llama-server" (default), "omlx", or "ollama".
	Runtime string
	// OMLXBin is the path to the omlx binary. Only used when Runtime is "omlx".
	OMLXBin string
	// OMLXPort is the port the shared oMLX daemon listens on (default 8000).
	OMLXPort int
	// OllamaPort is the port the Ollama daemon listens on (default 11434).
	OllamaPort int

	// MemoryProvider supplies system memory info. Nil defaults to DarwinMemoryProvider.
	MemoryProvider MemoryProvider
	// MemoryFraction is the fraction of total memory to budget for models (0 = auto-detect).
	MemoryFraction float64

	// WatchdogConfig configures the memory pressure watchdog. Nil disables it.
	WatchdogConfig *MemoryWatchdogConfig

	// MaxWatchFailures is the consecutive-failure threshold at which the
	// InferenceService watcher gives up on its current Kubernetes connection
	// and signals a fatal exit. Zero means use the watcher's built-in default
	// (DefaultMaxConsecutiveFailures).
	MaxWatchFailures int

	// LlamaServerStartupTimeout is how long the Metal executor waits for a
	// freshly-spawned llama-server to respond on /health before giving up.
	// Zero means use the executor default (DefaultLlamaServerStartupTimeout).
	// Bump this when serving very large models — mlock + warmup grow with
	// model size and the default may be too aggressive for 80+ GB models.
	LlamaServerStartupTimeout time.Duration

	// OMLXStartupTimeout is how long the agent waits for the oMLX daemon to
	// become healthy after launching it. Zero means use the executor default
	// (DefaultOMLXStartupTimeout). The original 30s constant was too short
	// for real M-series hardware.
	OMLXStartupTimeout time.Duration

	// ApplePowerEnabled launches the powermetrics-driven sampler that
	// publishes apple_power_*_watts gauges. Defaults false because
	// powermetrics requires sudo, which the agent reaches via a NOPASSWD
	// sudoers entry the operator must install explicitly. The gauges feed
	// InferCost's Apple Silicon per-token cost attribution. Darwin only.
	ApplePowerEnabled bool

	// ApplePowerInterval is the powermetrics sampling cadence. Zero means
	// use DefaultApplePowerInterval (1s). Only meaningful when
	// ApplePowerEnabled is true.
	ApplePowerInterval time.Duration

	// PowermetricsBin is the path to the macOS powermetrics binary. Empty
	// means use DefaultPowermetricsBin (/usr/bin/powermetrics). Only used
	// when ApplePowerEnabled is true.
	PowermetricsBin string
}

// MetalAgent watches Kubernetes InferenceService resources and manages
// native inference processes with Metal acceleration
type MetalAgent struct {
	config         MetalAgentConfig
	watcher        *InferenceServiceWatcher
	executor       ProcessExecutor
	registry       *ServiceRegistry
	processes      map[string]*ManagedProcess // namespacedName -> process
	logger         *zap.SugaredLogger
	mu             sync.RWMutex
	memoryProvider MemoryProvider
	memoryFraction float64
}

// ManagedProcess represents a running inference process (llama-server, oMLX, or Ollama model).
type ManagedProcess struct {
	Name      string
	Namespace string
	PID       int
	Port      int
	ModelPath string
	ModelID   string // oMLX/Ollama model identifier used for unload; empty for llama-server
	StartedAt time.Time
	Healthy   bool

	// SpecHash captures the hash of InferenceServiceSpec fields that, if
	// changed, require respawning the underlying process. Used by ensureProcess
	// to detect spec drift on UPDATED events and respawn instead of no-oping.
	SpecHash string
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

	// fatalErrChan carries terminal failures from background subsystems
	// (watcher, health server) up to the main select loop, so the agent can
	// return cleanly and let the supervisor restart the process.
	fatalErrChan := make(chan error, 2)

	// Initialize components
	a.watcher = NewInferenceServiceWatcher(a.config.K8sClient, a.config.Namespace, a.logger.With("subsystem", "watcher"))
	if a.config.MaxWatchFailures > 0 {
		a.watcher.SetMaxConsecutiveFailures(a.config.MaxWatchFailures)
	}

	switch a.config.Runtime {
	case runtimeOMLX:
		port := a.config.OMLXPort
		if port == 0 {
			port = 8000
		}
		omlxExec := NewOMLXExecutor(
			a.config.OMLXBin,
			a.config.ModelStorePath,
			port,
			a.logger.With("subsystem", "executor"),
		)
		if a.config.OMLXStartupTimeout > 0 {
			omlxExec.SetStartupTimeout(a.config.OMLXStartupTimeout)
		}
		a.executor = omlxExec
	case runtimeOllama:
		port := a.config.OllamaPort
		if port == 0 {
			port = 11434
		}
		a.executor = NewOllamaExecutor(
			port,
			a.logger.With("subsystem", "executor"),
		)
	default:
		metalExec := NewMetalExecutor(
			a.config.LlamaServerBin,
			a.config.ModelStorePath,
			a.logger.With("subsystem", "executor"),
		)
		if a.config.LlamaServerStartupTimeout > 0 {
			metalExec.SetStartupTimeout(a.config.LlamaServerStartupTimeout)
		}
		a.executor = metalExec
	}

	a.registry = NewServiceRegistry(a.config.K8sClient, a.config.HostIP, a.logger.With("subsystem", "registry"))

	// Reconcile orphaned Service+Endpoints from prior agent sessions. The
	// watcher's `seen` map starts fresh each Watch() call, so InferenceServices
	// deleted while the agent was down don't trigger the cleanup path. This
	// pass closes that gap by treating the agent-managed-by label as the
	// authoritative inventory and cross-checking each Service against the API.
	if cleaned, err := a.registry.ReconcileOrphanEndpoints(ctx, a.config.Namespace); err != nil {
		a.logger.Warnw("orphan endpoint reconciliation failed", "error", err)
	} else if cleaned > 0 {
		a.logger.Infow("cleaned up orphaned endpoints from prior sessions", "count", cleaned)
	}

	// Start health server. An unexpected exit here (port binding lost,
	// listener crashed) is fatal — the management plane is how operators
	// observe and recover the agent, so running blind is worse than
	// restarting clean.
	if a.config.Port > 0 {
		healthSrv := NewHealthServer(a, a.config.Port, a.logger.With("subsystem", "health-server"))
		go func() {
			a.reportHealthServerExit(ctx, healthSrv.Run(ctx), fatalErrChan)
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

	// Start Apple Silicon power sampler (if enabled). The sampler shells out
	// to powermetrics under sudo and publishes the apple_power_*_watts gauges
	// for InferCost to scrape. Disabled by default because it requires a
	// NOPASSWD sudoers entry the operator must install explicitly.
	a.maybeStartApplePowerSampler(ctx)

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

	// Start watcher with retry logic. If the CRDs are not installed when the
	// agent starts, Watch will fail immediately. The retry loop with
	// exponential backoff makes the agent recover once the CRDs land.
	// ErrWatchStalled bypasses the retry path — see runWatcherLoop for why.
	eventChan := make(chan InferenceServiceEvent)
	go a.runWatcherLoop(ctx, eventChan, fatalErrChan)

	// Process events
	for {
		select {
		case <-ctx.Done():
			return nil
		case fatalErr := <-fatalErrChan:
			a.logger.Errorw("agent received fatal signal, exiting for supervisor restart",
				"error", fatalErr)
			return fatalErr
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

// executorBaseConfig holds the values ensureProcess derives from sources
// outside isvc.Spec (memory check, defaults, perf-core detection). Passing
// these alongside the isvc into buildExecutorConfig lets the helper own all
// the spec → flag mapping in one place.
type executorBaseConfig struct {
	GPULayers      int32
	ContextSize    int
	FlashAttention bool
	BatchSize      int
	UBatchSize     int
}

// buildExecutorConfig collects every flag-relevant InferenceService field into
// an ExecutorConfig that buildLlamaServerArgs can consume. Pointer fields are
// dereferenced here so the executor sees plain values; cache types are resolved
// (custom > standard) to mirror the controller's runtime_llamacpp arg builder.
func buildExecutorConfig(
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	base executorBaseConfig,
) ExecutorConfig {
	cacheTypeK := isvc.Spec.CacheTypeK
	if isvc.Spec.CacheTypeCustomK != "" {
		cacheTypeK = isvc.Spec.CacheTypeCustomK
	}
	cacheTypeV := isvc.Spec.CacheTypeV
	if isvc.Spec.CacheTypeCustomV != "" {
		cacheTypeV = isvc.Spec.CacheTypeCustomV
	}

	return ExecutorConfig{
		Name:                   isvc.Name,
		Namespace:              isvc.Namespace,
		ModelSource:            model.Spec.Source,
		ModelName:              model.Name,
		GPULayers:              base.GPULayers,
		ContextSize:            base.ContextSize,
		Jinja:                  derefBool(isvc.Spec.Jinja),
		FlashAttention:         base.FlashAttention,
		Mlock:                  true,
		BatchSize:              base.BatchSize,
		UBatchSize:             base.UBatchSize,
		ParallelSlots:          derefInt32(isvc.Spec.ParallelSlots),
		CacheTypeK:             cacheTypeK,
		CacheTypeV:             cacheTypeV,
		MoeCPUOffload:          derefBool(isvc.Spec.MoeCPUOffload),
		MoeCPULayers:           derefInt32(isvc.Spec.MoeCPULayers),
		NoKvOffload:            derefBool(isvc.Spec.NoKvOffload),
		TensorOverrides:        isvc.Spec.TensorOverrides,
		MetadataOverrides:      isvc.Spec.MetadataOverrides,
		NoWarmup:               derefBool(isvc.Spec.NoWarmup),
		ReasoningBudget:        derefInt32(isvc.Spec.ReasoningBudget),
		ReasoningBudgetMessage: isvc.Spec.ReasoningBudgetMessage,
		ExtraArgs:              isvc.Spec.ExtraArgs,
	}
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefInt32(p *int32) int {
	if p == nil {
		return 0
	}
	return int(*p)
}

// validateRuntimeFormat returns an error if the model's format is incompatible
// with the agent's configured runtime. Empty format defaults to "gguf".
func (a *MetalAgent) validateRuntimeFormat(model *inferencev1alpha1.Model) error {
	modelFormat := model.Spec.Format
	if modelFormat == "" {
		modelFormat = "gguf"
	}

	var bad bool
	var runtimeLabel string
	switch a.config.Runtime {
	case runtimeOMLX:
		bad = modelFormat == "gguf"
		runtimeLabel = runtimeOMLX
	case runtimeOllama:
		bad = modelFormat == "mlx"
		runtimeLabel = runtimeOllama
	default:
		bad = modelFormat == "mlx"
		runtimeLabel = "llama-server"
	}
	if !bad {
		return nil
	}

	a.logger.Warnw("skipping incompatible model format for runtime",
		"model", model.Name, "format", modelFormat, "runtime", a.config.Runtime)
	return fmt.Errorf(
		"model %s has format %q which is incompatible with %s runtime",
		model.Name, modelFormat, runtimeLabel,
	)
}

// ensureProcess ensures a llama-server process is running for the InferenceService.
// On UPDATED events, the spec is diffed against the running process's stored
// hash; if it changed, the existing process is stopped before a fresh one is
// spawned so the new flags actually take effect. Replicas=0 stops the process
// without restarting.
func (a *MetalAgent) ensureProcess(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	key := types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      isvc.Name,
	}.String()

	desiredHash := computeSpecHash(isvc)

	// Check if process already exists
	a.mu.RLock()
	existing, exists := a.processes[key]
	a.mu.RUnlock()

	// Honor spec.replicas=0 by stopping a running process and not respawning.
	// Without this, a user trying to take a model offline via spec edits has
	// to fully reload the metal-agent to evict it.
	if isvc.Spec.Replicas != nil && *isvc.Spec.Replicas == 0 {
		if exists {
			a.logger.Infow("replicas=0; stopping process",
				"namespace", isvc.Namespace, "name", isvc.Name)
			return a.deleteProcess(ctx, key)
		}
		return nil
	}

	if exists && existing.Healthy {
		if existing.SpecHash == desiredHash {
			a.logger.Debugw("inference service already has a healthy process with matching spec", "key", key)
			return nil
		}
		a.logger.Infow("spec changed; restarting process to pick up new flags",
			"namespace", isvc.Namespace, "name", isvc.Name,
			"oldSpecHash", existing.SpecHash, "newSpecHash", desiredHash)
		if err := a.deleteProcess(ctx, key); err != nil {
			return fmt.Errorf("failed to stop process before respawn: %w", err)
		}
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

	if err := a.validateRuntimeFormat(model); err != nil {
		return err
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

	// Apple Silicon defaults: flash-attn and mlock both ON. The user can
	// disable flash-attn by setting spec.flashAttention=false; mlock has no
	// CRD opt-out because the macOS wired-collector eviction it prevents is
	// the entire reason the Metal agent exists in the first place.
	flashAttn := true
	if isvc.Spec.FlashAttention != nil {
		flashAttn = *isvc.Spec.FlashAttention
	}
	batchSize := 0
	if isvc.Spec.BatchSize != nil {
		batchSize = int(*isvc.Spec.BatchSize)
	}
	uBatchSize := 0
	if isvc.Spec.UBatchSize != nil {
		uBatchSize = int(*isvc.Spec.UBatchSize)
	}

	cfg := buildExecutorConfig(isvc, model, executorBaseConfig{
		GPULayers:      gpuLayers,
		ContextSize:    contextSize,
		FlashAttention: flashAttn,
		BatchSize:      batchSize,
		UBatchSize:     uBatchSize,
	})

	// Start the process
	process, err := a.executor.StartProcess(ctx, cfg)
	if err != nil {
		return fmt.Errorf("failed to start process: %w", err)
	}

	// Stamp the spec hash onto the process so future ensureProcess calls
	// can detect drift via simple string compare.
	process.SpecHash = desiredHash

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

	// For shared-daemon runtimes (oMLX, Ollama), unload the specific model
	// instead of killing the shared daemon.
	if ollama, ok := a.executor.(*OllamaExecutor); ok && process.ModelID != "" {
		if err := ollama.UnloadModel(ctx, process.ModelID); err != nil {
			deleteErrors = append(deleteErrors,
				fmt.Errorf("failed to unload Ollama model %s: %w", process.ModelID, err))
		}
	} else if omlx, ok := a.executor.(*OMLXExecutor); ok && process.ModelID != "" {
		if err := omlx.UnloadModel(ctx, process.ModelID); err != nil {
			deleteErrors = append(deleteErrors,
				fmt.Errorf("failed to unload oMLX model %s: %w", process.ModelID, err))
		}
	} else if err := a.executor.StopProcess(process.PID); err != nil {
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
// runWatcherLoop drives a.watcher.Watch in a loop, retrying transient errors
// with exponential backoff (handles the "CRDs not installed yet" startup
// race) but bubbling ErrWatchStalled up via fatalErrChan immediately.
// Stalled means the watcher's controller-runtime client cache is wedged;
// restarting Watch on the same client cannot fix that, so the agent has to
// exit and let its supervisor recycle the process with a fresh client.
//
// Extracted from Start so the retry-vs-fatal decision is unit-testable.
func (a *MetalAgent) runWatcherLoop(
	ctx context.Context,
	eventChan chan<- InferenceServiceEvent,
	fatalErrChan chan<- error,
) {
	const (
		initialBackoff = 5 * time.Second
		maxBackoff     = 60 * time.Second
		backoffFactor  = 2
	)
	backoff := initialBackoff
	for {
		err := a.watcher.Watch(ctx, eventChan)
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrWatchStalled) {
			fatalExitsTotal.WithLabelValues("watcher").Inc()
			select {
			case fatalErrChan <- err:
			default:
			}
			return
		}
		a.logger.Warnw("watcher exited with error, retrying",
			"error", err, "retryIn", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= backoffFactor
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// reportHealthServerExit handles the post-Run state of the management HTTP
// server. It is a no-op when the server returned cleanly or when the agent
// itself is shutting down (ctx cancelled). Any other return is fatal and
// pushed to fatalErrChan so the agent exits — running the agent without the
// management plane (metrics, healthz, readyz) is exactly the failure mode
// #276 reported.
//
// Extracted from the Start goroutine so the no-op-vs-fatal classification is
// unit-testable without spinning up an HTTP server.
func (a *MetalAgent) reportHealthServerExit(
	ctx context.Context,
	runErr error,
	fatalErrChan chan<- error,
) {
	if runErr == nil || ctx.Err() != nil {
		return
	}
	a.logger.Errorw("health server exited unexpectedly, signalling fatal exit", "error", runErr)
	fatalExitsTotal.WithLabelValues("health-server").Inc()
	select {
	case fatalErrChan <- fmt.Errorf("health server exited unexpectedly: %w", runErr):
	default:
	}
}

// applePowerRunner is the slice of ApplePowerSampler the agent depends on.
// Defining it as an interface lets tests inject a fake whose Run() is a
// guaranteed no-op without having to construct a darwin-only struct from a
// Linux test binary.
type applePowerRunner interface {
	Run(ctx context.Context)
}

// maybeStartApplePowerSampler launches the powermetrics-driven Apple power
// sampler in a goroutine if the feature is enabled in the agent config. It
// returns the runner (or nil) so tests can verify wiring without poking into
// goroutine state. The factory is overridable in tests via
// applePowerSamplerFactory; in production it's NewApplePowerSampler.
//
// Extracted from Start so the conditional + wiring is unit-testable without
// having to spin up the full agent loop.
func (a *MetalAgent) maybeStartApplePowerSampler(ctx context.Context) applePowerRunner {
	if !a.config.ApplePowerEnabled {
		return nil
	}
	sampler := applePowerSamplerFactory(
		a.config.PowermetricsBin,
		a.config.ApplePowerInterval,
		a.logger.With("subsystem", "apple-power"),
	)
	go sampler.Run(ctx)
	return sampler
}

// applePowerSamplerFactory builds the runner. Defined as a package variable
// (rather than a direct call to NewApplePowerSampler) so tests can swap in a
// fake whose Run() is deterministic. Production code never reassigns it. The
// declared return type is the interface; the production constructor returns
// *ApplePowerSampler which satisfies it on every platform.
var applePowerSamplerFactory func(string, time.Duration, *zap.SugaredLogger) applePowerRunner = func(
	bin string, interval time.Duration, logger *zap.SugaredLogger,
) applePowerRunner {
	return NewApplePowerSampler(bin, interval, logger)
}

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

	// For shared-daemon runtimes (oMLX, Ollama), unload each model instead of
	// killing the daemon.
	omlx, isOMLX := a.executor.(*OMLXExecutor)
	ollama, isOllama := a.executor.(*OllamaExecutor)

	for key, process := range a.processes {
		if isOllama && process.ModelID != "" {
			if err := ollama.UnloadModel(ctx, process.ModelID); err != nil {
				shutdownErrors = append(shutdownErrors,
					fmt.Errorf("failed to unload Ollama model %s: %w", key, err))
			}
		} else if isOMLX && process.ModelID != "" {
			if err := omlx.UnloadModel(ctx, process.ModelID); err != nil {
				shutdownErrors = append(shutdownErrors,
					fmt.Errorf("failed to unload oMLX model %s: %w", key, err))
			}
		} else {
			if err := a.executor.StopProcess(process.PID); err != nil {
				shutdownErrors = append(shutdownErrors,
					fmt.Errorf("failed to stop %s: %w", key, err))
			}
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
		fileSizeBytes = uint64(info.Size()) //nolint:gosec // G115: os.FileInfo.Size is non-negative by contract
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

// computeSpecHash returns a stable hash of the InferenceServiceSpec fields that,
// if changed, require respawning the underlying llama-server process. Listing
// fields explicitly (rather than hashing the full Spec) keeps the hash stable
// across CRD additions that don't affect process invocation — adding a new
// status-only or controller-only field won't trigger a spurious respawn.
func computeSpecHash(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil {
		return ""
	}
	// Fields included MUST match what the executor actually consumes (or what
	// it will consume once #349 closes the ExecutorConfig gap). When adding a
	// new spec field that affects llama-server args, add it here too.
	relevant := struct {
		ModelRef               string
		ContextSize            *int32
		BatchSize              *int32
		UBatchSize             *int32
		ParallelSlots          *int32
		FlashAttention         *bool
		Jinja                  *bool
		NoKvOffload            *bool
		NoWarmup               *bool
		MoeCPUOffload          *bool
		MoeCPULayers           *int32
		CacheTypeK             string
		CacheTypeV             string
		CacheTypeCustomK       string
		CacheTypeCustomV       string
		TensorOverrides        []string
		MetadataOverrides      []string
		ExtraArgs              []string
		ReasoningBudget        *int32
		ReasoningBudgetMessage string
		Replicas               *int32
		Runtime                string
	}{
		ModelRef:               isvc.Spec.ModelRef,
		ContextSize:            isvc.Spec.ContextSize,
		BatchSize:              isvc.Spec.BatchSize,
		UBatchSize:             isvc.Spec.UBatchSize,
		ParallelSlots:          isvc.Spec.ParallelSlots,
		FlashAttention:         isvc.Spec.FlashAttention,
		Jinja:                  isvc.Spec.Jinja,
		NoKvOffload:            isvc.Spec.NoKvOffload,
		NoWarmup:               isvc.Spec.NoWarmup,
		MoeCPUOffload:          isvc.Spec.MoeCPUOffload,
		MoeCPULayers:           isvc.Spec.MoeCPULayers,
		CacheTypeK:             isvc.Spec.CacheTypeK,
		CacheTypeV:             isvc.Spec.CacheTypeV,
		CacheTypeCustomK:       isvc.Spec.CacheTypeCustomK,
		CacheTypeCustomV:       isvc.Spec.CacheTypeCustomV,
		TensorOverrides:        isvc.Spec.TensorOverrides,
		MetadataOverrides:      isvc.Spec.MetadataOverrides,
		ExtraArgs:              isvc.Spec.ExtraArgs,
		ReasoningBudget:        isvc.Spec.ReasoningBudget,
		ReasoningBudgetMessage: isvc.Spec.ReasoningBudgetMessage,
		Replicas:               isvc.Spec.Replicas,
		Runtime:                isvc.Spec.Runtime,
	}
	b, err := json.Marshal(relevant)
	if err != nil {
		// json.Marshal on this struct shape is effectively infallible; if it
		// somehow fails we fall back to the zero hash, which forces a respawn
		// — safer than skipping the diff entirely.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
