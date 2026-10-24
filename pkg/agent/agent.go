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
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/safehttp"
	"github.com/defilantech/llmkube/pkg/agent/policy"
)

// Inference runtime identifiers used by MetalAgentConfig.Runtime and
// InferenceService.Spec.Runtime. runtimeLlamaCPP ("llamacpp") is the
// canonical llama.cpp value the CRD and the in-cluster controller emit;
// runtimeLlamaServer ("llama-server") is the metal-agent's historical
// key and the global --runtime default. Both map to the same llama.cpp
// executor (#784) so CRs authored with either value resolve correctly.
// Only "llamacpp" and the Metal-only runtimes are accepted by the CRD enum;
// "llama-server" is reachable only through the --runtime flag. The
// Metal-only names come from the API package so the controller's placement
// check and the agent's executor keys cannot drift apart.
const (
	runtimeLlamaServer = "llama-server"
	runtimeLlamaCPP    = "llamacpp"
	runtimeOMLX        = inferencev1alpha1.RuntimeOMLX
	runtimeOllama      = inferencev1alpha1.RuntimeOllama
	runtimeVLLMSwift   = inferencev1alpha1.RuntimeVLLMSwift
	runtimeMLXServer   = inferencev1alpha1.RuntimeMLXServer
	runtimeTensorFold  = inferencev1alpha1.RuntimeTensorFold
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
	Version    string // agent binary version string stamped on Endpoints annotations; empty omits the annotation
	Logger     *zap.SugaredLogger

	// EventRecorder publishes Kubernetes events on managed InferenceService
	// objects so operators can triage memory pressure / eviction / respawn
	// behavior via `kubectl describe` (and any tool that surfaces K8s events:
	// k9s, Lens, ArgoCD). Nil disables event emission; the agent still
	// updates conditions and metrics. Tests can pass a record.FakeRecorder
	// to assert on the event stream. Closes #390.
	EventRecorder record.EventRecorder

	// Runtime is the agent-wide default inference backend, used for any
	// InferenceService that leaves spec.runtime empty (#525): "llama-server"
	// (default), "omlx", "ollama", "vllm-swift", "mlx-server", or
	// "tensorfold". A CR that sets spec.runtime overrides it (see
	// resolveRuntime).
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
	// TensorFoldBin is the path to the tensorfold binary. The executor is
	// registered when this is set or Runtime is "tensorfold"; empty then
	// means "tensorfold" from $PATH.
	TensorFoldBin string
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
	// AllowModelMemoryBudgetOverride lets a Model's spec.hardware.memoryBudget
	// or memoryFraction exceed this Mac's budget (total * MemoryFraction).
	// False (the default) clamps such a Model budget to the Mac's budget with
	// a Warning event, so a Model cannot raise the limit its Mac's owner set.
	AllowModelMemoryBudgetOverride bool

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

	// HealthFailureThreshold is how many consecutive failed health probes
	// make the agent stop an engine and start a new one. The first failure
	// already withdraws the engine's endpoint. Zero means
	// DefaultHealthFailureThreshold.
	HealthFailureThreshold int

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

	// TensorFoldStartupTimeout is how long the agent waits for tensorfold to
	// respond on /health. Zero means use the executor default
	// (DefaultTensorFoldStartupTimeout), which leaves room for a first run's
	// kernel warmup and exactness self-check.
	TensorFoldStartupTimeout time.Duration

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

	// AllowedModelRoots lists directories, in addition to the model store
	// (which is always allowed), that local model sources, the oMLX
	// pagedSSDCacheDir and path-valued extraArgs may resolve into (symlinks
	// followed). Empty means the model store only.
	AllowedModelRoots []string

	// AllowedDownloadHosts lists hostnames and CIDRs the agent may download
	// models from even though they resolve to a private, loopback or
	// link-local address, for example a LAN MinIO (--allowed-download-hosts).
	// Every other such address is refused by the SSRF guard. Nil allowlists
	// nothing.
	AllowedDownloadHosts []string

	// MaxDownloadBytes caps the bytes a single Model download may write,
	// counting bytes a resumed download already has on disk
	// (--max-download-size, whose default cmd/metal-agent resolves to the
	// host's total memory). A download over it is stopped, its partial
	// removed, and the start refused with ModelDownloadTooLarge. Zero means
	// no cap.
	MaxDownloadBytes int64

	// AllowUnsafeExtraArgs relaxes the extraArgs policy on a Mac whose
	// InferenceService authors are fully trusted: unknown flags, stray
	// tokens, refused non-listener flags, path checks and vllm-swift
	// code-loading checks are skipped. Flags that set or move the engine's
	// listener or registration, or start multi-node or distributed backends, stay refused.
	// Logged at Warn on startup.
	AllowUnsafeExtraArgs bool

	// IngressPort is the TLS port of the authenticated ingress that
	// in-cluster relays use to reach engines. The ingress listens on all
	// interfaces; engines listen on 127.0.0.1 only. Must differ from Port and
	// ClientPort. Ignored in legacy mode.
	IngressPort int
	// StateDir holds agent state (the ingress TLS identity, under
	// "<StateDir>/ingress"). Empty means
	// ~/Library/Application Support/llmkube/metal-agent.
	StateDir string
	// LegacyDirectEndpoints is the deprecated pre-ingress mode: engines bind
	// on all interfaces without authentication and are registered directly
	// as "<isvc>", no ingress is started. Only for a controller that
	// predates relay support; removed in a future release.
	LegacyDirectEndpoints bool
}

// processInspectorFactory builds the inspector NewMetalAgent hands the engine
// record store. A variable so the package tests, whose engines carry fake
// PIDs, can substitute one that accepts them.
var processInspectorFactory = newProcessInspector

// MetalAgent watches Kubernetes InferenceService resources and manages
// native inference processes with Metal acceleration
type MetalAgent struct {
	config         MetalAgentConfig
	watcher        *InferenceServiceWatcher
	executors      map[string]ProcessExecutor // runtime name -> executor
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

	// downloadAllow is config.AllowedDownloadHosts parsed once. The pre-flight
	// size probe HEADs the Model source, so it dials through the same SSRF
	// guard and allowlist as the executor's downloads.
	downloadAllow safehttp.Allowlist

	// pressureBlocked records namespacedName keys of processes the agent
	// evicted under memory pressure. Subsequent ensureProcess calls for these
	// keys are no-ops, to prevent a thrashing respawn loop where the
	// controller's UPDATED event simply re-spawns the process we just killed
	// for memory. Once pressure is Normal, handleMemoryPressure moves the
	// ones the available memory has room for to pressureRestarting and
	// queues their restart.
	pressureBlocked map[string]evictedService
	// pressureRestarting holds evicted services whose restart is queued
	// (queueRestart), until their engine is running or the restart is
	// cancelled. Their RSS stays reserved against the headroom meanwhile.
	pressureRestarting map[string]evictedService
	// evictionBackoff is the restart backoff of services restarted after an
	// eviction (see nextEvictedRestartLocked).
	evictionBackoff map[string]*evictionBackoff
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
	// in flight, and keys whose unhealthy engine the health monitor is
	// stopping before a restart (restartUnhealthy). The model load between
	// the processes[] check and the store is long and unlocked, so without
	// this guard a second start for the key would pass the stale check and
	// spawn another runtime process next to the first: loading the model
	// twice, enough to exhaust host memory.
	starting map[string]bool

	// reservedMemory holds the admitted estimate of each start that
	// passed memory admission and has not yet stored its process, keyed like
	// processes. Admission counts it next to running processes, so two
	// services starting at once cannot both fit into the same free budget;
	// the start moves it onto ManagedProcess.AdmittedBytes or releases it.
	// Guarded by mu.
	reservedMemory map[string]uint64

	// restarts holds the health-restart backoff of each service whose engine
	// the health monitor has restarted (see health_restart.go).
	restarts map[string]*restartBackoff
	// restartWake asks the event loop to run due health restarts. Buffered
	// (capacity 1) so a wake-up never blocks its sender.
	restartWake chan struct{}

	// roots is the set of directories local model sources and the oMLX
	// pagedSSDCacheDir must resolve into. Derived from
	// config.AllowedModelRoots, defaulting to the model store.
	roots policy.Roots
	// home is the agent process's home directory, used to expand a leading
	// "~" in a path checked against roots. Empty when it cannot be
	// determined, in which case a "~"-prefixed path is refused.
	home string

	// engineRecords keeps the on-disk record of each engine this agent
	// started (<stateDir>/engines), so a restarted agent can adopt it (#2079).
	engineRecords *engineRecordStore
	// inspect reads the kernel identity of a PID (start token, executable). It
	// is the record store's inspector, used again to verify recorded engines
	// at adoption and before group-signalling an adopted one.
	inspect processInspector
	// healthChecker probes an engine's health endpoint, for the health monitor
	// and for adoption.
	healthChecker ProcessHealthChecker
	// adopted holds the keys of the engines adopted at startup, whose
	// endpoints the startup withdrawal leaves Ready. Guarded by mu.
	adopted map[string]bool

	// now returns the current time for the RelayNotAdopted watchdog.
	// Defaults to time.Now; tests inject a fake clock.
	now func() time.Time
	// relayMu guards relayRegisteredAt and relayNotAdoptedSent.
	relayMu sync.Mutex
	// relayRegisteredAt records, per namespacedName key, the first
	// successful "<isvc>-agent" registration in relay mode.
	relayRegisteredAt map[string]time.Time
	// relayNotAdoptedSent records keys that already received their one
	// RelayNotAdopted Event in this agent process.
	relayNotAdoptedSent map[string]bool

	// digestMismatches remembers a Model's most recent SHA256 mismatch or
	// over-cap download, so reconcileProcess can refuse such a Model without
	// re-downloading the whole file (tens of GB) every reconcile.
	digestMismatches *digestMismatchMemo
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

	// Runtime is the inference runtime that spawned this process
	// (e.g. "llama-server", "mlx-server", "omlx"). Captured at spawn time
	// so deleteProcess and Shutdown can pick the correct executor even
	// when the agent hosts multiple runtimes concurrently (#525).
	Runtime string

	// AdmittedBytes is the memory estimate this process was admitted with.
	// Memory admission sums it across running processes, so a new model must
	// fit next to them, not just on its own (#2080). Zero when admission did
	// not size the process (memory-check-mode=warn after a failed estimate).
	AdmittedBytes uint64

	// PGID and StartToken are the engine's process group and kernel start
	// token, as its record holds them (#2079).
	PGID       int
	StartToken string
	// Adopted marks an engine a previous agent process started and this one
	// adopted at startup. It is not this process's child, so no executor
	// tracks it: stopping it goes through stopEngineGroup, which re-proves
	// its identity before signalling its group.
	Adopted bool

	// healthFailures counts this process's consecutive failed health probes.
	// Guarded by MetalAgent.mu; a replacement process starts again at zero.
	healthFailures int
}

