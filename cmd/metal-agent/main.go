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
	"k8s.io/client-go/kubernetes/scheme"
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
	Namespace              string
	ModelStorePath         string
	LlamaServerBin         string
	Port                   int
	LogLevel               string
	HostIP                 string
	MemoryFraction         float64
	WatchdogInterval       time.Duration
	MemoryPressureWarning  float64
	MemoryPressureCritical float64
	EvictionEnabled        bool
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

func main() {
	cfg := &AgentConfig{}

	// Parse command-line flags
	var llamaServerFlag string
	flag.StringVar(&cfg.Namespace, "namespace", "default", "Kubernetes namespace to watch")
	flag.StringVar(&cfg.ModelStorePath, "model-store", "/tmp/llmkube-models", "Path to store downloaded models")
	flag.StringVar(&llamaServerFlag, "llama-server", "", "Path to llama-server binary (auto-detected if not set)")
	flag.IntVar(&cfg.Port, "port", 9090, "Agent metrics/health port")
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

	// Resolve llama-server binary path
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

	hostIP := cfg.HostIP
	if hostIP == "" {
		hostIP = "auto-detect"
	}
	logger.Infow("starting metal agent",
		"version", Version,
		"namespace", cfg.Namespace,
		"modelStore", cfg.ModelStorePath,
		"llamaServerBin", cfg.LlamaServerBin,
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
	logger.Infow("llama-server binary found", "path", cfg.LlamaServerBin)

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

	// Create agent
	logger.Infow("creating Metal agent")
	agentCfg := agent.MetalAgentConfig{
		K8sClient:      k8sClient,
		Namespace:      cfg.Namespace,
		ModelStorePath: cfg.ModelStorePath,
		LlamaServerBin: cfg.LlamaServerBin,
		Port:           cfg.Port,
		HostIP:         cfg.HostIP,
		Logger:         logger,
		MemoryFraction: cfg.MemoryFraction,
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
