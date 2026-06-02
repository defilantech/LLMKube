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
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/internal/platform"
	"github.com/defilantech/llmkube/pkg/agent"
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
	Port                      int
	ClientPort                int
	LogLevel                  string
	HostIP                    string
	MemoryFraction            float64
	WatchdogInterval          time.Duration
	MemoryPressureWarning     float64
	MemoryPressureCritical    float64
	EvictionEnabled           bool
	MaxWatchFailures          int
	InferenceServiceAllowlist string
	LlamaServerStartupTimeout time.Duration
	OMLXStartupTimeout        time.Duration
	VLLMSwiftStartupTimeout   time.Duration
	MLXServerStartupTimeout   time.Duration
	ApplePowerEnabled         bool
	ApplePowerInterval        time.Duration
	PowermetricsBin           string
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

func main() {
	cfg := &AgentConfig{}

	// Parse command-line flags
	var llamaServerFlag string
	flag.StringVar(&cfg.Namespace, "namespace", "default", "Kubernetes namespace to watch")
	flag.StringVar(&cfg.ModelStorePath, "model-store", "/tmp/llmkube-models", "Path to store downloaded models")
	flag.StringVar(&llamaServerFlag, "llama-server", "", "Path to llama-server binary (auto-detected if not set)")
	flag.IntVar(&cfg.LlamaServerPort, "llama-server-port", 0,
		"Fixed port for the llama-server runtime. 0 (default) allocates an "+
			"ephemeral port per process; set a fixed port for stable native "+
			"clients (e.g. an OpenAI-compatible tool pointed at localhost).")
	flag.StringVar(&cfg.Runtime, "runtime", "llama-server",
		"Inference runtime: llama-server, omlx, ollama, vllm-swift, or mlx-server")
	flag.StringVar(&cfg.OMLXBin, "omlx-bin", "", "Path to omlx binary (auto-detected if not set)")
	flag.IntVar(&cfg.OMLXPort, "omlx-port", 8000, "Port for oMLX server")
	flag.IntVar(&cfg.OllamaPort, "ollama-port", 11434, "Port for Ollama server")
	flag.StringVar(&cfg.VLLMSwiftBin, "vllm-swift-bin", "", "Path to vllm-swift binary (auto-detected if not set)")
	flag.StringVar(&cfg.MLXServerBin, "mlx-server-bin", "", "Path to mlx-server binary (auto-detected if not set)")
	flag.IntVar(&cfg.MLXServerPort, "mlx-server-port", 8080, "Fixed port for the mlx-server runtime")
	flag.IntVar(&cfg.Port, "port", 9090, "Agent metrics/health port")
	flag.IntVar(&cfg.ClientPort, "client-port", 9999,
		"Stable host-side listener (127.0.0.1:<port>) that forwards /v1/* to the current inference child; 0 disables")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&cfg.HostIP, "host-ip", "", "IP address to register in Kubernetes endpoints (auto-detected if empty)")
	flag.Float64Var(&cfg.MemoryFraction, "memory-fraction", 0,
		"Fraction of system memory to budget for models (0 = auto-detect based on total RAM)")
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
	flag.BoolVar(&cfg.ApplePowerEnabled, "apple-power-enabled", false,
		"Enable the macOS powermetrics sampler that publishes apple_power_*_watts gauges "+
			"for InferCost. Requires a NOPASSWD sudoers entry for /usr/bin/powermetrics; "+
			"see deployment/macos/sudoers.d/llmkube-powermetrics. Darwin only.")
	flag.DurationVar(&cfg.ApplePowerInterval, "apple-power-interval",
		agent.DefaultApplePowerInterval,
		"powermetrics sampling cadence. Only meaningful with --apple-power-enabled.")
	flag.StringVar(&cfg.PowermetricsBin, "powermetrics-bin", agent.DefaultPowermetricsBin,
		"Path to the macOS powermetrics binary. Only used with --apple-power-enabled.")
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

	// TODO: Wire this logger into controller-runtime via ctrl.SetLogger(...) so
	// Kubernetes client/controller-runtime logs share the same configuration.

	// Resolve runtime-specific binary paths
	switch cfg.Runtime {
	case "omlx":
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
	case "ollama":
		// Ollama manages itself — no binary resolution needed.
		// The agent will check if Ollama is running at startup via health check.
		logger.Infow("using Ollama runtime", "port", cfg.OllamaPort)
	case "vllm-swift":
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
	case "mlx-server":
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
	default:
		cfg.Runtime = "llama-server"
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

	// Create model store directory
	if err := os.MkdirAll(cfg.ModelStorePath, 0755); err != nil {
		logger.Errorw("failed to create model store directory", "path", cfg.ModelStorePath, "error", err)
		os.Exit(1)
	}
	switch cfg.Runtime {
	case "omlx":
		logger.Infow("omlx binary found", "path", cfg.OMLXBin)
	case "ollama":
		logger.Infow("using Ollama daemon", "port", cfg.OllamaPort)
	case "vllm-swift":
		logger.Infow("vllm-swift binary found", "path", cfg.VLLMSwiftBin)
	case "mlx-server":
		logger.Infow("mlx-server binary found", "path", cfg.MLXServerBin)
	default:
		logger.Infow("llama-server binary found", "path", cfg.LlamaServerBin)
	}

	// Get Kubernetes client
	logger.Infow("connecting to Kubernetes")
	k8sConfig, err := config.GetConfig()
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
	logger.Infow("connected to Kubernetes cluster")

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
		OMLXBin:                   cfg.OMLXBin,
		OMLXPort:                  cfg.OMLXPort,
		OllamaPort:                cfg.OllamaPort,
		VLLMSwiftBin:              cfg.VLLMSwiftBin,
		MLXServerBin:              cfg.MLXServerBin,
		MLXServerPort:             cfg.MLXServerPort,
		Port:                      cfg.Port,
		ClientPort:                cfg.ClientPort,
		HostIP:                    cfg.HostIP,
		Logger:                    logger,
		MemoryFraction:            cfg.MemoryFraction,
		MaxWatchFailures:          cfg.MaxWatchFailures,
		InferenceServiceAllowlist: splitCSV(cfg.InferenceServiceAllowlist),
		LlamaServerStartupTimeout: cfg.LlamaServerStartupTimeout,
		OMLXStartupTimeout:        cfg.OMLXStartupTimeout,
		VLLMSwiftStartupTimeout:   cfg.VLLMSwiftStartupTimeout,
		MLXServerStartupTimeout:   cfg.MLXServerStartupTimeout,
		ApplePowerEnabled:         cfg.ApplePowerEnabled,
		ApplePowerInterval:        cfg.ApplePowerInterval,
		PowermetricsBin:           cfg.PowermetricsBin,
		EvictionEnabled:           cfg.EvictionEnabled,
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