// EffectiveModelRoots returns the root paths NewMetalAgent resolves into
// policy.Roots: modelStorePath (when non-empty) always comes first, followed
// by every entry of allowedRoots that is not an exact duplicate of it.
// Exported so cmd/metal-agent can validate the very same effective set at
// startup, before constructing the agent, using policy.NewRoots directly.
func EffectiveModelRoots(modelStorePath string, allowedRoots []string) []string {
	var paths []string
	seen := make(map[string]bool)
	if modelStorePath != "" {
		paths = append(paths, modelStorePath)
		seen[modelStorePath] = true
	}
	for _, p := range allowedRoots {
		if seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	return paths
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

	// Resolve home first: NewRoots needs it to expand a leading "~" in a
	// configured root, and CheckPath needs it to expand "~" in a checked
	// path.
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Warnw("could not determine home directory; a \"~\"-prefixed path will be refused", "error", err)
	}

	// Resolve the allowed model roots. The model store is always a root:
	// downloaded model sources and Model.spec local paths land there
	// regardless of --allowed-model-roots, so leaving it out would break
	// every deployment that also configures extra roots. Configured roots
	// are added on top of it; an exact duplicate of the model store is
	// skipped rather than resolved twice. A root that fails to resolve
	// discards the whole configured set (model store included) rather than
	// silently dropping just that one entry, since NewRoots returns an
	// all-or-nothing Roots{} on error; the agent still starts (it will
	// simply refuse every local model path check) because this constructor
	// has no error return.
	rootPaths := EffectiveModelRoots(config.ModelStorePath, config.AllowedModelRoots)
	roots, ignoredRoots, err := policy.NewRoots(rootPaths, home)
	if err != nil {
		logger.Errorw("invalid allowed model roots; the whole configured set (model store included) "+
			"is discarded, so local model paths will all be refused", "error", err)
	}
	for _, r := range ignoredRoots {
		logger.Warnw("allowed model root does not exist; ignoring it", "root", r)
	}

	if config.AllowUnsafeExtraArgs {
		logger.Warnw("extraArgs policy relaxed by --allow-unsafe-extra-args: unknown flags, stray tokens, " +
			"refused non-listener flags, paths and vllm-swift code-loading flags from InferenceServices " +
			"are not checked; listener, registration, multi-node and distributed flags stay refused")
	}

	// The state directory holds the engine records in relay and legacy mode
	// alike; a directory that cannot be resolved fails each record write, and
	// so each engine start, rather than the agent's startup.
	stateDir, stateDirErr := resolveStateDir(config.StateDir, home)
	inspect := processInspectorFactory()
	records := newEngineRecordStore(filepath.Join(stateDir, "engines"), config.Version, inspect, stateDirErr)
	if stateDirErr != nil {
		logger.Warnw("engine records unavailable: state directory not resolved",
			"engineRecords", records.Dir(), "error", stateDirErr)
	} else {
		logger.Infow("engine records directory", "engineRecords", records.Dir())
	}

	return &MetalAgent{
		config:              config,
		engineRecords:       records,
		inspect:             inspect,
		healthChecker:       NewDefaultProcessHealthChecker(5 * time.Second),
		executors:           make(map[string]ProcessExecutor),
		processes:           make(map[string]*ManagedProcess),
		logger:              logger.With("component", "metal-agent"),
		memoryProvider:      provider,
		roots:               roots,
		home:                home,
		memoryFraction:      fraction,
		memoryCheckWarnOnly: warnOnly,
		downloadAllow:       safehttp.ParseAllowlist(config.AllowedDownloadHosts),
		pressureBlocked:     make(map[string]evictedService),
		pressureRestarting:  make(map[string]evictedService),
		evictionBackoff:     make(map[string]*evictionBackoff),
		pressureObserved:    make(map[string]MemoryPressureLevel),
		starting:            make(map[string]bool),
		reservedMemory:      make(map[string]uint64),
		restarts:            make(map[string]*restartBackoff),
		restartWake:         make(chan struct{}, 1),
		now:                 time.Now,
		relayRegisteredAt:   make(map[string]time.Time),
		relayNotAdoptedSent: make(map[string]bool),
		digestMismatches:    newDigestMismatchMemo(),
	}
}

// Start begins watching for InferenceService resources and managing processes
// buildExecutors populates a.executors with one ProcessExecutor per runtime
// whose binary path is configured, so each InferenceService can pick its own
// backend via spec.runtime (#525). The llama.cpp executor is always created
// because it is the default fallback, and it is registered under BOTH
// runtimeLlamaServer ("llama-server", the historical key and --runtime
// default) and runtimeLlamaCPP ("llamacpp", the CRD/controller canonical
// value) so CRs authored with either runtime name resolve to it (#784).
func (a *MetalAgent) buildExecutors() {
	metalExec := NewMetalExecutor(
		a.config.LlamaServerBin,
		a.config.ModelStorePath,
		a.logger.With("subsystem", "executor"),
		WithKubeClient(a.config.Namespace, a.config.K8sClient, nil),
		WithAllowedDownloadHosts(a.config.AllowedDownloadHosts),
		WithMaxDownloadBytes(a.config.MaxDownloadBytes),
	)
	if a.config.LlamaServerStartupTimeout > 0 {
		metalExec.SetStartupTimeout(a.config.LlamaServerStartupTimeout)
	}
	metalExec.SetPort(a.config.LlamaServerPort)
	a.executors[runtimeLlamaServer] = metalExec
	a.executors[runtimeLlamaCPP] = metalExec

	if a.config.OMLXBin != "" {
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
		a.executors[runtimeOMLX] = omlxExec
	}
	if a.config.OllamaPort != 0 || a.config.Runtime == runtimeOllama {
		port := a.config.OllamaPort
		if port == 0 {
			port = 11434
		}
		a.executors[runtimeOllama] = NewOllamaExecutor(
			port,
			a.logger.With("subsystem", "executor"),
		)
	}
	if a.config.VLLMSwiftBin != "" {
		vllmSwiftExec := NewVLLMSwiftExecutor(
			a.config.VLLMSwiftBin,
			a.config.ModelStorePath,
			a.logger.With("subsystem", "executor"),
		)
		if a.config.VLLMSwiftStartupTimeout > 0 {
			vllmSwiftExec.SetStartupTimeout(a.config.VLLMSwiftStartupTimeout)
		}
		a.executors[runtimeVLLMSwift] = vllmSwiftExec
	}
	if a.config.MLXServerBin != "" {
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
		a.executors[runtimeMLXServer] = mlxServerExec
	}
	if a.config.TensorFoldBin != "" || a.config.Runtime == runtimeTensorFold {
		bin := a.config.TensorFoldBin
		if bin == "" {
			bin = runtimeTensorFold // resolved from $PATH by exec
		}
		tensorFoldExec := NewTensorFoldExecutor(
			bin,
			a.config.ModelStorePath,
			a.logger.With("subsystem", "executor"),
		)
		if a.config.TensorFoldStartupTimeout > 0 {
			tensorFoldExec.SetStartupTimeout(a.config.TensorFoldStartupTimeout)
		}
		a.executors[runtimeTensorFold] = tensorFoldExec
	}
}

