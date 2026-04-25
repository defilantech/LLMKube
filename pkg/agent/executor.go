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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
)

type ExecutorConfig struct {
	Name        string
	Namespace   string
	ModelSource string
	ModelName   string
	GPULayers   int32
	ContextSize int
	Jinja       bool

	// FlashAttention enables llama.cpp's --flash-attn flag. On Apple Silicon
	// this is a clear win for long-context agentic workloads (prevents the
	// ~25% decode degradation observed at 4K+ context on Qwen-class models).
	// Defaults true at the agent → executor boundary.
	FlashAttention bool

	// Mlock pins model weights and KV cache so macOS's wired collector cannot
	// evict our Metal GPU buffers under memory pressure. Defaults true.
	Mlock bool

	// Threads sets --threads. Zero means auto-detect from performance core
	// count via detectPerfCoreCount(); a non-positive detection result causes
	// the flag to be omitted (let llama-server pick).
	Threads int

	// BatchSize sets --batch-size. Zero falls back to 2048, which prompt
	// processing benchmarks on M-series chips treat as a sweet spot.
	BatchSize int

	// UBatchSize sets --ubatch-size. Zero omits the flag (use llama-server's
	// own default).
	UBatchSize int
}

// ProcessExecutor is the interface that both llama-server and oMLX executors
// implement. It abstracts process lifecycle so the agent is runtime-agnostic.
type ProcessExecutor interface {
	StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error)
	StopProcess(pid int) error
}

type MetalExecutor struct {
	llamaServerBin string
	modelStorePath string
	logger         *zap.SugaredLogger
}

func NewMetalExecutor(llamaServerBin, modelStorePath string, logger *zap.SugaredLogger) *MetalExecutor {
	return &MetalExecutor{
		llamaServerBin: llamaServerBin,
		modelStorePath: modelStorePath,
		logger:         logger,
	}
}

func (e *MetalExecutor) StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error) {
	modelPath, err := e.ensureModel(ctx, config.ModelSource, config.ModelName)
	if err != nil {
		return nil, fmt.Errorf("failed to ensure model: %w", err)
	}

	port, err := e.allocatePort()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate port: %w", err)
	}

	args := buildLlamaServerArgs(modelPath, port, config)

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
		if stopErr := e.StopProcess(process.PID); stopErr != nil {
			e.logger.Warnw("failed to stop unhealthy process after health check failure",
				"pid", process.PID, "port", port, "error", stopErr)
		}
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
		e.logger.Debugw("model already downloaded", "path", localPath)
		return localPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return "", fmt.Errorf("failed to create model directory: %w", err)
	}

	e.logger.Infow("downloading model", "source", source, "destination", localPath)
	if err := e.downloadFile(ctx, source, localPath); err != nil {
		return "", fmt.Errorf("failed to download model: %w", err)
	}

	e.logger.Infow("model downloaded", "path", localPath)
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

// buildLlamaServerArgs constructs the command-line argument vector for the
// llama-server child process. It is split out from StartProcess so it can be
// unit tested without spawning a real process and so the Apple-Silicon-specific
// optimizations are inspectable in one place.
func buildLlamaServerArgs(modelPath string, port int, config ExecutorConfig) []string {
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

	if config.FlashAttention {
		args = append(args, "--flash-attn", "on")
	}

	if config.Mlock {
		args = append(args, "--mlock")
	}

	threads := config.Threads
	if threads == 0 {
		threads = detectPerfCoreCount()
	}
	if threads > 0 {
		args = append(args, "--threads", fmt.Sprintf("%d", threads))
	}

	batchSize := config.BatchSize
	if batchSize == 0 {
		batchSize = 2048
	}
	args = append(args, "--batch-size", fmt.Sprintf("%d", batchSize))

	if config.UBatchSize > 0 {
		args = append(args, "--ubatch-size", fmt.Sprintf("%d", config.UBatchSize))
	}

	if config.Jinja {
		args = append(args, "--jinja")
	}

	return args
}

// allocatePort asks the kernel for an unused TCP port by binding to
// "127.0.0.1:0" and immediately closing the listener. The returned port
// is guaranteed free at the moment of the call; there is a small TOCTOU
// window before llama-server binds on the same port. For the Metal
// executor that window is microseconds since we exec the child process
// synchronously, so a collision is vanishingly unlikely in practice.
func (e *MetalExecutor) allocatePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
