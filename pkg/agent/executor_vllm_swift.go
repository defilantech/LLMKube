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
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// DefaultVLLMSwiftStartupTimeout is how long the agent waits for a freshly
// spawned vllm-swift process to respond on /health. vllm-swift is a thin
// wrapper around vLLM's OpenAI api_server, so startup involves vLLM init,
// the Swift/Metal bridge load, and (for big models) the weight load. On
// real hardware that's 30-90s for a 30B model on M5 Max. 120s gives
// generous headroom while still failing fast on real breakage.
const DefaultVLLMSwiftStartupTimeout = 120 * time.Second

// VLLMSwiftExecutor manages individual vllm-swift processes, one per
// InferenceService. Unlike OMLXExecutor (single shared daemon serving N
// models), each ISvc gets its own vllm-swift process bound to its own port.
// This matches the MetalExecutor (llama-server) lifecycle.
type VLLMSwiftExecutor struct {
	bin            string
	modelStorePath string
	logger         *zap.SugaredLogger
	startupTimeout time.Duration
}

// NewVLLMSwiftExecutor creates an executor that spawns one vllm-swift
// process per InferenceService.
func NewVLLMSwiftExecutor(bin, modelStorePath string, logger *zap.SugaredLogger) *VLLMSwiftExecutor {
	return &VLLMSwiftExecutor{
		bin:            bin,
		modelStorePath: modelStorePath,
		logger:         logger,
		startupTimeout: DefaultVLLMSwiftStartupTimeout,
	}
}

// SetStartupTimeout overrides the default vllm-swift startup timeout.
// Values <= 0 are coerced back to DefaultVLLMSwiftStartupTimeout.
func (e *VLLMSwiftExecutor) SetStartupTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultVLLMSwiftStartupTimeout
	}
	e.startupTimeout = d
}

// StartProcess resolves the model directory, allocates a port, and spawns
// vllm-swift. It blocks until /health returns 200 or startupTimeout fires.
func (e *VLLMSwiftExecutor) StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error) {
	modelPath := e.resolveModelPath(config)
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf(
			"vllm-swift model directory not found at %s: %w (the host metal-agent does not download MLX/HF model directories; pre-download with `vllm-swift download <hf-id>`)",
			modelPath, err)
	}

	port, err := e.allocatePort()
	if err != nil {
		return nil, fmt.Errorf("failed to allocate port: %w", err)
	}

	args := buildVLLMSwiftArgs(modelPath, port, config)

	e.logger.Infow("starting vllm-swift",
		"bin", e.bin, "modelPath", modelPath, "port", port, "ctx", config.ContextSize)

	cmd := exec.Command(e.bin, args...)
	cmd.Env = os.Environ()

	// Capture child stdout/stderr to a per-process log file. Without this,
	// Go's exec.Command discards both, which means a vllm-swift import
	// failure or model load error becomes a silent crashloop with no
	// trail. The log path is stable per (namespace, name) so operators
	// can `tail -f` it during demos and post-mortem after bad rollouts.
	logPath := e.processLogPath(config.Namespace, config.Name)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open vllm-swift log file %s: %w", logPath, err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("failed to start vllm-swift: %w", err)
	}
	// The child holds the fd; we can close our handle. The OS keeps the
	// inode alive until the child closes (or exits, which closes its fds).
	_ = logFile.Close()

	process := &ManagedProcess{
		Name:      config.Name,
		Namespace: config.Namespace,
		PID:       cmd.Process.Pid,
		Port:      port,
		ModelPath: modelPath,
		ModelID:   filepath.Base(modelPath),
		StartedAt: time.Now(),
		Healthy:   false,
	}

	if err := e.waitForHealthy(port, e.startupTimeout); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			e.logger.Warnw("failed to kill unhealthy vllm-swift process",
				"pid", cmd.Process.Pid, "error", killErr)
		}
		return nil, fmt.Errorf("vllm-swift failed health check after %s: %w",
			e.startupTimeout, err)
	}

	process.Healthy = true
	e.logger.Infow("vllm-swift ready", "pid", process.PID, "port", port, "modelID", process.ModelID)
	return process, nil
}