func (a *MetalAgent) Start(ctx context.Context) error {
	if err := a.config.validateIngress(); err != nil {
		return err
	}

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
	fatalErrChan := make(chan error, 3)

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

	a.buildExecutors()

	a.registry = NewServiceRegistry(
		a.config.K8sClient,
		a.config.HostIP,
		a.logger.With("subsystem", "registry"),
		a.config.Version,
	)

	// Adopt engines a previous agent process left running, before the startup
	// sweep withdraws their endpoints (#2079). Needs no API server.
	a.startAdoption(ctx)

	// Put the registry in its mode (relay mode: identity + "<isvc>-agent"
	// registrations) and clean up what earlier agent processes left behind,
	// in that order; see prepareRegistry.
	ingressSrv, err := a.prepareRegistry(ctx)
	if err != nil {
		return err
	}

	// The ingress is the only way the cluster reaches an engine in relay
	// mode, so losing it (a bind failure included) is fatal.
	if ingressSrv != nil {
		go a.runIngress(ctx, ingressSrv, fatalErrChan)
		go a.warnIfDaemonsExposed()
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
		a.healthChecker,
		30*time.Second,
		a.logger.With("subsystem", "health-monitor"),
	)
	go monitor.Run(ctx)

	// Start heartbeat loop: periodically re-registers every running process's
	// endpoint to refresh the llmkube.ai/agent-heartbeat annotation and
	// self-heal any missed registration (#663, #657).
	go a.runHeartbeatLoop(ctx)

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
		watchdog.wantNormal = a.hasEvictedWaiting
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
		case <-a.restartWake:
			// Health restarts start here, serialized with watch events, so
			// a delete or scale-to-zero cannot race a restart's start.
			a.runDueRestarts(ctx)
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
		// Deleted, or no longer claimed by this agent: an eviction block or
		// queued eviction restart for it has nothing left to wait for.
		a.forgetEvicted(key)
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
		ServedModelName:        servedModelName(isvc, model),
		SourceSecretRef:        model.Spec.SourceSecretRef,
		SHA256:                 model.Spec.SHA256,
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
		Mode:                   isvc.Spec.Mode,
		ExtraArgs:              isvc.Spec.ExtraArgs,
		TurboQuantBits:         derefInt32(isvc.Spec.TurboQuantBits),
		PagedSSDCacheDir:       derefString(isvc.Spec.PagedSSDCacheDir),
		HotCacheMaxSize:        derefString(isvc.Spec.HotCacheMaxSize),
		PagedSSDCacheMaxSize:   derefString(isvc.Spec.PagedSSDCacheMaxSize),
	}
}

// servedModelName derives the llama-server --alias from spec.modelRef, falling
// back to the Model's name, and returns "" when neither is set. It mirrors the
// controller-side LlamaCppBackend.BuildArgs block so the in-cluster and metal
// runtimes report the same model ID; the parity test is the contract.
func servedModelName(isvc *inferencev1alpha1.InferenceService, model *inferencev1alpha1.Model) string {
	if isvc.Spec.ModelRef != "" {
		return isvc.Spec.ModelRef
	}
	if model != nil {
		return model.Name
	}
	return ""
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

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// resolveRuntime returns the effective runtime for an InferenceService.
// If isvc.Spec.Runtime is set, it is used directly. Otherwise the agent's
// global --runtime flag (a.config.Runtime) is the fallback. If both are
// empty, "llama-server" is the default. This lets each InferenceService
// pick its own backend while preserving backward compat for CRs that omit
// spec.runtime (#525).
func (a *MetalAgent) resolveRuntime(isvc *inferencev1alpha1.InferenceService) string {
	if isvc.Spec.Runtime != "" {
		return isvc.Spec.Runtime
	}
	if a.config.Runtime != "" {
		return a.config.Runtime
	}
	return runtimeLlamaServer
}

// validateRuntimeFormat returns an error if the model's format is incompatible
// with the given runtime. Empty format defaults to "gguf".
func (a *MetalAgent) validateRuntimeFormat(model *inferencev1alpha1.Model, runtime string) error {
	modelFormat := model.Spec.Format
	if modelFormat == "" {
		modelFormat = formatGGUF
	}

	var bad bool
	var runtimeLabel string
	switch runtime {
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
	case runtimeTensorFold:
		// tensorfold serves an MLX model directory; gguf (and the empty
		// format, which means gguf) cannot load.
		bad = modelFormat == formatGGUF
		runtimeLabel = runtimeTensorFold
	default:
		bad = modelFormat == formatMLX
		runtimeLabel = runtimeLlamaServer
	}
	if !bad {
		return nil
	}

	a.logger.Warnw("skipping incompatible model format for runtime",
		"model", model.Name, "format", modelFormat, "runtime", runtime)
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
// currentBackend returns the loopback address and runtime of the inference
// child the host-side client proxy should forward to, satisfying
// backendProvider (#406). The runtime is the same string the ingress uses
// (ManagedProcess.Runtime, mirrored into ingress.Route.Runtime by Route), so
// the client proxy can apply the identical ingress.Allowed path policy. The
// agent tracks one process per InferenceService but in practice runs one at a
// time on a single Mac, so we return the first running child with an
// allocated port, preferring a healthy one. ok is false when none is running.
func (a *MetalAgent) currentBackend() (addr, runtime string, ok bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var fallbackAddr, fallbackRuntime string
	for _, p := range a.processes {
		if p == nil || p.Port <= 0 {
			continue
		}
		candidate := fmt.Sprintf("127.0.0.1:%d", p.Port)
		if p.Healthy {
			return candidate, p.Runtime, true
		}
		if fallbackAddr == "" {
			fallbackAddr = candidate
			fallbackRuntime = p.Runtime
		}
	}
	if fallbackAddr != "" {
		return fallbackAddr, fallbackRuntime, true
	}
	return "", "", false
}

// isvcStopped reports whether the InferenceService desires zero running
// serving processes: either explicitly scaled to zero or administratively
// suspended (spec.suspend, e.g. held by the Kueue integration).
func isvcStopped(isvc *inferencev1alpha1.InferenceService) bool {
	if isvc.Spec.Suspend {
		return true
	}
	return isvc.Spec.Replicas != nil && *isvc.Spec.Replicas == 0
}

func (a *MetalAgent) ensureProcess(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	err := a.reconcileProcess(ctx, isvc)
	if err != nil {
		a.withdrawAfterStartFailure(ctx, isvc)
	}
	return err
}

// withdrawAfterStartFailure withdraws the endpoint of an InferenceService this
// agent failed to start, so kube-proxy stops routing to a port nothing listens
// on. Without it a slice left Ready by an earlier start (in this process or a
// previous one) keeps attracting traffic until the controller's heartbeat
// expiry, and forever for direct Service clients (#1918). Stopped or deleting
// services are skipped: those paths tear the endpoint down themselves, and a
// withdrawal would recreate a Service they just removed.
func (a *MetalAgent) withdrawAfterStartFailure(ctx context.Context, isvc *inferencev1alpha1.InferenceService) {
	if isvc.DeletionTimestamp != nil || isvcStopped(isvc) {
		return
	}
	key := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}.String()
	a.mu.RLock()
	p := a.processes[key]
	a.mu.RUnlock()
	if p != nil && p.Healthy {
		return
	}
	a.withdrawEndpointFor(ctx, isvc)
}

// withdrawInheritedEndpoints runs WithdrawOwnedEndpoints with the watcher's
// ownership predicate, so the allowlist partition (#524) bounds which slices a
// restarted agent may touch. The endpoint of an engine adopted at startup is
// left alone: it is still serving (#2079).
func (a *MetalAgent) withdrawInheritedEndpoints(ctx context.Context) {
	if a.watcher == nil {
		return
	}
	withdraw := func(ctx context.Context, isvc *inferencev1alpha1.InferenceService) bool {
		a.mu.RLock()
		adopted := a.adopted[isvc.Namespace+"/"+isvc.Name]
		a.mu.RUnlock()
		return !adopted && a.watcher.owns(ctx, isvc)
	}
	n, err := a.registry.WithdrawOwnedEndpoints(ctx, a.config.Namespace, withdraw)
	if err != nil {
		a.logger.Warnw("startup endpoint withdrawal failed", "error", err)
		return
	}
	if n > 0 {
		a.logger.Infow("withdrew endpoints inherited from a previous agent process", "count", n)
	}
}

// fetchModelForReconcile gets the Model isvc.Spec.ModelRef names. A NotFound
// result clears any remembered digest mismatch or over-cap download for it
// (nothing else revisits a NamespacedName once its Get starts 404ing, so this is the only place that
// notices the Model is gone). Split out of reconcileProcess, rather than
// inlined there, to keep its cyclomatic complexity from crossing the linter's
// threshold.
func (a *MetalAgent) fetchModelForReconcile(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService,
) (*inferencev1alpha1.Model, error) {
	model := &inferencev1alpha1.Model{}
	modelKey := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Spec.ModelRef}
	if err := a.config.K8sClient.Get(ctx, modelKey, model); err != nil {
		if apierrors.IsNotFound(err) {
			a.digestMismatches.clear(modelKey)
		}
		return nil, fmt.Errorf("failed to get model %s: %w", isvc.Spec.ModelRef, err)
	}
	return model, nil
}

