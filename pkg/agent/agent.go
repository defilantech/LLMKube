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
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// Inference runtime identifiers used by MetalAgentConfig.Runtime.
const (
	runtimeOMLX      = "omlx"
	runtimeOllama    = "ollama"
	runtimeVLLMSwift = "vllm-swift"
	runtimeMLXServer = "mlx-server"
)

// Model format identifiers (Model.Spec.Format) the agent recognizes for
// runtime-compatibility checks. Defined here so validateRuntimeFormat can
// switch on them without scattering literals (goconst).
const (
	formatGGUF = "gguf"
	formatMLX  = "mlx"
)

// MetalAgentConfig contains configuration for the Metal agent
type MetalAgentConfig struct {
	K8sClient      client.Client
	Namespace      string
	ModelStorePath string
	LlamaServerBin string
	Port           int
	// ClientPort is the host-side client-proxy listener port (#406). When > 0
	// the agent serves a stable 127.0.0.1:<ClientPort> endpoint that forwards
	// to the current child's dynamic port, so host tools need not track it.
	// 0 disables the proxy.
	ClientPort int
	HostIP     string // explicit IP to register in K8s endpoints; empty = auto-detect
	Logger     *zap.SugaredLogger

	// EventRecorder publishes Kubernetes events on managed InferenceService
	// objects so operators can triage memory pressure / eviction / respawn
	// behavior via `kubectl describe` (and any tool that surfaces K8s events:
	// k9s, Lens, ArgoCD). Nil disables event emission; the agent still
	// updates conditions and metrics. Tests can pass a record.FakeRecorder
	// to assert on the event stream. Closes #390.
	EventRecorder record.EventRecorder

	// Runtime selects the inference backend: "llama-server" (default), "omlx", or "ollama".
	Runtime string
	// OMLXBin is the path to the omlx binary. Only used when Runtime is "omlx".
	OMLXBin string
	// OMLXPort is the port the shared oMLX daemon listens on (default 8000).
	OMLXPort int
	// OllamaPort is the port the Ollama daemon listens on (default 11434).
	OllamaPort int
	// VLLMSwiftBin is the path to the vllm-swift binary. Only used when
	// Runtime is "vllm-swift". Empty means auto-detect via $PATH.
	VLLMSwiftBin string
	// MLXServerBin is the path to the mlx-server binary. Only used when
	// Runtime is "mlx-server".
	MLXServerBin string
	// MLXServerPort is the fixed port the mlx-server process binds.
	// Only used when Runtime is "mlx-server"; zero defaults to 8080.
	MLXServerPort int
	// LlamaServerPort is a fixed port for the llama-server runtime. Only used
	// when Runtime is "llama-server"; zero allocates an ephemeral port per
	// process (the historical behavior).
	LlamaServerPort int

	// MemoryProvider supplies system memory info. Nil defaults to DarwinMemoryProvider.
	MemoryProvider MemoryProvider
	// MemoryFraction is the fraction of total memory to budget for models (0 = auto-detect).
	MemoryFraction float64
	// MemoryCheckMode controls what happens when the pre-flight memory check
	// cannot be completed: MemoryCheckModeEnforce (default, also for "") fails
	// closed and refuses to start the process; MemoryCheckModeWarn logs and
	// proceeds (the legacy behavior).
	MemoryCheckMode string

	// WatchdogConfig configures the memory pressure watchdog. Nil disables it.
	WatchdogConfig *MemoryWatchdogConfig

	// EvictionEnabled gates the watchdog's eviction action. When false the
	// watchdog still updates conditions and metrics but never stops a
	// process. Default false because killing inference workloads silently
	// is a sharp tool: operators must opt in. Wire from --eviction-enabled.
	EvictionEnabled bool

	// MaxWatchFailures is the consecutive-failure threshold at which the
	// InferenceService watcher gives up on its current Kubernetes connection
	// and signals a fatal exit. Zero means use the watcher's built-in default
	// (DefaultMaxConsecutiveFailures).
	MaxWatchFailures int

	// InferenceServiceAllowlist optionally restricts which
	// InferenceServices this agent claims by name. Empty / nil claims
	// every metal-accelerator InferenceService in the namespace (v0.1
	// behavior); non-empty surfaces only the named ones to the
	// runtime executor. v0.2 (#524): lets multi-Mac fleets share a
	// cluster without racing for each other's InferenceServices.
	InferenceServiceAllowlist []string

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

	// VLLMSwiftStartupTimeout is how long the agent waits for vllm-swift to
	// respond on /health. Zero means use the executor default
	// (DefaultVLLMSwiftStartupTimeout). vLLM init + Swift bridge load + weight
	// load grow with model size; 120s default works for ~30B models on M5 Max.
	VLLMSwiftStartupTimeout time.Duration

	// MLXServerStartupTimeout is how long the agent waits for mlx-server to
	// respond on /health. Zero means use the executor default
	// (DefaultMLXServerStartupTimeout). MLX weight load grows with model
	// size; the 120s default works for ~35B models on M5 Max.
	MLXServerStartupTimeout time.Duration

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
	// memoryCheckWarnOnly is true when config.MemoryCheckMode resolved to
	// MemoryCheckModeWarn; an incomplete admission check then logs and
	// proceeds instead of failing closed.
	memoryCheckWarnOnly bool

	// pressureBlocked records namespacedName keys of processes the agent
	// evicted under memory pressure. Subsequent ensureProcess calls for these
	// keys are no-ops while lastPressureLevel != Normal, to prevent a
	// thrashing respawn loop where the controller's UPDATED event simply
	// re-spawns the process we just killed for memory.
	pressureBlocked map[string]bool
	// lastPressureLevel is the most recent pressure level reported by the
	// watchdog. Used to gate respawn (above) and to detect transitions for
	// status condition updates.
	lastPressureLevel MemoryPressureLevel
	// pressureObserved records the pressure level at which each managed
	// process key has already received a MemoryPressure condition patch.
	// Lets handleMemoryPressure patch new (late-spawned) processes at the
	// current level without re-patching ones already observed at it.
	// Reset on every level transition.
	pressureObserved map[string]MemoryPressureLevel

	// starting records namespacedName keys whose ensureProcess call is
	// in flight. The K8s watcher (handleEvent) and the health monitor
	// (scheduleRestart) can both call ensureProcess for the same key at the
	// same time; the model load between the processes[] check and the store
	// is long and unlocked, so without this guard both callers pass the
	// stale check and each spawns a runtime process — loading the model
	// twice, enough to exhaust host memory.
	starting map[string]bool
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

	// Priority is the InferenceService.Spec.Priority enum value
	// (critical/high/normal/low/batch) captured at spawn time. Used by the
	// memory-pressure eviction selector to pick the lowest-priority running
	// process when system memory is critical. Empty defaults to "normal".
	Priority string

	// EvictionProtection mirrors InferenceService.Spec.EvictionProtection
	// at spawn time. When true, the memory-pressure eviction selector
	// excludes this process from its candidate set. The MemoryPressure
	// status condition is still patched on protected services so operators
	// can see system pressure even when their workload is shielded from it.
	EvictionProtection bool
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

	// Resolve memory check mode. Unknown values fall back to enforce: the
	// fail-open direction is the dangerous one, so a typo must not pick it.
	warnOnly := false
	switch config.MemoryCheckMode {
	case "", MemoryCheckModeEnforce:
	case MemoryCheckModeWarn:
		warnOnly = true
	default:
		logger.Warnw("unknown memory-check-mode, defaulting to enforce",
			"mode", config.MemoryCheckMode)
	}

	return &MetalAgent{
		config:              config,
		processes:           make(map[string]*ManagedProcess),
		logger:              logger.With("component", "metal-agent"),
		memoryProvider:      provider,
		memoryFraction:      fraction,
		memoryCheckWarnOnly: warnOnly,
		pressureBlocked:     make(map[string]bool),
		pressureObserved:    make(map[string]MemoryPressureLevel),
		starting:            make(map[string]bool),
	}
}