// StopProcess sends SIGTERM with a 10s grace period before SIGKILL.
// vllm-swift's underlying Python api_server respects SIGTERM and shuts down
// the engine cleanly.
func (e *VLLMSwiftExecutor) StopProcess(pid int) error {
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

// processLogPath returns the per-process log file path for vllm-swift's
// stdout/stderr capture. We anchor under modelStorePath so log files live
// alongside the model artifacts the agent is already managing. The
// (namespace, name) pair is stable per InferenceService so operators can
// `tail -f` it across restarts.
func (e *VLLMSwiftExecutor) processLogPath(namespace, name string) string {
	return filepath.Join(e.modelStorePath, fmt.Sprintf("vllm-swift-%s-%s.log", namespace, name))
}

// resolveModelPath returns the on-disk path to the model directory.
// vllm-swift takes a directory containing config.json + safetensors/MLX
// weights, NOT a single file. If ModelSource is an absolute path or
// directory-shaped string, use it as-is; otherwise treat it as relative to
// modelStorePath.
//
// We resolve symlinks aggressively: vllm-swift's Swift-side architecture
// detection has a known issue where loading through a symlinked path
// reports "Unsupported model type: qwen3" (and similar for other archs)
// even when the same model loads cleanly via the resolved target. Passing
// the canonical path to the child sidesteps this entirely. EvalSymlinks
// returns the original path on failure so a missing dir still produces
// the friendly "model directory not found" error from StartProcess.
func (e *VLLMSwiftExecutor) resolveModelPath(config ExecutorConfig) string {
	candidate := config.ModelSource
	if candidate == "" {
		candidate = filepath.Join(e.modelStorePath, config.ModelName)
	} else if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(e.modelStorePath, candidate)
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return candidate
	}
	return resolved
}

// allocatePort asks the kernel for an unused TCP port. Same TOCTOU
// behavior as MetalExecutor.allocatePort (microsecond window before the
// child binds). Reused via a free function to avoid coupling executors.
func (e *VLLMSwiftExecutor) allocatePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// waitForHealthy polls /health on the allocated port until 200 or timeout.
func (e *VLLMSwiftExecutor) waitForHealthy(port int, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	healthURL := fmt.Sprintf("http://localhost:%d/health", port)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for vllm-swift /health")
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

// buildVLLMSwiftArgs constructs the command-line argument vector for the
// vllm-swift child process. Split out from StartProcess so the mapping
// from CRD fields to vLLM flags is unit-testable without spawning a real
// process.
//
// vllm-swift is a thin wrapper around `python -m vllm.entrypoints.openai.api_server`
// that auto-detects and injects the right --tool-call-parser based on the
// model architecture (see TheTom/vllm-swift detect_tool_parser.py). The
// metal-agent therefore does NOT inject --tool-call-parser explicitly; we
// let the wrapper do its job. Users who need to override pass the flag
// through ExtraArgs (which appends last so it wins).
func buildVLLMSwiftArgs(modelPath string, port int, config ExecutorConfig) []string {
	args := []string{
		"serve", modelPath,
		"--port", fmt.Sprintf("%d", port),
		"--host", "0.0.0.0",
		"--max-model-len", fmt.Sprintf("%d", config.ContextSize),
	}

	if config.ParallelSlots > 1 {
		args = append(args, "--max-num-seqs", fmt.Sprintf("%d", config.ParallelSlots))
	}

	// TurboQuant KV cache compression. CacheTypeK is the resolved cache-type
	// at the executor boundary: the agent layer (resolveCacheTypes) flattens
	// the CRD's CacheTypeK + CacheTypeCustomK pair so executors only see one
	// string. vllm-swift uses a single kv_scheme/kv_bits pair for both K and
	// V, so we only consult the K side.
	if scheme, bits := turboQuantConfig(config.CacheTypeK); scheme != "" {
		blob, _ := json.Marshal(map[string]any{
			"kv_scheme": scheme,
			"kv_bits":   bits,
		})
		args = append(args, "--additional-config", string(blob))
	}

	// ExtraArgs comes last so user-provided overrides actually override
	// any flag we set above (vLLM uses last-wins for repeated flags).
	if len(config.ExtraArgs) > 0 {
		args = append(args, config.ExtraArgs...)
	}

	return args
}

// turboQuantConfig maps a CacheTypeCustomK string to (kv_scheme, kv_bits)
// for vllm-swift's --additional-config JSON. Only the documented turbo*
// schemes are recognized; anything else (including empty) returns ("", 0)
// so the caller knows to omit --additional-config entirely.
//
// Reference: TheTom/vllm-swift README + worker.py:123-126 which reads
// kv_scheme/kv_bits from vllm_config.additional_config.
func turboQuantConfig(cacheType string) (scheme string, bits int) {
	switch cacheType {
	case "turbo4v2", "turbo4":
		return cacheType, 4
	case "turbo3":
		return cacheType, 3
	case "turbo2":
		return cacheType, 2
	default:
		return "", 0
	}
}