// fetchModelThenStopStale gets the Model for isvc and only then stops
// existing, when it is a healthy process running an older spec (existing is
// nil when nothing runs for key). A lookup that fails with anything but
// NotFound (a timeout, throttling, a 5xx while the API server restarts)
// returns with the old process still serving: stopping it first would leave
// the service down until the InferenceService changes again, because nothing
// else retries (#2076). A NotFound Model can never be started, so the old
// process is stopped as before. Split out of reconcileProcess to keep its
// cyclomatic complexity under the linter's threshold.
//
// The Model reference is recorded (digestMismatchMemo.noteRef, so deleting
// the InferenceService later clears its Model's memo entry; a
// persistently-refused one never reaches a.processes, so this is the only
// place the association is available) only after the stop: deleteProcess
// forgets the previous reference with forgetISVC, and noting the new one
// first would have it forgotten, along with that Model's memo entry, at once.
func (a *MetalAgent) fetchModelThenStopStale(
	ctx context.Context, isvc *inferencev1alpha1.InferenceService, key string,
	existing *ManagedProcess, desiredHash string,
) (*inferencev1alpha1.Model, error) {
	model, modelErr := a.fetchModelForReconcile(ctx, isvc)
	if modelErr != nil && !apierrors.IsNotFound(modelErr) {
		return nil, modelErr
	}
	if existing != nil {
		a.logger.Infow("spec changed; restarting process to pick up new flags",
			"namespace", isvc.Namespace, "name", isvc.Name,
			"oldSpecHash", existing.SpecHash, "newSpecHash", desiredHash)
		if err := a.deleteProcess(ctx, key); err != nil {
			return nil, fmt.Errorf("failed to stop process before respawn: %w", err)
		}
	}
	if modelErr != nil {
		return nil, modelErr
	}
	a.digestMismatches.noteRef(key, types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Spec.ModelRef})
	return model, nil
}

