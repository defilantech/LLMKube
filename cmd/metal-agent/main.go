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

package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/platform"
	"github.com/defilantech/llmkube/pkg/agent"
	"github.com/defilantech/llmkube/pkg/agent/policy"
)

var (
	// Version information (set during build)
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

type AgentConfig struct {
	Namespace                 string
	ModelStorePath            string
	LlamaServerBin            string
	LlamaServerPort           int
	Runtime                   string
	OMLXBin                   string
	OMLXPort                  int
	OllamaPort                int
	VLLMSwiftBin              string
	MLXServerBin              string
	MLXServerPort             int
	TensorFoldBin             string
	Port                      int
	ClientPort                int
	LogLevel                  string
	HostIP                    string
	MemoryFraction            float64
	MemoryCheckMode           string
	WatchdogInterval          time.Duration
	MemoryPressureWarning     float64
	MemoryPressureCritical    float64
	EvictionEnabled           bool
	MaxWatchFailures          int
	HealthFailureThreshold    int
	InferenceServiceAllowlist string
	LlamaServerStartupTimeout time.Duration
	OMLXStartupTimeout        time.Duration
	VLLMSwiftStartupTimeout   time.Duration
	MLXServerStartupTimeout   time.Duration
	TensorFoldStartupTimeout  time.Duration
	ApplePowerEnabled         bool
	ApplePowerInterval        time.Duration
	PowermetricsBin           string
	AllowedModelRoots         string
	AllowedDownloadHosts      string
	MaxDownloadSize           string
	AllowUnsafeExtraArgs      bool
	IngressPort               int
	StateDir                  string
	LegacyDirectEndpoints     bool

	// AllowModelMemoryBudgetOverride: see agent.MetalAgentConfig.
	AllowModelMemoryBudgetOverride bool
}

// splitCSV parses a comma-separated string into a trimmed []string,
// dropping empties. Returns nil for an empty input so callers can
// `if len(...) > 0` cleanly.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// loadKubeconfig defers to controller-runtime's standard discovery chain:
// the auto-registered --kubeconfig flag, then $KUBECONFIG, then in-cluster,
// then ~/.kube/config. A non-empty contextName selects that context instead
// of the kubeconfig's current one, so the agent stays on its cluster when the
// operator switches kubectl contexts.
func loadKubeconfig(contextName string) (*restclient.Config, error) {
	cfg, err := config.GetConfigWithContext(contextName)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	return cfg, nil
}

func parseLogLevel(level string) zapcore.Level {
	switch strings.ToLower(level) {
	case "debug":
		return zapcore.DebugLevel
	case "warn", "warning":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

func newLogger(level string) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(parseLogLevel(level))
	return cfg.Build()
}

// defaultLlamaServerPaths is the list of paths to search for llama-server,
// in order of preference. Apple Silicon Homebrew installs to /opt/homebrew/bin,
// Intel Homebrew installs to /usr/local/bin.
var defaultLlamaServerPaths = []string{
	"/opt/homebrew/bin/llama-server",
	"/usr/local/bin/llama-server",
}

// statFunc is the function used to check file existence (overridden in tests).
var statFunc = os.Stat

// resolveLlamaServerBin returns the llama-server binary path. If override is
// non-empty, it is returned as-is. Otherwise the function searches
// defaultLlamaServerPaths and returns the first one that exists.
func resolveLlamaServerBin(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	for _, p := range defaultLlamaServerPaths {
		if _, err := statFunc(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"llama-server not found in default paths (%v); "+
			"install with: brew install llama.cpp, or pass --llama-server=/path/to/binary",
		defaultLlamaServerPaths)
}

// defaultOMLXPaths is the list of paths to search for the omlx binary.
var defaultOMLXPaths = []string{
	"/opt/homebrew/opt/omlx/bin/omlx",
	"/usr/local/opt/omlx/bin/omlx",
	"/opt/homebrew/bin/omlx",
	"/usr/local/bin/omlx",
}

// defaultVLLMSwiftPaths is the list of paths to search for the vllm-swift binary.
var defaultVLLMSwiftPaths = []string{
	"/opt/homebrew/bin/vllm-swift",
	"/usr/local/bin/vllm-swift",
}

// resolveVLLMSwiftBin returns the vllm-swift binary path. If override is
// non-empty it is returned as-is. Otherwise the function searches
// defaultVLLMSwiftPaths.
func resolveVLLMSwiftBin(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	for _, p := range defaultVLLMSwiftPaths {
		if _, err := statFunc(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"vllm-swift binary not found in default paths (%v); "+
			"install with: brew tap TheTom/tap && brew install vllm-swift, or pass --vllm-swift-bin=/path/to/binary",
		defaultVLLMSwiftPaths)
}

// defaultMLXServerPaths is the list of paths to search for the mlx-server
// binary. These are the Homebrew prefixes; `brew install
// defilantech/tap/mlx-server` installs the shim into one of them.
var defaultMLXServerPaths = []string{
	"/opt/homebrew/bin/mlx-server",
	"/usr/local/bin/mlx-server",
}

// resolveMLXServerBin returns the mlx-server binary path. If override is
// non-empty it is returned as-is. Otherwise the function searches
// defaultMLXServerPaths.
func resolveMLXServerBin(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	for _, p := range defaultMLXServerPaths {
		if _, err := statFunc(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"mlx-server binary not found in default paths (%v); "+
			"install with: brew install defilantech/tap/mlx-server, "+
			"or pass --mlx-server-bin=/path/to/mlx-server",
		defaultMLXServerPaths)
}

// defaultTensorFoldPaths is the list of paths to search for the tensorfold
// binary. TensorFold is a Python CLI; `uv tool install` (the documented,
// pinned install) puts its shim in ~/.local/bin. A leading "~/" is expanded
// against the agent user's home directory.
var defaultTensorFoldPaths = []string{
	"~/.local/bin/tensorfold",
	"/opt/homebrew/bin/tensorfold",
	"/usr/local/bin/tensorfold",
}

// userHomeDir is os.UserHomeDir, overridden in tests.
var userHomeDir = os.UserHomeDir

// resolveModelStorePath returns the absolute model store path: the default
// "~/Library/Application Support/llmkube/models" under the agent user's home
// when the flag is empty (beside the --state-dir default, out of the
// world-writable /tmp the store used to default to), otherwise the flag with
// a leading "~" or "~/" resolved against home exactly as --state-dir does
// (launchd passes "~" literally). "~user" is not expanded. Anything still
// relative is refused: it would resolve against the agent's working
// directory rather than a directory the operator chose.
func resolveModelStorePath(flagValue string) (string, error) {
	if flagValue == "" {
		home, err := modelStoreHome("--model-store is not set")
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "llmkube", "models"), nil
	}
	path := flagValue
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := modelStoreHome(fmt.Sprintf("--model-store %q starts with ~", flagValue))
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("--model-store %q must be an absolute path (or start with ~/)", flagValue)
	}
	return filepath.Clean(path), nil
}

// modelStoreHome returns the agent user's home directory for resolving the
// model store, or an error that starts with why it was needed.
func modelStoreHome(why string) (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("%s but the home directory is unknown: %w", why, err)
	}
	if home == "" {
		return "", fmt.Errorf("%s but the home directory is unknown", why)
	}
	return home, nil
}

// prepareModelStore creates the model store if it is missing (0700, and 0700
// for any parent it has to create), refuses it unless the agent owns it and
// no other user can write it or replace it (agent.ResolveModelStore), and
// returns the resolved, symlink-free path the agent must use from then on.
// An existing store keeps its mode; the check alone decides whether it is
// safe.
func prepareModelStore(path string) (string, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", fmt.Errorf("create model store %s: %w", path, err)
	}
	return agent.ResolveModelStore(path)
}

// configureModelStore resolves --model-store, prepares and checks the store,
// and pins cfg.ModelStorePath to the checked directory. It must run before
// anything else uses the store: the allowed-roots validation, NewMetalAgent
// (which makes the store an allowed root) and the executors all read
// cfg.ModelStorePath, and pinning the resolved path means a symlink in the
// configured path cannot be repointed after the check.
func configureModelStore(cfg *AgentConfig) error {
	path, err := resolveModelStorePath(cfg.ModelStorePath)
	if err != nil {
		return err
	}
	resolved, err := prepareModelStore(path)
	if err != nil {
		return err
	}
	cfg.ModelStorePath = resolved
	return nil
}

// resolveMaxDownloadBytes turns --max-download-size into the byte cap the
// agent enforces on each Model download. Empty means the host's total
// physical memory, read through totalMemory: the agent wires model weights
// into memory and memory admission requires the model to fit, so a single
// file larger than RAM is never one it could serve. "0" disables the cap.
// Anything else is a Kubernetes quantity or a plain byte count (128Gi, 200G,
// 137438953472). An unknown or zero total memory is an error rather than a
// silent fallback to no cap.
func resolveMaxDownloadBytes(flagValue string, totalMemory func() (uint64, error)) (int64, error) {
	if flagValue == "" {
		total, err := totalMemory()
		if err != nil {
			return 0, fmt.Errorf("--max-download-size is not set and its default, "+
				"the host's total memory, is unknown (set it explicitly): %w", err)
		}
		if total == 0 {
			return 0, fmt.Errorf("--max-download-size is not set and its default, " +
				"the host's total memory, reads as 0 (set it explicitly)")
		}
		return int64(min(total, math.MaxInt64)), nil //nolint:gosec // G115: clamped to MaxInt64
	}
	qty, err := resource.ParseQuantity(flagValue)
	if err != nil {
		return 0, fmt.Errorf("--max-download-size %q is not a byte count or a quantity such as 128Gi: %w",
			flagValue, err)
	}
	n, ok := qty.AsInt64()
	if !ok || n < 0 {
		return 0, fmt.Errorf("--max-download-size %q must be a whole, non-negative number of bytes", flagValue)
	}
	return n, nil
}

// resolveTensorFoldBin returns the tensorfold binary path. If override is
// non-empty it is returned as-is. Otherwise the function searches
// defaultTensorFoldPaths.
func resolveTensorFoldBin(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	home, homeErr := userHomeDir()
	for _, p := range defaultTensorFoldPaths {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			if homeErr != nil {
				continue
			}
			p = filepath.Join(home, rest)
		}
		if _, err := statFunc(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"tensorfold binary not found in default paths (%v); "+
			"install with: uv tool install --python 3.12 "+
			"\"git+https://github.com/ashhart/TensorFold.git@v0.3.4.1\", "+
			"or pass --tensorfold-bin=/path/to/tensorfold",
		defaultTensorFoldPaths)
}

// resolveOMLXBin returns the omlx binary path. If override is non-empty it is
// returned as-is. Otherwise the function searches defaultOMLXPaths.
func resolveOMLXBin(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	for _, p := range defaultOMLXPaths {
		if _, err := statFunc(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf(
		"omlx binary not found in default paths (%v); "+
			"install with: brew install jundot/omlx/omlx, or pass --omlx-bin=/path/to/binary",
		defaultOMLXPaths)
}

// resolveOptionalRuntimeBins resolves the binaries of the non-default
// runtimes. Each one is resolved when its binary flag is set or --runtime
// selects it, so one agent can host several runtimes (#525). A runtime that
// was asked for but cannot be found is fatal.
func resolveOptionalRuntimeBins(cfg *AgentConfig, logger *zap.SugaredLogger) {
	if cfg.Runtime == "omlx" || cfg.OMLXBin != "" {
		resolvedBin, err := resolveOMLXBin(cfg.OMLXBin)
		if err != nil {
			logger.Errorw("omlx binary not found",
				"searchPaths", defaultOMLXPaths,
				"installHint", "brew install jundot/omlx/omlx",
				"error", err,
			)
			os.Exit(1)
		}
		cfg.OMLXBin = resolvedBin
	}
	if cfg.Runtime == "ollama" {
		// Ollama manages itself — no binary resolution needed.
		// The agent will check if Ollama is running at startup via health check.
		logger.Infow("using Ollama runtime", "port", cfg.OllamaPort)
	}
	if cfg.Runtime == "vllm-swift" || cfg.VLLMSwiftBin != "" {
		resolvedBin, err := resolveVLLMSwiftBin(cfg.VLLMSwiftBin)
		if err != nil {
			logger.Errorw("vllm-swift binary not found",
				"searchPaths", defaultVLLMSwiftPaths,
				"installHint", "brew tap TheTom/tap && brew install vllm-swift",
				"error", err,
			)
			os.Exit(1)
		}
		cfg.VLLMSwiftBin = resolvedBin
	}
	if cfg.Runtime == "mlx-server" || cfg.MLXServerBin != "" {
		resolvedBin, err := resolveMLXServerBin(cfg.MLXServerBin)
		if err != nil {
			logger.Errorw("mlx-server binary not found",
				"searchPaths", defaultMLXServerPaths,
				"installHint", "brew install defilantech/tap/mlx-server, "+
					"or pass --mlx-server-bin=/path/to/mlx-server",
				"error", err,
			)
			os.Exit(1)
		}
		cfg.MLXServerBin = resolvedBin
	}
	if cfg.Runtime == "tensorfold" || cfg.TensorFoldBin != "" {
		resolvedBin, err := resolveTensorFoldBin(cfg.TensorFoldBin)
		if err != nil {
			logger.Errorw("tensorfold binary not found",
				"searchPaths", defaultTensorFoldPaths,
				"installHint", "uv tool install --python 3.12 "+
					"\"git+https://github.com/ashhart/TensorFold.git@v0.3.4.1\", "+
					"or pass --tensorfold-bin=/path/to/tensorfold",
				"error", err,
			)
			os.Exit(1)
		}
		cfg.TensorFoldBin = resolvedBin
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "stop-engines" {
		os.Exit(runStopEnginesCommand(os.Args[2:]))
	}
	cfg := &AgentConfig{}

	// Parse command-line flags
	var llamaServerFlag, kubeContext string
	flag.StringVar(&kubeContext, "kube-context", "",
		"kubeconfig context to connect with (default: the kubeconfig's current context). "+
			"Pick the kubeconfig file with --kubeconfig or $KUBECONFIG.")
	flag.StringVar(&cfg.Namespace, "namespace", "default", "Kubernetes namespace to watch")
	flag.StringVar(&cfg.ModelStorePath, "model-store", "",
		"Path to store downloaded models: absolute, or starting with ~/. "+
			"Default: ~/Library/Application Support/llmkube/models. The agent refuses a store it does "+
			"not own, that other users can write, or that sits under a directory other users can write.")
	flag.StringVar(&llamaServerFlag, "llama-server", "", "Path to llama-server binary (auto-detected if not set)")
	flag.IntVar(&cfg.LlamaServerPort, "llama-server-port", 0,
		"Fixed port for the llama-server runtime. 0 (default) allocates an "+
			"ephemeral port per process; set a fixed port for stable native "+
			"clients (e.g. an OpenAI-compatible tool pointed at localhost).")
	flag.StringVar(&cfg.Runtime, "runtime", "llama-server",
		"Inference runtime: llama-server, omlx, ollama, vllm-swift, mlx-server, or tensorfold")
	flag.StringVar(&cfg.OMLXBin, "omlx-bin", "", "Path to omlx binary (auto-detected if not set)")
	flag.IntVar(&cfg.OMLXPort, "omlx-port", 8000, "Port for oMLX server")
	flag.IntVar(&cfg.OllamaPort, "ollama-port", 11434, "Port for Ollama server")
	flag.StringVar(&cfg.VLLMSwiftBin, "vllm-swift-bin", "", "Path to vllm-swift binary (auto-detected if not set)")
	flag.StringVar(&cfg.MLXServerBin, "mlx-server-bin", "", "Path to mlx-server binary (auto-detected if not set)")
	flag.IntVar(&cfg.MLXServerPort, "mlx-server-port", 8080, "Fixed port for the mlx-server runtime")
	flag.StringVar(&cfg.TensorFoldBin, "tensorfold-bin", "",
		"Path to tensorfold binary (auto-detected if not set; searched when --runtime tensorfold)")
	flag.IntVar(&cfg.Port, "port", 9090, "Agent metrics/health port")
	flag.IntVar(&cfg.ClientPort, "client-port", 9999,
		"Stable host-side listener (127.0.0.1:<port>) that forwards /v1/* to the current inference child; 0 disables")
	flag.IntVar(&cfg.IngressPort, "ingress-port", 9443,
		"TLS port of the authenticated ingress that in-cluster relays use to reach engines (engines listen on "+
			"127.0.0.1 only). Open this port, not engine ports, in the macOS firewall.")
	flag.StringVar(&cfg.StateDir, "state-dir", "",
		"Directory for agent state (the ingress TLS key). Default: ~/Library/Application Support/llmkube/metal-agent.")
	flag.BoolVar(&cfg.LegacyDirectEndpoints, "legacy-direct-endpoints", false,
		"DEPRECATED, removed in a future release: bind engines on all interfaces without authentication and "+
			"register them directly, as before the ingress. Only for a controller that predates relay support.")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&cfg.HostIP, "host-ip", "", "IP address to register in Kubernetes endpoints (auto-detected if empty)")
	flag.Float64Var(&cfg.MemoryFraction, "memory-fraction", 0,
		"Fraction of system memory to budget for models (0 = auto-detect based on total RAM)")
	flag.StringVar(&cfg.MemoryCheckMode, "memory-check-mode", agent.MemoryCheckModeEnforce,
		"What to do when the pre-flight memory check cannot be completed: "+
			"\"enforce\" refuses to start the process (fail closed), \"warn\" logs and starts it anyway")
	flag.BoolVar(&cfg.AllowModelMemoryBudgetOverride, "allow-model-memory-budget-override", false,
		"Let a Model's spec.hardware.memoryBudget or memoryFraction exceed this Mac's budget "+
			"(total * memory-fraction); by default such a budget is clamped to it with a Warning event")
	flag.DurationVar(&cfg.WatchdogInterval, "memory-watchdog-interval", 10*time.Second,
		"How often to check memory pressure (0 to disable)")
	flag.Float64Var(&cfg.MemoryPressureWarning, "memory-pressure-warning", 0.20,
		"Available memory fraction below which a warning is emitted")
	flag.Float64Var(&cfg.MemoryPressureCritical, "memory-pressure-critical", 0.10,
		"Available memory fraction below which pressure is critical")
	flag.BoolVar(&cfg.EvictionEnabled, "eviction-enabled", false,
		"Enable automatic process eviction under critical memory pressure")
	flag.IntVar(&cfg.MaxWatchFailures, "max-watch-failures", agent.DefaultMaxConsecutiveFailures,
		"Consecutive Kubernetes list failures from the InferenceService watcher before the agent "+
			"gives up and exits for supervisor restart. Set to 0 to use the agent default.")
	flag.IntVar(&cfg.HealthFailureThreshold, "health-failure-threshold", agent.DefaultHealthFailureThreshold,
		"Consecutive failed health probes (one every 30s) before the agent stops an engine and starts "+
			"a new one; restarts that keep failing back off from 10s to 5m. The first failed probe already "+
			"withdraws the engine's endpoint. Set to 0 to use the agent default.")
	flag.StringVar(&cfg.InferenceServiceAllowlist, "inference-service-allowlist", "",
		"Comma-separated list of InferenceService names this agent is permitted to claim. "+
			"Empty (default) claims every metal-accelerator InferenceService in --namespace "+
			"(v0.1 behavior). Set on multi-Mac fleets that share one Kubernetes cluster so "+
			"each node claims only its own InferenceServices instead of racing with peers (#524).")
	flag.DurationVar(&cfg.LlamaServerStartupTimeout, "llama-server-startup-timeout",
		agent.DefaultLlamaServerStartupTimeout,
		"How long to wait for a freshly-spawned llama-server to respond on /health. "+
			"Bump for very large models (mlock + warmup grow with model size).")
	flag.DurationVar(&cfg.OMLXStartupTimeout, "omlx-startup-timeout",
		agent.DefaultOMLXStartupTimeout,
		"How long to wait for the oMLX daemon to become healthy after launching it. "+
			"First model loads on M-series take 30-90s; default 120s.")
	flag.DurationVar(&cfg.VLLMSwiftStartupTimeout, "vllm-swift-startup-timeout",
		agent.DefaultVLLMSwiftStartupTimeout,
		"How long to wait for vllm-swift to respond on /health. vLLM init plus "+
			"Swift bridge load plus weight load grow with model size; default 120s "+
			"works for 30B-class models on M5 Max. Bump for larger models.")
	flag.DurationVar(&cfg.MLXServerStartupTimeout, "mlx-server-startup-timeout",
		agent.DefaultMLXServerStartupTimeout,
		"How long to wait for mlx-server to respond on /health. MLX weight load "+
			"grows with model size; default 120s works for ~35B models on M5 Max.")
	flag.DurationVar(&cfg.TensorFoldStartupTimeout, "tensorfold-startup-timeout",
		agent.DefaultTensorFoldStartupTimeout,
		"How long to wait for tensorfold to respond on /health. A warm load is "+
			"seconds, but a first run compiles kernels and runs an exactness "+
			"self-check; a child that exits ends the wait early.")
	flag.BoolVar(&cfg.ApplePowerEnabled, "apple-power-enabled", false,
		"Enable the macOS powermetrics sampler that publishes apple_power_*_watts gauges "+
			"for InferCost. Requires a NOPASSWD sudoers entry for /usr/bin/powermetrics; "+
			"see deployment/macos/sudoers.d/llmkube-powermetrics. Darwin only.")
	flag.DurationVar(&cfg.ApplePowerInterval, "apple-power-interval",
		agent.DefaultApplePowerInterval,
		"powermetrics sampling cadence. Only meaningful with --apple-power-enabled.")
	flag.StringVar(&cfg.PowermetricsBin, "powermetrics-bin", agent.DefaultPowermetricsBin,
		"Path to the macOS powermetrics binary. Only used with --apple-power-enabled.")
	flag.StringVar(&cfg.AllowedModelRoots, "allowed-model-roots", "",
		"Comma-separated absolute directories that local model sources, the oMLX pagedSSDCacheDir "+
			"and path-valued extraArgs must resolve into (symlinks followed), in addition to the model "+
			"store. Default: the model store only.")
	flag.StringVar(&cfg.AllowedDownloadHosts, "allowed-download-hosts", "",
		"Comma-separated hostnames or CIDRs the agent may download models from even though they "+
			"resolve to a private, loopback or link-local address, e.g. a LAN MinIO. Default: none.")
	flag.StringVar(&cfg.MaxDownloadSize, "max-download-size", "",
		"Cap on the bytes one Model download may write, resumed bytes included, as a byte count or a quantity "+
			"such as 128Gi. A download over it is stopped, its partial removed, and the start refused with "+
			"ModelDownloadTooLarge. Default: the host's total memory (a model file larger than RAM cannot be "+
			"wired). 0 disables the cap.")
	flag.BoolVar(&cfg.AllowUnsafeExtraArgs, "allow-unsafe-extra-args", false,
		"Relax the extraArgs policy (unknown flags, stray tokens, refused non-listener flags, path checks, "+
			"vllm-swift code-loading) for Macs whose InferenceService authors are fully trusted; flags that "+
			"set or move the engine's listener or registration, or start multi-node or distributed backends, stay refused.")
	showVersion := flag.Bool("version", false, "Show version information")
	flag.Parse()

	if *showVersion {
		fmt.Printf("llmkube-metal-agent version %s\n", Version)
		fmt.Printf("  git commit: %s\n", GitCommit)
		fmt.Printf("  build date: %s\n", BuildDate)
		os.Exit(0)
	}

	baseLogger, err := newLogger(cfg.LogLevel)
	if err != nil {
		fmt.Printf("failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = baseLogger.Sync()
	}()
	logger := baseLogger.Sugar()

	// Create, vet and pin the model store before anything uses it.
	if err := configureModelStore(cfg); err != nil {
		logger.Errorw("refusing to use the model store", "model-store", cfg.ModelStorePath, "error", err)
		os.Exit(1)
	}

	// Validate the effective allowed-model-roots set eagerly: the same list
	// NewMetalAgent resolves (the model store plus --allowed-model-roots, see
	// agent.EffectiveModelRoots). A relative --model-store or a relative
	// --allowed-model-roots entry is a config mistake, and failing fast here
	// beats starting an agent that silently refuses every local model path.
	// home is looked up the same way NewMetalAgent looks it up, so a "~"
	// entry validates here exactly as it will resolve at construction time.
	allowedModelRoots := splitCSV(cfg.AllowedModelRoots)
	home, err := os.UserHomeDir()
	if err != nil {
		logger.Warnw("could not determine home directory; a \"~\"-prefixed allowed model root will be refused", "error", err)
	}
	if _, _, err := policy.NewRoots(agent.EffectiveModelRoots(cfg.ModelStorePath, allowedModelRoots), home); err != nil {
		logger.Errorw("invalid allowed model roots (model store plus --allowed-model-roots)", "error", err)
		os.Exit(1)
	}

	// TODO: Wire this logger into controller-runtime via ctrl.SetLogger(...) so
	// Kubernetes client/controller-runtime logs share the same configuration.

	// Resolve runtime-specific binary paths. llama-server is always resolved
	// because it is the default fallback runtime. Other runtimes are resolved
	// when their binary path flag is set or when the agent's --runtime flag
	// selects them, so the agent can host multiple runtimes concurrently
	// (#525).
	{
		resolvedBin, err := resolveLlamaServerBin(llamaServerFlag)
		if err != nil {
			logger.Errorw("llama-server binary not found",
				"searchPaths", defaultLlamaServerPaths,
				"installHint", "brew install llama.cpp",
				"error", err,
			)
			os.Exit(1)
		}
		cfg.LlamaServerBin = resolvedBin
	}
	resolveOptionalRuntimeBins(cfg, logger)

	hostIP := cfg.HostIP
	if hostIP == "" {
		hostIP = "auto-detect"
	}
	logger.Infow("starting metal agent",
		"version", Version,
		"namespace", cfg.Namespace,
		"modelStore", cfg.ModelStorePath,
		"runtime", cfg.Runtime,
		"llamaServerBin", cfg.LlamaServerBin,
		"omlxBin", cfg.OMLXBin,
		"vllmSwiftBin", cfg.VLLMSwiftBin,
		"agentPort", cfg.Port,
		"ingressPort", cfg.IngressPort,
		"legacyDirectEndpoints", cfg.LegacyDirectEndpoints,
		"hostIP", hostIP,
		"logLevel", cfg.LogLevel,
	)

	// Verify Metal support
	logger.Infow("checking Metal support")
	caps := platform.DetectCapabilities()
	if !caps.Metal {
		logger.Errorw("Metal support not detected",
			"requirement", "macOS with Apple Silicon (M1/M2/M3/M4)",
		)
		os.Exit(1)
	}
	logger.Infow("Metal support detected",
		"gpuName", caps.GPUName,
		"gpuCores", caps.GPUCores,
		"metalVersion", caps.MetalVersion,
	)

	// Resolved after the Metal check: its default reads the host's memory
	// through the darwin provider, which is a stub elsewhere, so a non-Mac
	// host should fail on Metal first.
	maxDownloadBytes, err := resolveMaxDownloadBytes(cfg.MaxDownloadSize, (&agent.DarwinMemoryProvider{}).TotalMemory)
	if err != nil {
		logger.Errorw("invalid --max-download-size", "error", err)
		os.Exit(1)
	}
	logger.Infow("model download cap", "maxDownloadBytes", maxDownloadBytes)

	// Log which runtimes are available. llama-server is always available
	// (resolved above). Other runtimes are available when their binary
	// path was resolved successfully.
	logger.Infow("llama-server binary found", "path", cfg.LlamaServerBin)
	if cfg.OMLXBin != "" {
		logger.Infow("omlx binary found", "path", cfg.OMLXBin)
	}
	if cfg.Runtime == "ollama" {
		logger.Infow("using Ollama daemon", "port", cfg.OllamaPort)
	}
	if cfg.VLLMSwiftBin != "" {
		logger.Infow("vllm-swift binary found", "path", cfg.VLLMSwiftBin)
	}
	if cfg.MLXServerBin != "" {
		logger.Infow("mlx-server binary found", "path", cfg.MLXServerBin)
	}
	if cfg.TensorFoldBin != "" {
		logger.Infow("tensorfold binary found", "path", cfg.TensorFoldBin)
	}

	// Get Kubernetes client
	logger.Infow("connecting to Kubernetes", "context", kubeContext)
	k8sConfig, err := loadKubeconfig(kubeContext)
	if err != nil {
		logger.Errorw("failed to get kubeconfig", "error", err)
		os.Exit(1)
	}

	// Register our custom types
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		logger.Errorw("failed to add scheme", "error", err)
		os.Exit(1)
	}

	k8sClient, err := client.New(k8sConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		logger.Errorw("failed to create Kubernetes client", "error", err)
		os.Exit(1)
	}
	logger.Infow("connected to Kubernetes cluster", "host", k8sConfig.Host)

	// EventRecorder feeds operator-facing Kubernetes events on managed
	// InferenceService objects (memory-pressure transitions, evictions,
	// respawn blocks). The controller-runtime client doesn't expose an
	// EventSink, so we build a typed clientset just for the events API and
	// wire it through a shared broadcaster. Closes #390.
	clientset, err := kubernetes.NewForConfig(k8sConfig)
	if err != nil {
		logger.Errorw("failed to create kubernetes clientset for events", "error", err)
		os.Exit(1)
	}
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{
		Interface: clientset.CoreV1().Events(""),
	})
	eventRecorder := eventBroadcaster.NewRecorder(scheme.Scheme,
		corev1.EventSource{Component: "llmkube-metal-agent"})
	defer eventBroadcaster.Shutdown()

	// Create agent
	logger.Infow("creating Metal agent")
	agentCfg := agent.MetalAgentConfig{
		K8sClient:                 k8sClient,
		EventRecorder:             eventRecorder,
		Namespace:                 cfg.Namespace,
		ModelStorePath:            cfg.ModelStorePath,
		LlamaServerBin:            cfg.LlamaServerBin,
		LlamaServerPort:           cfg.LlamaServerPort,
		Runtime:                   cfg.Runtime,
		Version:                   Version,
		OMLXBin:                   cfg.OMLXBin,
		OMLXPort:                  cfg.OMLXPort,
		OllamaPort:                cfg.OllamaPort,
		VLLMSwiftBin:              cfg.VLLMSwiftBin,
		MLXServerBin:              cfg.MLXServerBin,
		MLXServerPort:             cfg.MLXServerPort,
		TensorFoldBin:             cfg.TensorFoldBin,
		Port:                      cfg.Port,
		ClientPort:                cfg.ClientPort,
		HostIP:                    cfg.HostIP,
		Logger:                    logger,
		MemoryFraction:            cfg.MemoryFraction,
		MemoryCheckMode:           cfg.MemoryCheckMode,
		MaxWatchFailures:          cfg.MaxWatchFailures,
		HealthFailureThreshold:    cfg.HealthFailureThreshold,
		InferenceServiceAllowlist: splitCSV(cfg.InferenceServiceAllowlist),
		LlamaServerStartupTimeout: cfg.LlamaServerStartupTimeout,
		OMLXStartupTimeout:        cfg.OMLXStartupTimeout,
		VLLMSwiftStartupTimeout:   cfg.VLLMSwiftStartupTimeout,
		MLXServerStartupTimeout:   cfg.MLXServerStartupTimeout,
		TensorFoldStartupTimeout:  cfg.TensorFoldStartupTimeout,
		ApplePowerEnabled:         cfg.ApplePowerEnabled,
		ApplePowerInterval:        cfg.ApplePowerInterval,
		PowermetricsBin:           cfg.PowermetricsBin,
		EvictionEnabled:           cfg.EvictionEnabled,
		AllowedModelRoots:         allowedModelRoots,
		AllowedDownloadHosts:      splitCSV(cfg.AllowedDownloadHosts),
		MaxDownloadBytes:          maxDownloadBytes,
		AllowUnsafeExtraArgs:      cfg.AllowUnsafeExtraArgs,
		IngressPort:               cfg.IngressPort,
		StateDir:                  cfg.StateDir,
		LegacyDirectEndpoints:     cfg.LegacyDirectEndpoints,

		AllowModelMemoryBudgetOverride: cfg.AllowModelMemoryBudgetOverride,
	}
	if cfg.WatchdogInterval > 0 {
		agentCfg.WatchdogConfig = &agent.MemoryWatchdogConfig{
			Interval:          cfg.WatchdogInterval,
			WarningThreshold:  cfg.MemoryPressureWarning,
			CriticalThreshold: cfg.MemoryPressureCritical,
		}
		logger.Infow("memory watchdog enabled",
			"interval", cfg.WatchdogInterval,
			"warningThreshold", cfg.MemoryPressureWarning,
			"criticalThreshold", cfg.MemoryPressureCritical,
		)
	}
	metalAgent := agent.NewMetalAgent(agentCfg)

	// Setup context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Infow("received shutdown signal; cleaning up")
		cancel()
	}()

	// Start the agent
	logger.Infow("Metal agent started successfully")
	logger.Infow("watching for InferenceService resources")

	if err := metalAgent.Start(ctx); err != nil {
		logger.Errorw("agent failed", "error", err)
		os.Exit(1)
	}

	// Graceful shutdown
	logger.Infow("shutting down gracefully")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := metalAgent.Shutdown(shutdownCtx); err != nil {
		logger.Warnw("shutdown completed with errors", "error", err)
	}

	logger.Infow("Metal agent stopped")
}
