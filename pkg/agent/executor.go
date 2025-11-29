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

type ExecutorConfig struct {
	Name        string
	Namespace   string
	ModelSource string
	ModelName   string
	GPULayers   int32
	ContextSize int
}

type MetalExecutor struct {
	llamaServerBin string
	modelStorePath string
	nextPort       int
}

func NewMetalExecutor(llamaServerBin, modelStorePath string) *MetalExecutor {
	return &MetalExecutor{
		llamaServerBin: llamaServerBin,
		modelStorePath: modelStorePath,
		nextPort:       8080, // TODO: Implement proper port allocation
	}
}

func (e *MetalExecutor) StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error) {
	modelPath, err := e.ensureModel(ctx, config.ModelSource, config.ModelName)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure model: %w", err)
	}

	port := e.allocatePort()

	gpuLayers := config.GPULayers
	if gpuLayers == 0 {
		gpuLayers = 99
	}

	args := []string{
		"--model", modelPath,
		"--host", "0.0.0.0",
		"--port", fmt.Sprintf("%d", port),
		"--n-gpu-layers", fmt.Sprintf("%d", gpuLayers),
		"--ctx-size", fmt.Sprintf("%d", config.ContextSize),
		"--metrics",
	}

	cmd := exec.Command(e.llamaServerBin, args...)

	cmd.Env = append(os.Environ(),
		"GGML_METAL_ENABLE=1",
		"GGML_METAL_PATH_RESOURCES=/usr/local/share/llama.cpp",
	)

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

	if err := e.waitForHealthy(port, 30*time.Second); err != nil {
		_ = e.StopProcess(process.PID)
		return nil, fmt.Errorf("process failed health check: %w", err)
	}

	process.Healthy = true
	return process, nil
}

func (e *MetalExecutor) StopProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process %d: %w", pid, err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send SIGTERM to process %d: %w", pid, err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := process.Wait()
		done <- err
	}()

	select {
	case <-time.After(10 * time.Second):
		_ = process.Kill()
		return fmt.Errorf("process %d did not exit gracefully, killed", pid)
	case err := <-done:
		return err
	}
}

func (e *MetalExecutor) ensureModel(ctx context.Context, source, name string) (string, error) {
	filename := filepath.Base(source)
	localPath := filepath.Join(e.modelStorePath, name, filename)

	if _, err := os.Stat(localPath); err == nil {
		fmt.Printf("📦 Model already downloaded: %s\n", localPath)
		return localPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create model directory: %w", err)
	}

	fmt.Printf("⬇️  Downloading model from %s...\n", source)
	if err := e.downloadFile(ctx, source, localPath); err != nil {
		return "", fmt.Errorf("failed to download model: %w", err)
	}

	fmt.Printf("✅ Model downloaded: %s\n", localPath)
	return localPath, nil
}

func (e *MetalExecutor) downloadFile(ctx context.Context, url, filePath string) error {
	out, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

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

	_, err = io.Copy(out, resp.Body)
	return err
}

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

func (e *MetalExecutor) allocatePort() int {
	port := e.nextPort
	e.nextPort++
	return port
}