// reconcileProcess is the body of ensureProcess; see ensureProcess for the
// start-failure withdrawal wrapped around it.
func (a *MetalAgent) reconcileProcess(ctx context.Context, isvc *inferencev1alpha1.InferenceService) error {
	key := types.NamespacedName{
		Namespace: isvc.Namespace,
		Name:      isvc.Name,
	}.String()

	desiredHash := computeSpecHash(isvc)

	// Serialize starts per service. The health monitor holds this guard
	// while it stops an unhealthy engine before a restart, and the spawn
	// path between the processes[] check and the store is long and
	// unlocked, so without it a second caller would spawn a runtime process
	// next to the first and load the model twice. A spec change that
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
	a.mu.Lock()
	existing, exists := a.processes[key]
	a.holdQueuedEvictedLocked(key)
	_, blocked := a.pressureBlocked[key]
	pressureLevel := a.lastPressureLevel
	a.mu.Unlock()

	// Refuse to respawn a service the watchdog evicted. Without this guard,
	// the controller's UPDATED event loop would silently re-spawn the very
	// process we just killed for memory, defeating eviction. The block holds
	// at Normal too, until the available memory has room for the service:
	// handleMemoryPressure then lifts it and queues the restart.
	if blocked {
		return a.refuseBlockedStart(ctx, isvc, key, exists, pressureLevel)
	}

	// Honor spec.replicas=0 or spec.suspend by stopping a running process and
	// not respawning. Without this, a user trying to take a model offline via
	// spec edits has to fully reload the metal-agent to evict it; without the
	// suspend half, the Kueue integration's admission hold would be ignored
	// on hosts running the metal agent, since it doesn't watch Deployments.
	if isvcStopped(isvc) {
		return a.handleScaleToZero(ctx, isvc, key, exists)
	}

	// With no engine running, a pending restart owns the start and its
	// backoff; see deferToPendingRestart.
	if a.deferToPendingRestart(key, desiredHash) {
		return nil
	}

	if exists && existing.SpecHash == desiredHash {
		// An unhealthy process is left to the health monitor, which stops
		// it and restarts it after healthFailureThreshold consecutive
		// failed probes. Starting a new engine here would leave this one
		// running untracked, with its weights still wired (#2077).
		a.logger.Debugw("inference service already has a process with matching spec",
			"key", key, "healthy", existing.Healthy)
		return nil
	}

	model, err := a.fetchModelThenStopStale(ctx, isvc, key, existing, desiredHash)
	if err != nil {
		return err
	}

	a.logger.Infow("starting inference service", "namespace", isvc.Namespace, "name", isvc.Name)

	// Resolve the effective runtime for this InferenceService.
	// spec.runtime wins; the agent's --runtime flag is the fallback.
	runtime := a.resolveRuntime(isvc)

	// Relay mode appends "-agent" to the Service name; refuse a name that no
	// longer fits a DNS label before starting anything.
	if a.relayActive() {
		if svcName := a.registry.serviceNameFor(isvc.Name); len(svcName) > validation.DNS1035LabelMaxLength {
			return a.refuseStart(ctx, isvc, EventReasonServiceNameTooLong, fmt.Sprintf(
				"Service name %q is %d characters, over the %d-character limit; shorten the InferenceService "+
					"name to at most %d characters", svcName, len(svcName), validation.DNS1035LabelMaxLength,
				validation.DNS1035LabelMaxLength-len(inferencev1alpha1.MetalAgentServiceSuffix)))
		}
	}

	// Refuse a taken endpoint name before anything else is evaluated or
	// started. Checking only at registration (after StartProcess) turns every
	// watch re-delivery into a full model load followed by a kill, and lets
	// memory admission's success path clear the refusal status in between.
	if err := a.registry.CheckEndpointName(ctx, isvc); err != nil {
		var conflict *EndpointNameConflictError
		if errors.As(err, &conflict) {
			return a.refuseStart(ctx, isvc, EventReasonEndpointNameConflict, conflict.Error())
		}
		return fmt.Errorf("failed to check endpoint name: %w", err)
	}

	// Refuse a Model the controller marked Failed, any local model source or
	// pagedSSDCacheDir that resolves outside the allowed roots, a symlink in
	// a downloaded source's cache slot, and a Model this agent already found
	// to have a mismatching SHA256 (refused without downloading it again).
	// All of these run before memory admission, since admission's success
	// path clears SchedulingStatus and a refusal placed after it would flap
	// the status on every reconcile.
	if err := a.checkModelPreflight(ctx, isvc, model, runtime, derefString(isvc.Spec.PagedSSDCacheDir)); err != nil {
		return err
	}

	// Refuse a dangerous or unrecognized extraArgs flag before memory
	// admission runs, for the same reason as checkModelPaths above: admission's
	// success path clears SchedulingStatus, so a refusal placed after it would
	// flap the status on every reconcile of an otherwise-admittable spec.
	// buildExecutorConfig passes isvc.Spec.ExtraArgs straight through to
	// ExecutorConfig.ExtraArgs with no transformation, so checking it here
	// checks exactly what the executor will receive.
	if err := a.checkExtraArgs(ctx, isvc, runtime, isvc.Spec.ExtraArgs); err != nil {
		return err
	}

	if err := a.validateRuntimeFormat(model, runtime); err != nil {
		return err
	}

	// Look up the executor for the resolved runtime.
	exec, ok := a.executors[runtime]
	if !ok {
		return fmt.Errorf("no executor registered for runtime %q; "+
			"ensure the corresponding binary is installed and the agent was started with the right flags", runtime)
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
	// Admission reserved the estimate; give it back unless the store below
	// moves it onto the process record.
	defer a.releaseAdmission(key)

	// Apple Silicon defaults: flash-attn and mlock both ON. The user can
	// disable flash-attn by setting spec.flashAttention=false. mlock has no
	// CRD field, because the macOS wired-collector eviction it prevents is the
	// entire reason the Metal agent exists; a service that must not wire its
	// weights chooses its own load mode in extraArgs (for example
	// "--load-mode mmap"), which suppresses the agent's (hasLoadModeExtraArg).
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
	cfg.BindHost = a.engineBindHost()
	ns, name := isvc.Namespace, isvc.Name
	cfg.OnSpawned = a.recordOnSpawn(ns, name, runtime)
	// A start that fails after the provisional record was written leaves no
	// record behind; a stored process keeps it.
	stored := false
	defer a.dropUnstoredRecord(ns, name, &stored)

	// Start the process using the runtime-specific executor.
	process, err := exec.StartProcess(ctx, cfg)
	if err != nil {
		return a.handleStartProcessError(ctx, isvc, model, err)
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
	// Capture the runtime so deleteProcess and Shutdown can pick the
	// correct executor even when the agent hosts multiple runtimes (#525).
	process.Runtime = runtime

	// Store process and update metrics. The admission reservation moves onto
	// the record in the same critical section, so the budget it holds is
	// never counted twice or dropped in between.
	a.mu.Lock()
	process.AdmittedBytes = a.reservedMemory[key]
	delete(a.reservedMemory, key)
	a.processes[key] = process
	managedProcesses.Set(float64(len(a.processes)))
	a.updateCommittedMetricLocked()
	a.mu.Unlock()
	// From here deleteProcess owns the engine and its record: it keeps the
	// record when the stop fails, so the deferred drop must not remove it.
	stored = true
	processHealthy.WithLabelValues(isvc.Name, isvc.Namespace).Set(1)

	if err := a.completeEngineRecord(ctx, key, process); err != nil {
		return err
	}

	// Register service endpoint in Kubernetes
	if err := a.registry.RegisterEndpointWithRetry(ctx, isvc, process.Port); err != nil {
		var conflict *EndpointNameConflictError
		if errors.As(err, &conflict) {
			// Race backstop: CheckEndpointName above saw the name free, but a
			// foreign object took it while the engine was starting. The name
			// belongs to someone else: do not serve under it.
			if stopErr := a.deleteProcess(ctx, key); stopErr != nil {
				a.logger.Warnw("failed to stop process after endpoint conflict", "key", key, "error", stopErr)
			}
			return a.refuseStart(ctx, isvc, EventReasonEndpointNameConflict, conflict.Error())
		}
		a.logger.Errorw(
			"failed to register endpoint",
			"namespace", isvc.Namespace,
			"name", isvc.Name,
			"port", process.Port,
			"error", err,
		)
	} else {
		a.noteRelayRegistered(key)
	}

	a.logger.Infow(
		"started inference service",
		"namespace", isvc.Namespace,
		"name", isvc.Name,
		"port", process.Port,
		"pid", process.PID,
	)
	a.noteEvictedRestarted(ctx, key)

	return nil
}

// errStopFailed marks the part of a deleteProcess error that means the
// engine may still be running. The eviction path clears pressureBlocked on
// this marker and on nothing else (#2091): a SIGKILL after the grace period,
// an engine already gone, or an endpoint that failed to unregister all leave
// the engine stopped and count as a completed eviction.
var errStopFailed = errors.New("process may still be running after teardown")

// engineStopped reports whether a stop error still means the engine is not
// running: it was SIGKILLed after ignoring SIGTERM for the grace period, or
// it was already gone when the stop ran (#2091).
func engineStopped(err error) bool {
	var killed *KilledAfterGraceError
	return errors.As(err, &killed) || errors.Is(err, errProcessAlreadyGone)
}

// deleteProcess stops a running inference process. It uses the process's
// Runtime field to pick the correct executor from the agent's executor
// registry, so multi-runtime agents can stop each process with its own
// backend (#525). An error it returns either carries errStopFailed (the
// engine may still be running) or is a teardown that completed with a
// non-fatal residue: a SIGKILL after the grace period, an engine already
// gone, or a failed endpoint unregister (#2091).
func (a *MetalAgent) deleteProcess(ctx context.Context, key string) error {
	// Clear any remembered digest mismatch for the Model this InferenceService
	// referenced, before the early-return below: a persistently-refused
	// InferenceService (the exact case a digest-mismatch memo exists for)
	// never reaches a.processes, since refuseStart returns before a process
	// is ever stored there.
	a.digestMismatches.forgetISVC(key)

	a.mu.Lock()
	// Whatever tears the service down (a delete, eviction, a spec change)
	// supersedes a pending restart, so a backed-off retry cannot start it
	// again later. Cancelled under the same lock as the process lookup: the
	// health monitor removes the process and registers its restart in one
	// critical section, so one of the two always sees the other.
	_, hadRestart := a.restarts[key]
	a.cancelRestartLocked(key)
	process, exists := a.processes[key]
	if !exists {
		a.mu.Unlock()
		if hadRestart {
			// The health monitor stopped the engine and left its endpoint
			// withdrawn for the restart; nothing else removes it now.
			return a.unregisterKey(ctx, key)
		}
		return nil
	}
	delete(a.processes, key)
	managedProcesses.Set(float64(len(a.processes)))
	a.updateCommittedMetricLocked()
	a.mu.Unlock()

	a.logger.Infow("stopping inference service", "key", key)
	namespace, name := parseKey(key)

	// Clean up per-process metrics
	processHealthy.DeleteLabelValues(name, namespace)
	memoryEstimatedBytes.DeleteLabelValues(name, namespace)
	healthCheckDuration.DeleteLabelValues(name, namespace)
	processRestarts.DeleteLabelValues(name, namespace)

	var deleteErrors []error
	stopErr := a.stopManagedProcess(ctx, process, true)
	// The record goes only once the engine is confirmed stopped.
	if stopErr == nil || engineStopped(stopErr) {
		a.removeEngineRecord(process)
	}
	if err := stopErr; err != nil {
		if engineStopped(err) {
			// The engine is stopped; the teardown completed without grace.
			// Keep the typed error so callers can tell it apart from a
			// failed stop (#2091).
			deleteErrors = append(deleteErrors, err)
		} else {
			deleteErrors = append(deleteErrors, fmt.Errorf("%w: %w", errStopFailed, err))
		}
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

// recordOnSpawn is the ExecutorConfig.OnSpawned hook: it writes the
// provisional record, naming the records directory in any failure.
func (a *MetalAgent) recordOnSpawn(ns, name, runtime string) func(SpawnedProcess) error {
	return func(sp SpawnedProcess) error {
		if err := a.engineRecords.WriteProvisional(ns, name, runtime, sp); err != nil {
			engineRecordErrorsTotal.Inc()
			return fmt.Errorf("%w in %s: %w", errEngineRecordWrite, a.engineRecords.Dir(), err)
		}
		return nil
	}
}

// dropUnstoredRecord removes the record of a start that never reached the
// process table.
func (a *MetalAgent) dropUnstoredRecord(ns, name string, stored *bool) {
	if !*stored {
		a.removeEngineRecord(&ManagedProcess{Namespace: ns, Name: name})
	}
}

// errEngineRecordWrite marks a start that failed because the engine's record
// could not be written.
var errEngineRecordWrite = errors.New("could not record the engine")

// completeEngineRecord turns the provisional record into the running one. A
// process nothing can find after an agent restart is worse than one that did
// not start, so a failed write stops the engine and fails the start.
func (a *MetalAgent) completeEngineRecord(ctx context.Context, key string, process *ManagedProcess) error {
	err := a.engineRecords.WriteRunning(process)
	if err == nil {
		return nil
	}
	engineRecordErrorsTotal.Inc()
	msg := fmt.Sprintf("could not record the engine in %s: %v", a.engineRecords.Dir(), err)
	a.emitInferenceEvent(ctx, process, corev1.EventTypeWarning, EventReasonEngineRecordFailed, "Stopping: %s", msg)
	if stopErr := a.deleteProcess(ctx, key); stopErr != nil {
		a.logger.Warnw("failed to stop an engine whose record could not be written", "key", key, "error", stopErr)
	}
	return errors.New(msg)
}

// removeEngineRecord drops the record of an engine that has been stopped.
func (a *MetalAgent) removeEngineRecord(process *ManagedProcess) {
	if a.engineRecords == nil {
		return // an agent assembled without NewMetalAgent (tests) keeps no records
	}
	if err := a.engineRecords.Remove(process.Namespace, process.Name); err != nil {
		a.logger.Warnw("failed to remove engine record",
			"namespace", process.Namespace, "name", process.Name, "error", err)
	}
}

// stopManagedProcess stops a process the caller has already removed from
// a.processes, with the executor that matches its runtime. For shared-daemon
// runtimes (oMLX, Ollama) it unloads the model instead of killing the shared
// daemon; for the others StopProcess sends SIGTERM and, after a grace period,
// SIGKILL. forgetServed marks a terminal stop (delete, scale-to-zero, spec
// teardown): the oMLX model is dropped from the executor's served set even
// when the unload did not reach a hung daemon, so a later daemon replacement
// does not reload a model nothing wants. The health restart passes false: its
// model must survive in the set so the replacement reloads it.
func (a *MetalAgent) stopManagedProcess(ctx context.Context, process *ManagedProcess, forgetServed bool) error {
	// Pick the executor that matches the process's runtime.
	exec := a.executors[process.Runtime]
	if exec == nil {
		// Fallback to the default executor if the runtime is unknown
		// (e.g. a process spawned before the multi-runtime refactor).
		exec = a.executors[runtimeLlamaServer]
	}

	if ollama, ok := exec.(*OllamaExecutor); ok && process.ModelID != "" {
		if err := ollama.UnloadModel(ctx, process.ModelID); err != nil {
			return fmt.Errorf("failed to unload Ollama model %s: %w", process.ModelID, err)
		}
		return nil
	}
	if omlx, ok := exec.(*OMLXExecutor); ok && process.ModelID != "" {
		err := omlx.UnloadModel(ctx, process.ModelID)
		if forgetServed {
			omlx.ForgetServedModel(process.ModelID)
		}
		if err != nil {
			return fmt.Errorf("failed to unload oMLX model %s: %w", process.ModelID, err)
		}
		return nil
	}
	if process.Adopted && process.PID > 0 {
		// Not our child: no executor tracks it. Signal its group once its
		// identity is proven again.
		if err := stopEngineGroup(process.PID, process.PGID, process.StartToken, a.inspect); err != nil {
			return fmt.Errorf("failed to stop adopted engine: %w", err)
		}
		return nil
	}
	if err := exec.StopProcess(process.PID); err != nil {
		return fmt.Errorf("failed to stop process: %w", err)
	}
	return nil
}

// Condition / reason constants for the stop-path status patch. Kept here
// next to the only caller; if we grow more agent-driven conditions they can
// move into a shared constants file. The phase strings are hoisted to
// api/v1alpha1 (PhaseStopped / PhaseSuspended) so the agent and the operator
// share one importable home: determinePhase returns Suspended
// unconditionally for spec.suspend, so if the agent wrote Stopped for a
// suspended service the two status writers would overwrite each other
// forever (the operator's watch has no status-only filter).
const (
	conditionAvailable          = "Available"
	reasonManuallyScaledToZero  = "ManuallyScaledToZero"
	reasonSuspended             = "Suspended"
	messageManuallyScaledToZero = "spec.replicas=0; metal-agent has torn down the workload"
	messageSuspended            = "spec.suspend=true; metal-agent has torn down the workload; spec.replicas preserved"
)

// handleScaleToZero stops the managed process (if any) for an
// InferenceService with spec.replicas=0 or spec.suspend and patches its
// status so downstream observers see Phase=Stopped (or Suspended) +
// readyReplicas=0 immediately. Extracted from ensureProcess to keep the
// parent function under the gocyclo threshold.
func (a *MetalAgent) handleScaleToZero(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	key string,
	exists bool,
) error {
	// A pending restart must not bring a stopped service back. The process
	// may already be gone (stopped by the health monitor), so this does not
	// rely on deleteProcess below; it also removes the endpoint such a stop
	// left withdrawn.
	if err := a.dropRestart(ctx, key); err != nil {
		a.logger.Warnw("failed to cancel the pending restart of a stopped service",
			"namespace", isvc.Namespace, "name", isvc.Name, "error", err)
	}
	if exists {
		a.logger.Infow("scaled to zero or suspended; stopping process",
			"namespace", isvc.Namespace, "name", isvc.Name,
			"suspend", isvc.Spec.Suspend)
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
// spec.suspend or spec.replicas=0. Without this patch, kubectl /
// dashboards / any HPA-style controller keep observing the stale
// Phase=Ready and ReadyReplicas count from before the stop. Best-effort:
// the caller logs and continues on error rather than blocking the stop
// itself.
//
// The phase written branches on the actual cause: suspend takes
// precedence (mirroring the operator's determinePhase, which checks
// Spec.Suspend before anything else) and writes Phase=Suspended with a
// suspend-specific reason/message; only a true replicas==0 writes
// Phase=Stopped/ManuallyScaledToZero. Writing Stopped for a suspended
// service would fight the operator's Suspended phase forever.
//
// Surfaced as https://github.com/defilantech/LLMKube/issues/452;
// suspend cause added for the Kueue integration (#1251).
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

	// Branch the status text on the cause, using the re-fetched spec as
	// the source of truth (a suspend flipped between the watch event and
	// now should win). Suspend beats replicas==0 when both hold, exactly
	// like the operator's determinePhase ordering.
	targetPhase := inferencev1alpha1.PhaseStopped
	reason := reasonManuallyScaledToZero
	message := messageManuallyScaledToZero
	if fresh.Spec.Suspend {
		targetPhase = inferencev1alpha1.PhaseSuspended
		reason = reasonSuspended
		message = messageSuspended
	}

	// Idempotency: if the status already reflects the target settled
	// state (whichever cause applies), skip the round trip. This is hit
	// on every reconcile for a long-stopped/suspended service, and it is
	// what keeps the agent from thrashing against the operator, which
	// also writes Phase=Suspended for suspended services.
	if fresh.Status.Phase == targetPhase &&
		fresh.Status.ReadyReplicas == 0 &&
		fresh.Status.DesiredReplicas == 0 {
		return nil
	}

	fresh.Status.Phase = targetPhase
	fresh.Status.ReadyReplicas = 0
	fresh.Status.DesiredReplicas = 0

	meta.SetStatusCondition(&fresh.Status.Conditions, metav1.Condition{
		Type:               conditionAvailable,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: fresh.Generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	})

	return a.config.K8sClient.Status().Update(ctx, fresh)
}

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

// withdrawEndpoint fetches the InferenceService for the given name/namespace
// and calls registry.WithdrawEndpoint to flip the endpoint's Ready condition
// to false. This is the event-driven path (#662): the health monitor calls
// this immediately when it detects a healthy→unhealthy transition, rather
// than waiting for the next heartbeat tick.
func (a *MetalAgent) withdrawEndpoint(ctx context.Context, name, namespace string) {
	isvc := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, isvc); err != nil {
		a.logger.Warnw("failed to fetch InferenceService for withdrawal",
			"name", name, "namespace", namespace, "error", err)
		return
	}
	a.withdrawEndpointFor(ctx, isvc)
}

// withdrawEndpointFor flips isvc's endpoint to Ready=false. With a managed
// process it withdraws at that process's port; without one (a start that
// failed before a child existed) it withdraws only a slice that already
// exists, at the port recorded on it, and never creates one.
func (a *MetalAgent) withdrawEndpointFor(ctx context.Context, isvc *inferencev1alpha1.InferenceService) {
	port := 0
	a.mu.RLock()
	if p := a.processes[types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}.String()]; p != nil {
		port = p.Port
	}
	a.mu.RUnlock()

	var err error
	if port > 0 {
		err = a.registry.WithdrawEndpoint(ctx, isvc, port)
	} else {
		_, err = a.registry.WithdrawEndpointIfPresent(ctx, isvc)
	}
	if err != nil {
		a.logger.Warnw("failed to withdraw endpoint",
			"name", isvc.Name, "namespace", isvc.Namespace, "error", err)
	}
}

// registerEndpoint fetches the InferenceService of proc, tracked under key,
// and calls registry.RegisterEndpoint to flip the endpoint's Ready condition
// back to true. This is the event-driven recovery path (#662): the health
// monitor calls this immediately when it detects an unhealthy→healthy
// transition, rather than waiting for the next heartbeat tick.
func (a *MetalAgent) registerEndpoint(ctx context.Context, key string, proc *ManagedProcess) {
	name, namespace := proc.Name, proc.Namespace
	isvc := &inferencev1alpha1.InferenceService{}
	if err := a.config.K8sClient.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}, isvc); err != nil {
		a.logger.Warnw("failed to fetch InferenceService for registration",
			"name", name, "namespace", namespace, "error", err)
		return
	}

	// Register only while proc is still the tracked process. A delete,
	// scale-to-zero or spec change handled on the event loop since the probe
	// removed or replaced it, and may have removed the endpoint; registering
	// now would bring that endpoint back.
	a.mu.RLock()
	current := a.processes[key] == proc
	port := proc.Port
	a.mu.RUnlock()
	if !current {
		return
	}

	if err := a.registry.RegisterEndpoint(ctx, isvc, port); err != nil {
		a.logger.Warnw("failed to register endpoint",
			"name", name, "namespace", namespace, "error", err)
		return
	}
	a.noteRelayRegistered(key)
}

// heartbeatOnce re-registers the endpoint for every currently-running managed
// process. It snapshots the running process set under RLock, releases the lock,
// then performs one best-effort re-registration per process (no lock held during
// network I/O). Failures are logged at Warn and skipped; the next tick is the
// retry (no separate backoff wrapper here because the interval itself provides
// the retry cadence, and using RegisterEndpointWithRetry would serialize all
// heartbeats behind a single stalled one).
//
// Two additional safety checks run per entry before re-registering:
//   - If the InferenceService is scaled to zero (spec.replicas == 0) or has a
//     DeletionTimestamp, skip re-registration — re-asserting Service+Endpoints
//     for a deleted or scaled-to-zero service would leave a routable ClusterIP
//     pointing at a dead port with no self-heal path.
//   - If the InferenceService is NotFound, treat it as a missed deletion event
//     and tear the process down via deleteProcess (the same path the watch event
//     handler uses), then continue to the next entry.
func (a *MetalAgent) heartbeatOnce(ctx context.Context) {
	a.mu.RLock()
	// Snapshot name/namespace/port/health while holding the lock so we don't
	// hold it across the API calls below. Capturing Healthy here lets the loop
	// withdraw (Ready=false) an unhealthy process's endpoint instead of
	// re-registering it (#662).
	type entry struct {
		namespace, name string
		port            int
		healthy         bool
	}
	entries := make([]entry, 0, len(a.processes))
	for _, p := range a.processes {
		if p == nil || p.Port <= 0 {
			continue
		}
		entries = append(entries, entry{p.Namespace, p.Name, p.Port, p.Healthy})
	}
	a.mu.RUnlock()

	for _, e := range entries {
		isvc := &inferencev1alpha1.InferenceService{}
		if err := a.config.K8sClient.Get(ctx, types.NamespacedName{
			Namespace: e.namespace,
			Name:      e.name,
		}, isvc); err != nil {
			if apierrors.IsNotFound(err) {
				// Missed deletion event: tear the process down via the same path
				// the watch-event handler uses. deleteProcess acquires its own
				// lock so it is safe to call here without holding a.mu.
				key := types.NamespacedName{Namespace: e.namespace, Name: e.name}.String()
				a.logger.Warnw("heartbeat: InferenceService gone; tearing down lingering process",
					"namespace", e.namespace, "name", e.name)
				if tearErr := a.deleteProcess(ctx, key); tearErr != nil {
					a.logger.Warnw("heartbeat: teardown failed",
						"namespace", e.namespace, "name", e.name, "error", tearErr)
				}
				continue
			}
			a.logger.Warnw("heartbeat: failed to fetch InferenceService",
				"namespace", e.namespace, "name", e.name, "error", err)
			continue
		}

		// Skip re-registration when the service is being deleted, scaled to
		// zero, or suspended. Re-asserting Service+Endpoints here would
		// resurrect networking that deleteProcess (or handleScaleToZero) just
		// cleaned up.
		if isvc.DeletionTimestamp != nil || isvcStopped(isvc) {
			continue
		}

		// Health-aware: a process the health monitor marked unhealthy (agent
		// alive, runtime down) is withdrawn (Ready=false, slice kept, heartbeat
		// refreshed) rather than re-registered, so kube-proxy drops the address
		// and the operator observes readyReplicas: 0 (#662).
		if !e.healthy {
			a.logger.Warnw("heartbeat: withdrawing endpoint; runtime unhealthy",
				"namespace", e.namespace, "name", e.name)
			if err := a.registry.WithdrawEndpoint(ctx, isvc, e.port); err != nil {
				a.logger.Warnw("heartbeat: failed to withdraw endpoint",
					"namespace", e.namespace, "name", e.name, "error", err)
			}
			continue
		}

		if err := a.registry.RegisterEndpoint(ctx, isvc, e.port); err != nil {
			a.logger.Warnw("heartbeat: failed to re-register endpoint",
				"namespace", e.namespace, "name", e.name, "error", err)
			continue
		}
		a.noteRelayRegistered(types.NamespacedName{Namespace: e.namespace, Name: e.name}.String())
	}

	a.checkRelayAdoption(ctx)
}

// runHeartbeatLoop periodically re-registers every running process's
// endpoint. This both refreshes the llmkube.ai/agent-heartbeat annotation
// (the controller expires registrations whose heartbeat goes stale, #663)
// and re-asserts the registration content, healing any update the agent
// failed to deliver earlier (#657).
func (a *MetalAgent) runHeartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(inferencev1alpha1.DefaultAgentHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.heartbeatOnce(ctx)
		}
	}
}