// Start begins watching for InferenceService resources and managing processes
func (a *MetalAgent) Start(ctx context.Context) error {
	// Log effective memory budget and set gauge
	if total, err := a.memoryProvider.TotalMemory(); err == nil {
		budget := uint64(float64(total) * a.memoryFraction)
		checkMode := MemoryCheckModeEnforce
		if a.memoryCheckWarnOnly {
			checkMode = MemoryCheckModeWarn
		}
		a.logger.Infow("memory budget",
			"total", formatMemory(total),
			"fraction", a.memoryFraction,
			"budget", formatMemory(budget),
			"checkMode", checkMode,
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
	if len(a.config.InferenceServiceAllowlist) > 0 {
		a.watcher.SetNameAllowlist(a.config.InferenceServiceAllowlist)
		a.logger.Infow(
			"InferenceService name allowlist active (#524 multi-Mac partition)",
			"allowed", a.config.InferenceServiceAllowlist,
		)
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
	case runtimeVLLMSwift:
		vllmSwiftExec := NewVLLMSwiftExecutor(
			a.config.VLLMSwiftBin,
			a.config.ModelStorePath,
			a.logger.With("subsystem", "executor"),
		)
		if a.config.VLLMSwiftStartupTimeout > 0 {
			vllmSwiftExec.SetStartupTimeout(a.config.VLLMSwiftStartupTimeout)
		}
		a.executor = vllmSwiftExec
	case runtimeMLXServer:
		port := a.config.MLXServerPort
		if port == 0 {
			port = 8080
		}
		mlxServerExec := NewMLXServerExecutor(
			a.config.MLXServerBin,
			a.config.ModelStorePath,
			port,
			a.logger.With("subsystem", "executor"),
		)
		if a.config.MLXServerStartupTimeout > 0 {
			mlxServerExec.SetStartupTimeout(a.config.MLXServerStartupTimeout)
		}
		a.executor = mlxServerExec
	default:
		metalExec := NewMetalExecutor(
			a.config.LlamaServerBin,
			a.config.ModelStorePath,
			a.logger.With("subsystem", "executor"),
		)
		if a.config.LlamaServerStartupTimeout > 0 {
			metalExec.SetStartupTimeout(a.config.LlamaServerStartupTimeout)
		}
		metalExec.SetPort(a.config.LlamaServerPort)
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

	// Start the host-side client proxy (#406): a stable loopback listener that
	// forwards /v1/* to whichever child is currently running, so host tools
	// (opencode, aider, curl) target a fixed port instead of the agent's
	// dynamic per-spawn child port. In-cluster clients are unaffected.
	if a.config.ClientPort > 0 {
		cp := NewClientProxy(a, a.config.ClientPort, a.logger.With("subsystem", "client-proxy"))
		go func() {
			if err := cp.Start(ctx); err != nil {
				a.logger.Errorw("client proxy stopped", "err", err)
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

	// Start Apple Silicon power sampler (if enabled). The sampler shells out
	// to powermetrics under sudo and publishes the apple_power_*_watts gauges
	// for InferCost to scrape. Disabled by default because it requires a
	// NOPASSWD sudoers entry the operator must install explicitly.
	a.maybeStartApplePowerSampler(ctx)

	// Start memory watchdog (if configured). Pass handleMemoryPressure as
	// the callback so the watchdog can drive condition updates and (under
	// Critical pressure) eviction.
	if a.config.WatchdogConfig != nil {
		watchdog := NewMemoryWatchdog(
			a.memoryProvider,
			a.processMemInfoSnapshot,
			func(level MemoryPressureLevel, stats MemoryStats) {
				a.handleMemoryPressure(ctx, level, stats)
			},
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

// resolveCacheTypes picks the effective llama.cpp KV cache types from the
// InferenceService spec. Custom types (TurboQuant turbo3/turbo4 and any other
// fork-specific value) win over the enum-validated standard fields. Mirrors
// internal/controller's resolveCacheType so the metal-agent and the K8s
// runtime emit identical flags for the same spec.
func resolveCacheTypes(isvc *inferencev1alpha1.InferenceService) (k, v string) {
	k = isvc.Spec.CacheTypeK
	if isvc.Spec.CacheTypeCustomK != "" {
		k = isvc.Spec.CacheTypeCustomK
	}
	v = isvc.Spec.CacheTypeV
	if isvc.Spec.CacheTypeCustomV != "" {
		v = isvc.Spec.CacheTypeCustomV
	}
	return k, v
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
	cacheTypeK, cacheTypeV := resolveCacheTypes(isvc)

	var ropeType, ropeFactor string
	var ropeOrigCtx int
	if r := isvc.Spec.RopeScaling; r != nil {
		ropeType = string(r.Type)
		ropeFactor = r.Factor
		ropeOrigCtx = derefInt32(r.OriginalContext)
	}

	return ExecutorConfig{
		Name:                   isvc.Name,
		Namespace:              isvc.Namespace,
		ModelSource:            model.Spec.Source,
		ModelName:              model.Name,
		GPULayers:              base.GPULayers,
		ContextSize:            base.ContextSize,
		RopeScalingType:        ropeType,
		RopeScalingFactor:      ropeFactor,
		RopeScalingOrigCtx:     ropeOrigCtx,
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
		modelFormat = formatGGUF
	}

	var bad bool
	var runtimeLabel string
	switch a.config.Runtime {
	case runtimeOMLX:
		bad = modelFormat == formatGGUF
		runtimeLabel = runtimeOMLX
	case runtimeOllama:
		bad = modelFormat == formatMLX
		runtimeLabel = runtimeOllama
	case runtimeVLLMSwift:
		// vllm-swift accepts MLX directories AND HuggingFace safetensors
		// directories (the SwiftInferenceEngine reads both). gguf is the
		// only incompatible format.
		bad = modelFormat == formatGGUF
		runtimeLabel = runtimeVLLMSwift
	case runtimeMLXServer:
		// mlx-server reads MLX directories and HuggingFace safetensors
		// directories; gguf is the only incompatible format.
		bad = modelFormat == formatGGUF
		runtimeLabel = runtimeMLXServer
	default:
		bad = modelFormat == formatMLX
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
// currentBackend returns the loopback address of the inference child the
// host-side client proxy should forward to, satisfying backendProvider (#406).
// The agent tracks one process per InferenceService but in practice runs one
// at a time on a single Mac, so we return the first running child with an
// allocated port, preferring a healthy one. ok is false when none is running.
func (a *MetalAgent) currentBackend() (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var fallback string
	for _, p := range a.processes {
		if p == nil || p.Port <= 0 {
			continue
		}
		addr := fmt.Sprintf("127.0.0.1:%d", p.Port)
		if p.Healthy {
			return addr, true
		}
		if fallback == "" {
			fallback = addr
		}
	}
	if fallback != "" {
		return fallback, true
	}
	return "", false
}

func (a *MetalAgent) ensureProcess(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	key := types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      isvc.Name,
	}.String()

	desiredHash := computeSpecHash(isvc)

	// Serialize ensureProcess per service. The K8s watcher and the health
	// monitor's scheduleRestart can both reach ensureProcess for this key
	// concurrently; the spawn path between the processes[] check and the
	// store is long and unlocked, so without this guard both callers would
	// spawn a runtime process and load the model twice. A spec change that
	// arrives mid-spawn is dropped here and reconciled by the next watch
	// event — acceptable, where a double model load is not.
	a.mu.Lock()
	if a.starting[key] {
		a.mu.Unlock()
		a.logger.Debugw("ensureProcess already in flight; skipping", "key", key)
		return nil
	}
	a.starting[key] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.starting, key)
		a.mu.Unlock()
	}()

	// Check if process already exists
	a.mu.RLock()
	existing, exists := a.processes[key]
	blocked := a.pressureBlocked[key]
	pressureLevel := a.lastPressureLevel
	a.mu.RUnlock()

	// Refuse to respawn a service the watchdog evicted while pressure is
	// still abnormal. Without this guard, the controller's UPDATED event
	// loop would silently re-spawn the very process we just killed for
	// memory, defeating eviction. The block clears automatically once the
	// watchdog reports MemoryPressureNormal.
	if blocked && pressureLevel != MemoryPressureNormal {
		a.logger.Warnw("skipping ensureProcess; eviction-blocked under memory pressure",
			"namespace", isvc.Namespace, "name", isvc.Name,
			"pressureLevel", pressureLevel.String())
		// Surface the block as a Kubernetes event so an operator who
		// `kubectl describe`s a service that "won't start" can see why.
		// Synthesize a ManagedProcess shell with just the IS coordinates;
		// emitInferenceEvent re-fetches the live IS for the event target.
		a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
			corev1.EventTypeWarning, EventReasonRespawnBlocked,
			"Respawn blocked: previous process was evicted and host memory pressure is %s",
			pressureLevel.String(),
		)
		return nil
	}

	// Honor spec.replicas=0 by stopping a running process and not respawning.
	// Without this, a user trying to take a model offline via spec edits has
	// to fully reload the metal-agent to evict it.
	if isvc.Spec.Replicas != nil && *isvc.Spec.Replicas == 0 {
		return a.handleScaleToZero(ctx, isvc, key, exists)
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

	// Resolve KV cache types now (custom > standard) so the memory check and
	// the executor config see the same values; otherwise the pre-flight check
	// would always assume f16 and reject configs that fit thanks to TurboQuant
	// or q8_0 KV.
	cacheTypeK, cacheTypeV := resolveCacheTypes(isvc)

	// Pre-flight memory check
	if err := a.checkMemoryAdmission(ctx, isvc, model, contextSize, cacheTypeK, cacheTypeV); err != nil {
		return err
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
	// Capture the workload's priority enum at spawn time so the eviction
	// selector can rank running processes without re-reading the CRD.
	process.Priority = isvc.Spec.Priority
	// Capture eviction protection so the selector can filter the candidate
	// set without round-tripping to the apiserver under memory pressure.
	if isvc.Spec.EvictionProtection != nil {
		process.EvictionProtection = *isvc.Spec.EvictionProtection
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

// Condition / reason constants for the "manually scaled to zero"
// status patch. Kept here next to the only caller; if we grow more
// agent-driven conditions they can move into a shared constants file.
const (
	conditionAvailable          = "Available"
	reasonManuallyScaledToZero  = "ManuallyScaledToZero"
	phaseStopped                = "Stopped"
	messageManuallyScaledToZero = "spec.replicas=0; metal-agent has torn down the workload"
)

// handleScaleToZero stops the managed process (if any) for an
// InferenceService with spec.replicas=0 and patches its status so
// downstream observers see Phase=Stopped + readyReplicas=0
// immediately. Extracted from ensureProcess to keep the parent
// function under the gocyclo threshold.
func (a *MetalAgent) handleScaleToZero(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	key string,
	exists bool,
) error {
	if exists {
		a.logger.Infow("replicas=0; stopping process",
			"namespace", isvc.Namespace, "name", isvc.Name)
		if err := a.deleteProcess(ctx, key); err != nil {
			return err
		}
	}
	// Patch status whether or not we had a managed process. A user
	// editing replicas=0 wants kubectl / dashboards / HPA-like
	// callers to see Stopped immediately, not the stale Ready from
	// before the stop. Without this patch the InferenceService
	// keeps reporting readyReplicas from the prior generation. See
	// https://github.com/defilantech/LLMKube/issues/452.
	if err := a.markStopped(ctx, isvc); err != nil {
		a.logger.Warnw("failed to patch InferenceService status after stop",
			"namespace", isvc.Namespace, "name", isvc.Name, "error", err)
	}
	return nil
}

// markStopped patches the InferenceService status to reflect that the
// metal-agent has stopped the managed llama-server in response to
// spec.replicas=0. Without this patch, kubectl / dashboards / any HPA-
// style controller keep observing the stale Phase=Ready and
// ReadyReplicas count from before the stop. Best-effort: the caller
// logs and continues on error rather than blocking the stop itself.
//
// Surfaced as https://github.com/defilantech/LLMKube/issues/452.
func (a *MetalAgent) markStopped(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	// Re-fetch to avoid a stale resource version under conflict; the
	// watch may have delivered an older copy than what the apiserver
	// currently has.
	fresh := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      isvc.Name,
	}, fresh); err != nil {
		return fmt.Errorf("fetch InferenceService for status patch: %w", err)
	}

	// Idempotency: if we already patched to Stopped, skip the round
	// trip. This is hit on every reconcile for a long-stopped service.
	if fresh.Status.Phase == phaseStopped &&
		fresh.Status.ReadyReplicas == 0 &&
		fresh.Status.DesiredReplicas == 0 {
		return nil
	}

	fresh.Status.Phase = phaseStopped
	fresh.Status.ReadyReplicas = 0
	fresh.Status.DesiredReplicas = 0

	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type:               conditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: fresh.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reasonManuallyScaledToZero,
		Message:            messageManuallyScaledToZero,
	})

	return a.config.K8sClient.Status().Update(ctx, fresh)
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

// checkMemoryAdmission runs the pre-flight memory check for an
// InferenceService: estimate the model's memory need, resolve the budget, and
// reject the service when it does not fit. A check that cannot be completed
// (unknown model size, unreadable budget, memory query failure) FAILS CLOSED
// by default: an unsized llama-server wires its weights and KV cache into
// unevictable memory, and admitting one blind is how the 2026-06-09 host
// panic happened. MemoryCheckModeWarn restores the legacy log-and-proceed
// behavior.
func (a *MetalAgent) checkMemoryAdmission(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	model *inferencev1alpha1.Model,
	contextSize int,
	cacheTypeK, cacheTypeV string,
) error {
	estimate, err := a.estimateModelMemory(ctx, model, contextSize, cacheTypeK, cacheTypeV)
	if err != nil {
		return a.failMemoryCheck(ctx, isvc, fmt.Sprintf("memory estimation failed: %v", err))
	}
	memoryEstimatedBytes.WithLabelValues(isvc.Name, isvc.Namespace).Set(float64(estimate.TotalBytes))

	resolved, resolveErr := ResolveMemoryBudget(model.Spec.Hardware, a.memoryFraction)
	if resolveErr != nil {
		return a.failMemoryCheck(ctx, isvc, fmt.Sprintf("memory budget resolution failed: %v", resolveErr))
	}
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
			return a.failMemoryCheck(ctx, isvc, fmt.Sprintf("memory budget check failed: %v", budgetErr))
		}
	}

	if !budget.Fits {
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
	}

	a.logger.Infow("memory check passed",
		"estimate", formatMemory(budget.EstimateBytes),
		"budget", formatMemory(budget.BudgetBytes),
		"headroom", formatMemory(budget.HeadroomBytes),
		"source", resolved.Source,
	)
	return nil
}

// failMemoryCheck handles a memory admission check that could not be
// completed. In enforce mode (default) it records why on the
// InferenceService status, emits a warning event, and returns an error so
// the caller refuses to start the process. In warn mode it logs and returns
// nil, preserving the pre-fail-closed behavior.
func (a *MetalAgent) failMemoryCheck(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	reason string,
) error {
	if a.memoryCheckWarnOnly {
		a.logger.Warnw("memory check incomplete, proceeding without check (memory-check-mode=warn)",
			"reason", reason)
		return nil
	}

	// Warn, not error: the watcher retries every poll interval and zap
	// attaches stacktraces at error level, which floods the log for a
	// condition already surfaced via status and a Kubernetes event.
	a.logger.Warnw("memory check incomplete, refusing to start process",
		"reason", reason, "namespace", isvc.Namespace, "name", isvc.Name)
	isvc.Status.SchedulingStatus = "MemoryCheckFailed"
	isvc.Status.SchedulingMessage = reason
	if updateErr := a.config.K8sClient.Status().Update(ctx, isvc); updateErr != nil {
		a.logger.Warnw("failed to update InferenceService status", "error", updateErr)
	}
	a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
		corev1.EventTypeWarning, EventReasonMemoryCheckFailed,
		"Memory admission check could not be completed and failed closed: %s", reason,
	)
	return fmt.Errorf("memory admission check failed: %s", reason)
}

// estimateModelMemory builds a MemoryEstimate for a model using the file on
// disk (preferred), the Status.Size string, or a HEAD probe of an http(s)
// source as a last resort, plus GGUF metadata when available. Cache types are
// passed through so quantized KV caches (q8_0, turbo3, turbo4) produce a
// realistic estimate instead of always assuming f16. The remote fallback
// matters on fresh hosts: with no file on disk and a not-yet-populated status
// size ("0"), the old chain errored out and the caller started the process
// unchecked.
func (a *MetalAgent) estimateModelMemory(
	ctx context.Context,
	model *inferencev1alpha1.Model,
	contextSize int,
	cacheTypeK, cacheTypeV string,
) (MemoryEstimate, error) {
	var fileSizeBytes uint64
	var reasons []string

	// A model with an absolute local-path source is loaded in place by the
	// host agent (the Metal path) and is never copied into the model store,
	// so the model-store lookup below would miss it. Size it from the source
	// path directly.
	if filepath.IsAbs(model.Spec.Source) {
		if size, err := localModelSize(model.Spec.Source); err == nil {
			fileSizeBytes = size
		} else {
			reasons = append(reasons, fmt.Sprintf("local source not readable: %v", err))
		}
	}

	// Try to stat the model file in the model store (downloaded models).
	filename := filepath.Base(model.Spec.Source)
	localPath := filepath.Join(a.config.ModelStorePath, model.Name, filename)
	if fileSizeBytes == 0 {
		if info, err := os.Stat(localPath); err == nil {
			fileSizeBytes = uint64(info.Size()) //nolint:gosec // G115: os.FileInfo.Size is non-negative by contract
		} else {
			reasons = append(reasons, fmt.Sprintf("file not found at %s", localPath))
		}
	}

	// Fall back to parsing the human-readable size from model status. A
	// literal "0" means the controller has not measured the model yet; treat
	// it as absent rather than an error so the remote probe below still runs.
	if fileSizeBytes == 0 && model.Status.Size != "" {
		parsed, err := parseSize(model.Status.Size)
		switch {
		case err != nil:
			reasons = append(reasons, fmt.Sprintf("status size %q unparseable", model.Status.Size))
		case parsed == 0:
			reasons = append(reasons, "status size not populated yet")
		default:
			fileSizeBytes = parsed
		}
	}

	// Last resort: HEAD the source for its Content-Length.
	if fileSizeBytes == 0 &&
		(strings.HasPrefix(model.Spec.Source, "http://") || strings.HasPrefix(model.Spec.Source, "https://")) {
		size, err := remoteModelSize(ctx, model.Spec.Source)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("remote size probe failed: %v", err))
		} else {
			fileSizeBytes = size
		}
	}

	if fileSizeBytes == 0 {
		return MemoryEstimate{}, fmt.Errorf(
			"cannot determine model size: %s", strings.Join(reasons, "; "))
	}

	var layerCount, embeddingSize uint64
	if model.Status.GGUF != nil {
		layerCount = model.Status.GGUF.LayerCount
		embeddingSize = model.Status.GGUF.EmbeddingSize
	}

	return EstimateModelMemoryWithOptions(fileSizeBytes, layerCount, embeddingSize, contextSize, EstimateOptions{
		CacheTypeK: cacheTypeK,
		CacheTypeV: cacheTypeV,
	}), nil
}

// localModelSize returns the on-disk size of a model at a local path: the
// file size for a single-file model (e.g. GGUF), or the summed size of the
// regular files for a model directory (e.g. an MLX safetensors model).
func localModelSize(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return uint64(info.Size()), nil //nolint:gosec // G115: os.FileInfo.Size is non-negative by contract
	}
	var total uint64
	walkErr := filepath.WalkDir(path, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += uint64(fi.Size()) //nolint:gosec // G115: os.FileInfo.Size is non-negative by contract
		return nil
	})
	if walkErr != nil {
		return 0, walkErr
	}
	return total, nil
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
