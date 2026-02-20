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
	"syscall"
	"time"

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
	Namespace      string
	ModelStorePath string
	LlamaServerBin string
	Port           int
	LogLevel       string
	HostIP         string
}

func main() {
	cfg := &AgentConfig{}

	// Parse command-line flags
	flag.StringVar(&cfg.Namespace, "namespace", "default", "Kubernetes namespace to watch")
	flag.StringVar(&cfg.ModelStorePath, "model-store", "/tmp/llmkube-models", "Path to store downloaded models")
	flag.StringVar(&cfg.LlamaServerBin, "llama-server", "/usr/local/bin/llama-server", "Path to llama-server binary")
	flag.IntVar(&cfg.Port, "port", 9090, "Agent metrics/health port")
	flag.StringVar(&cfg.LogLevel, "log-level", "info", "Log level (debug, info, warn, error)")
	flag.StringVar(&cfg.HostIP, "host-ip", "", "IP address to register in Kubernetes endpoints (auto-detected if empty)")
	showVersion := flag.Bool("version", false, "Show version information")
	flag.Parse()

	if *showVersion {
		fmt.Printf("llmkube-metal-agent version %s\n", Version)
		fmt.Printf("  git commit: %s\n", GitCommit)
		fmt.Printf("  build date: %s\n", BuildDate)
		os.Exit(0)
	}

	// Print startup banner
	fmt.Printf("🚀 LLMKube Metal Agent v%s\n", Version)
	fmt.Println("═══════════════════════════════════════════════")
	fmt.Printf("Namespace:       %s\n", cfg.Namespace)
	fmt.Printf("Model Store:     %s\n", cfg.ModelStorePath)
	fmt.Printf("Llama Server:    %s\n", cfg.LlamaServerBin)
	fmt.Printf("Agent Port:      %d\n", cfg.Port)
	if cfg.HostIP != "" {
		fmt.Printf("Host IP:         %s\n", cfg.HostIP)
	} else {
		fmt.Printf("Host IP:         (auto-detect)\n")
	}
	fmt.Println("═══════════════════════════════════════════════")

	// Verify Metal support
	fmt.Println("\n🔍 Checking Metal support...")
	caps := platform.DetectCapabilities()
	if !caps.Metal {
		fmt.Println("❌ ERROR: Metal support not detected on this system")
		fmt.Println("   This agent requires macOS with Apple Silicon (M1/M2/M3/M4)")
		os.Exit(1)
	}
	fmt.Printf("✅ Metal support detected: %s\n", caps.GPUName)
	fmt.Printf("   GPU Cores: %d\n", caps.GPUCores)
	fmt.Printf("   Metal Version: Metal %d\n", caps.MetalVersion)

	// Create model store directory
	if err := os.MkdirAll(cfg.ModelStorePath, 0755); err != nil {
		fmt.Printf("❌ ERROR: Failed to create model store directory: %v\n", err)
		os.Exit(1)
	}

	// Verify llama-server binary exists
	if _, err := os.Stat(cfg.LlamaServerBin); os.IsNotExist(err) {
		fmt.Printf("⚠️  WARNING: llama-server not found at %s\n", cfg.LlamaServerBin)
		fmt.Println("   Install llama.cpp with Metal support:")
		fmt.Println("   brew install llama.cpp")
		os.Exit(1)
	}
	fmt.Printf("✅ llama-server found at %s\n\n", cfg.LlamaServerBin)

	// Get Kubernetes client
	fmt.Println("🔗 Connecting to Kubernetes...")
	k8sConfig, err := config.GetConfig()
	if err != nil {
		fmt.Printf("❌ ERROR: Failed to get kubeconfig: %v\n", err)
		os.Exit(1)
	}

	// Register our custom types
	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		fmt.Printf("❌ ERROR: Failed to add scheme: %v\n", err)
		os.Exit(1)
	}

	k8sClient, err := client.New(k8sConfig, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Printf("❌ ERROR: Failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ Connected to Kubernetes cluster")

	// Create agent
	fmt.Println("🎯 Starting Metal agent...")
	metalAgent := agent.NewMetalAgent(agent.MetalAgentConfig{
		K8sClient:      k8sClient,
		Namespace:      cfg.Namespace,
		ModelStorePath: cfg.ModelStorePath,
		LlamaServerBin: cfg.LlamaServerBin,
		Port:           cfg.Port,
		HostIP:         cfg.HostIP,
	})

	// Setup context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\n\n🛑 Received shutdown signal, cleaning up...")
		cancel()
	}()

	// Start the agent
	fmt.Println("✅ Metal agent started successfully")
	fmt.Println("👀 Watching for InferenceService resources...")

	if err := metalAgent.Start(ctx); err != nil {
		fmt.Printf("❌ ERROR: Agent failed: %v\n", err)
		os.Exit(1)
	}

	// Graceful shutdown
	fmt.Println("👋 Shutting down gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := metalAgent.Shutdown(shutdownCtx); err != nil {
		fmt.Printf("⚠️  WARNING: Shutdown errors: %v\n", err)
	}

	fmt.Println("✅ Metal agent stopped")
}