// Shutdown stops the agent's own work. Engines and their records are left in
// place: they keep serving and the next agent process adopts them (#2079).
// oMLX models are the exception until the oMLX daemon can be adopted: they are
// unloaded as before, since the daemon dies with the agent's process group.
// StopEngines is the explicit stop.
func (a *MetalAgent) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	// No health restart may start an engine after shutdown.
	for key := range a.restarts {
		a.cancelRestartLocked(key)
	}
	var omlx []*ManagedProcess
	for _, p := range a.processes {
		if p.Runtime == runtimeOMLX {
			omlx = append(omlx, p)
		}
	}
	kept := len(a.processes) - len(omlx)
	a.mu.Unlock()

	var errs []error
	for _, p := range omlx {
		if err := a.stopManagedProcess(ctx, p, false); err != nil && !engineStopped(err) {
			errs = append(errs, fmt.Errorf("unload oMLX model %s/%s: %w", p.Namespace, p.Name, err))
			continue // the model may still be loaded: keep its record
		}
		a.removeEngineRecord(p)
	}
	a.logger.Infow("agent stopping; engines keep running for the next agent process to adopt", "engines", kept)
	return errors.Join(errs...)
}

// StopEngines stops every engine this agent tracks: it withdraws the endpoint
// (best effort), stops the engine with the executor that matches its runtime
// (or the group of an adopted one), and removes the record of each engine that
// is confirmed stopped. Unlike Shutdown, nothing is left for a later agent.
func (a *MetalAgent) StopEngines(ctx context.Context) error {
	a.mu.Lock()
	for key := range a.restarts {
		a.cancelRestartLocked(key)
	}
	procs := make([]*ManagedProcess, 0, len(a.processes))
	for _, p := range a.processes {
		procs = append(procs, p)
	}
	a.mu.Unlock()

	a.logger.Infow("stopping all engines", "count", len(procs))

	var errs []error
	for _, p := range procs {
		if a.registry != nil {
			isvc := &inferencev1alpha1.InferenceService{
				ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.Name},
			}
			if _, err := a.registry.WithdrawEndpointIfPresent(ctx, isvc); err != nil {
				a.logger.Warnw("failed to withdraw endpoint",
					"namespace", p.Namespace, "name", p.Name, "error", err)
			}
		}
		if err := a.stopManagedProcess(ctx, p, false); err != nil {
			errs = append(errs, fmt.Errorf("failed to stop %s/%s: %w", p.Namespace, p.Name, err))
			if !engineStopped(err) {
				continue // the engine may still run: keep its record
			}
		}
		a.removeEngineRecord(p)
	}
	return errors.Join(errs...)
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

	// This Mac's budget, the limit its owner set with --memory-fraction. Every
	// admitted model shares it, and a Model budget may not exceed it.
	total, totalErr := a.memoryProvider.TotalMemory()
	if totalErr != nil {
		return a.failMemoryCheck(ctx, isvc,
			fmt.Sprintf("memory budget check failed: failed to get total memory: %v", totalErr))
	}
	hostBudget := uint64(float64(total) * a.memoryFraction)
	memoryBudgetBytes.Set(float64(hostBudget))

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
	default: // BudgetModeFraction
		var budgetErr error
		budget, budgetErr = CheckMemoryBudget(a.memoryProvider, estimate, resolved.Fraction)
		if budgetErr != nil {
			return a.failMemoryCheck(ctx, isvc, fmt.Sprintf("memory budget check failed: %v", budgetErr))
		}
	}

	// A Model's memoryBudget or memoryFraction may lower this Mac's limit for
	// itself but not raise it, unless the agent allows overrides (#2080).
	clamped := false
	if budget.BudgetBytes > hostBudget && !a.config.AllowModelMemoryBudgetOverride {
		a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
			corev1.EventTypeWarning, EventReasonMemoryBudgetClamped,
			"Model %s budget %s (%s) exceeds this Mac's budget %s (%s total * %.0f%%); clamped to it. "+
				"Start the metal-agent with --allow-model-memory-budget-override to allow it",
			model.Name, formatMemory(budget.BudgetBytes), resolved.Source,
			formatMemory(hostBudget), formatMemory(total), a.memoryFraction*100,
		)
		budget = CheckMemoryBudgetAbsolute(hostBudget, estimate)
		clamped = true
	}

	if !budget.Fits {
		var msg string
		switch {
		case clamped:
			msg = fmt.Sprintf("estimated %s required, budget %s (the Model's budget, clamped to this Mac's: %s total * %.0f%%)",
				formatMemory(budget.EstimateBytes),
				formatMemory(budget.BudgetBytes),
				formatMemory(total),
				a.memoryFraction*100,
			)
		case resolved.Mode == BudgetModeAbsolute:
			msg = fmt.Sprintf("estimated %s required, budget %s (absolute from CRD)",
				formatMemory(budget.EstimateBytes),
				formatMemory(budget.BudgetBytes),
			)
		default:
			msg = fmt.Sprintf("estimated %s required, budget %s (%s total * %.0f%%)",
				formatMemory(budget.EstimateBytes),
				formatMemory(budget.BudgetBytes),
				formatMemory(budget.TotalBytes),
				resolved.Fraction*100,
			)
		}
		return a.refuseInsufficientMemory(ctx, isvc, msg, "source", resolved.Source)
	}

	// The model fits on its own; it must also fit next to every running and
	// starting process. The check and the reservation share one critical
	// section, so two services starting at once cannot both claim the same
	// free budget. A respawn is checked against everything but its own key:
	// its previous engine is already stopped. An override-raised Model budget
	// raises the shared limit for this admission too.
	key := types.NamespacedName{Namespace: isvc.Namespace, Name: isvc.Name}.String()
	capacity := max(hostBudget, budget.BudgetBytes)
	a.mu.Lock()
	committed, holders := a.committedMemoryLocked(key)
	if committed+estimate.TotalBytes > capacity {
		a.mu.Unlock()
		var free uint64
		if committed < capacity {
			free = capacity - committed
		}
		msg := fmt.Sprintf("estimated %s required, %s of the %s budget already committed to %s, %s free",
			formatMemory(estimate.TotalBytes),
			formatMemory(committed),
			formatMemory(capacity),
			describeMemoryHolders(holders),
			formatMemory(free),
		)
		return a.refuseInsufficientMemory(ctx, isvc, msg, "committed", formatMemory(committed))
	}
	a.reservedMemory[key] = estimate.TotalBytes
	a.updateCommittedMetricLocked()
	a.mu.Unlock()

	a.logger.Infow("memory check passed",
		"estimate", formatMemory(budget.EstimateBytes),
		"budget", formatMemory(budget.BudgetBytes),
		"headroom", formatMemory(budget.HeadroomBytes),
		"committed", formatMemory(committed),
		"source", resolved.Source,
	)

	// Clear any stale scheduling status from a previous failed check.
	if isvc.Status.SchedulingStatus != "" || isvc.Status.SchedulingMessage != "" {
		isvc.Status.SchedulingStatus = ""
		isvc.Status.SchedulingMessage = ""
		if updateErr := a.config.K8sClient.Status().Update(ctx, isvc); updateErr != nil {
			a.logger.Warnw("failed to clear scheduling status", "error", updateErr)
		}
	}

	return nil
}

