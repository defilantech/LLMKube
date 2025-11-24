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
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// ExecutorConfig contains configuration for starting a process
type ExecutorConfig struct {
	Name        string
	Namespace   string
	ModelSource string
	ModelName   string
	GPULayers   int32
	ContextSize int
}

// MetalExecutor manages llama-server processes with Metal acceleration
type MetalExecutor struct {
	llamaServerBin string
	modelStorePath string
	nextPort       int
}

// NewMetalExecutor creates a new executor
func NewMetalExecutor(llamaServerBin, modelStorePath string) *MetalExecutor {
	return &MetalExecutor{
		llamaServerBin: llamaServerBin,
		modelStorePath: modelStorePath,
		nextPort:       8080, // TODO: Implement proper port allocation
	}
}

// StartProcess downloads the model (if needed) and starts llama-server
func (e *MetalExecutor) StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error) {
	// Download model if not present
	modelPath, err := e.ensureModel(ctx, config.ModelSource, config.ModelName)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure model: %w", err)
	}

	// Allocate port
	port := e.allocatePort()

	// Prepare command
	gpuLayers := config.GPULayers
	if gpuLayers == 0 {
		gpuLayers = 99 // Default: offload all layers
	}

	args := []string{
		"--model", modelPath,
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", port),
		"--n-gpu-layers", fmt.Sprintf("%d", gpuLayers),
		"--ctx-size", fmt.Sprintf("%d", config.ContextSize),
		"--metrics", // Enable Prometheus metrics
	}

	cmd := exec.Command(e.llamaServerBin, args...)

	// Set environment variables for Metal
	cmd.Env = append(os.Environ(),
		"GGML_METAL_ENABLE=1",
		"GGML_METAL_PATH_RESOURCES=/usr/local/share/llama.cpp",
	)

	// Start the process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start llama-server: %w", err)
	}

	process := &ManagedProcess{
		Name:      config.Name,
		Namespace: config.Namespace,
		PID:       cmd.Process.Pid,
		Port:      port,
		ModelPath: modelPath,
		StartedAt: time.Now(),
		Healthy:   false,
	}

	// Wait for health check
	if err := e.waitForHealthy(port, 30*time.Second); err != nil {
		_ = e.StopProcess(process.PID)
		return nil, fmt.Errorf("process failed health check: %w", err)
	}

	process.Healthy = true
	return process, nil
}

// StopProcess stops a running llama-server process
func (e *MetalExecutor) StopProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}

	// Send SIGTERM for graceful shutdown
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}

	// Wait for process to exit (with timeout)
	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()

	select {
	case <-time.After(10 * time.Second):
		// Force kill if graceful shutdown fails
		_ = process.Kill()
		return fmt.Errorf("process %d did not exit gracefully, killed", pid)
	case err := <-done:
		return err
	}
}

// ensureModel downloads the model if not present, returns path to model file
func (e *MetalExecutor) ensureModel(ctx context.Context, source, name string) (string, error) {
	// Generate local path
	filename := filepath.Base(source)
	localPath := filepath.Join(e.modelStorePath, name, filename)

	// Check if already downloaded
	if _, err := os.Stat(localPath); err == nil {
		fmt.Printf("📦 Model already downloaded: %s\n", localPath)
		return localPath, nil
	}

	// Create directory
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create model directory: %w", err)
	}

	// Download model
	fmt.Printf("⬇️  Downloading model from %s...\n", source)
	if err := e.downloadFile(ctx, source, localPath); err != nil {
		return "", fmt.Errorf("failed to download model: %w", err)
	}

	fmt.Printf("✅ Model downloaded: %s\n", localPath)
	return localPath, nil
}

// downloadFile downloads a file from URL to local path
func (e *MetalExecutor) downloadFile(ctx context.Context, url, filePath string) error {
	// Create the file
	out, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	// Create HTTP request with context
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	// Download
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad status: %s", resp.Status)
	}

	// Write to file
	_, err = io.Copy(out, resp.Body)
	return err
}

// waitForHealthy waits for the llama-server to become healthy
func (e *MetalExecutor) waitForHealthy(port int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	healthURL := fmt.Sprintf("http://localhost:%d/health", port)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for health check")
		case <-ticker.C:
			resp, err := http.Get(healthURL)
			if err == nil && resp.StatusCode == http.StatusOK {
				_ = resp.Body.Close()
				return nil
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
		}
	}
}

// allocatePort allocates a new port for a process
func (e *MetalExecutor) allocatePort() int {
	port := e.nextPort
	e.nextPort++
	return port
}