// refuseInsufficientMemory refuses a start that does not fit: it records msg on
// the InferenceService status, emits a Warning event with it, and returns the
// error the caller hands back to the reconcile loop.
func (a *MetalAgent) refuseInsufficientMemory(
	ctx context.Context,
	isvc *inferencev1alpha1.InferenceService,
	msg string,
	logKV ...interface{},
) error {
	a.logger.Warnw("model does not fit in memory budget",
		append([]interface{}{"namespace", isvc.Namespace, "name", isvc.Name, "reason", msg}, logKV...)...)
	isvc.Status.SchedulingStatus = EventReasonInsufficientMemory
	isvc.Status.SchedulingMessage = msg
	if updateErr := a.config.K8sClient.Status().Update(ctx, isvc); updateErr != nil {
		a.logger.Warnw("failed to update InferenceService status", "error", updateErr)
	}
	a.emitInferenceEvent(ctx, &ManagedProcess{Namespace: isvc.Namespace, Name: isvc.Name},
		corev1.EventTypeWarning, EventReasonInsufficientMemory, "Refusing to start: %s", msg)
	return fmt.Errorf("insufficient memory: %s", msg)
}

// committedMemoryLocked sums the admitted estimates of running processes and
// of starts holding a reservation, skipping exclude, and returns the keys that
// hold memory, sorted. Caller holds a.mu.
func (a *MetalAgent) committedMemoryLocked(exclude string) (uint64, []string) {
	perKey := make(map[string]uint64, len(a.processes)+len(a.reservedMemory))
	for k, p := range a.processes {
		perKey[k] += p.AdmittedBytes
	}
	for k, b := range a.reservedMemory {
		perKey[k] += b
	}
	var sum uint64
	holders := make([]string, 0, len(perKey))
	for k, b := range perKey {
		if k == exclude || b == 0 {
			continue
		}
		sum += b
		holders = append(holders, k)
	}
	sort.Strings(holders)
	return sum, holders
}

// updateCommittedMetricLocked publishes the committed total. Caller holds a.mu.
func (a *MetalAgent) updateCommittedMetricLocked() {
	committed, _ := a.committedMemoryLocked("")
	memoryCommittedBytes.Set(float64(committed))
}

// releaseAdmission drops key's admission reservation, if it still holds one.
// The start path defers it, so a start that fails or returns early after
// admission gives its budget back; a successful start has already moved the
// reservation onto its process record, which makes this a no-op.
func (a *MetalAgent) releaseAdmission(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, held := a.reservedMemory[key]; !held {
		return
	}
	delete(a.reservedMemory, key)
	a.updateCommittedMetricLocked()
}

// describeMemoryHolders renders the services holding memory for a refusal
// message: "2 services (default/a, default/b)".
func describeMemoryHolders(holders []string) string {
	if len(holders) == 1 {
		return "1 service (" + holders[0] + ")"
	}
	return fmt.Sprintf("%d services (%s)", len(holders), strings.Join(holders, ", "))
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
	isvc.Status.SchedulingStatus = EventReasonMemoryCheckFailed
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

	// A local source (absolute path or file:// URI, per isLocalModelSource)
	// is loaded in place by the host agent (the Metal path) and is never
	// copied into the model store, so the model-store lookup below would miss
	// it. Size it from the source path directly. A local source that is
	// missing, unreadable, or relative is not an error here: fall back to the
	// status/remote chain like any other unsized source; the executor's
	// resolveLocalModelSource fails the load with a precise message if the
	// path really does not exist.
	if isLocalModelSource(model.Spec.Source) {
		src := localSourcePath(model.Spec.Source)
		if !filepath.IsAbs(src) {
			reasons = append(reasons, fmt.Sprintf("local source %q is not an absolute path", src))
		} else if size, err := localModelSize(src); err == nil {
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
		probeClient := safehttp.NewClient(a.downloadAllow, 0, downloadHostsFlag)
		size, err := remoteModelSize(ctx, probeClient, model.Spec.Source)
		probeClient.CloseIdleConnections()
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("remote size probe failed: %v", err))
		} else {
			fileSizeBytes = size
		}
	}

	// s3:// sources need the signed, credential-bearing client, so they are
	// probed through the same executor the download uses rather than the
	// plain client above.
	if fileSizeBytes == 0 && isS3Source(model.Spec.Source) {
		size, err := a.s3SizeProbe(ctx, model)
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

	var layerCount, embeddingSize, headCount, headCountKV, keyLength, valueLength uint64
	if model.Status.GGUF != nil {
		layerCount = model.Status.GGUF.LayerCount
		embeddingSize = model.Status.GGUF.EmbeddingSize
		headCount = model.Status.GGUF.HeadCount
		headCountKV = model.Status.GGUF.HeadCountKV
		keyLength = model.Status.GGUF.KeyLength
		valueLength = model.Status.GGUF.ValueLength
	}

	return EstimateModelMemoryWithOptions(fileSizeBytes, layerCount, embeddingSize, contextSize, EstimateOptions{
		CacheTypeK:  cacheTypeK,
		CacheTypeV:  cacheTypeV,
		HeadCount:   headCount,
		HeadCountKV: headCountKV,
		KeyLength:   keyLength,
		ValueLength: valueLength,
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
//
// Runtime-arg parity: this hash must cover every field that buildExecutorConfig
// feeds into buildLlamaServerArgs. If a new field is added to one side and the
// other side is updated to match (see the "Runtime-arg parity" subsection in
// AGENTS.md), it must also be included here — otherwise a spec change would not
// be detected and the process would keep running with stale flags.
func computeSpecHash(isvc *inferencev1alpha1.InferenceService) string {
	if isvc == nil {
		return ""
	}
	// Fields included MUST match what the executor actually consumes (or what
	// it will consume once #349 closes the ExecutorConfig gap). When adding a
	// new spec field that affects llama-server args, add it here too.
	//
	// ModelRef is hashed because it also derives the --alias served model name
	// (servedModelName); that derivation needs no separate entry, and a renamed
	// Model is a new object rather than a spec change.
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
		Mode                   string
		Replicas               *int32
		Suspend                bool
		Runtime                string
		TurboQuantBits         *int32
		PagedSSDCacheDir       *string
		HotCacheMaxSize        *string
		PagedSSDCacheMaxSize   *string
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
		Mode:                   isvc.Spec.Mode,
		Replicas:               isvc.Spec.Replicas,
		Suspend:                isvc.Spec.Suspend,
		Runtime:                isvc.Spec.Runtime,
		TurboQuantBits:         isvc.Spec.TurboQuantBits,
		PagedSSDCacheDir:       isvc.Spec.PagedSSDCacheDir,
		HotCacheMaxSize:        isvc.Spec.HotCacheMaxSize,
		PagedSSDCacheMaxSize:   isvc.Spec.PagedSSDCacheMaxSize,
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
