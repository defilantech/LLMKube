/*
Copyright 2026.

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
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// DefaultMLXServerStartupTimeout is how long the agent waits for a freshly
// spawned mlx-server process to respond on /health. mlx-server loads MLX
// weights on startup; a 35B-class model on an M5 Max is well under a minute
// once cached, but a cold load is slower. 120s gives generous headroom while
// still failing fast on real breakage.
const DefaultMLXServerStartupTimeout = 120 * time.Second

// MLXServerExecutor manages a single mlx-server process for the agent's
// InferenceService. mlx-server (github.com/defilantech/mlx-server) is a native
// OpenAI-compatible MLX inference server. Unlike the vllm-swift executor's
// per-spawn ephemeral port, this executor binds a fixed port so clients
// (e.g. opencode) keep a stable base URL across respawns.
type MLXServerExecutor struct {
	bin            string
	modelStorePath string
	port           int
	logger         *zap.SugaredLogger
	startupTimeout time.Duration

	// childTracker holds the reaper of every mlx-server this executor
	// spawned, so StopProcess never waits on a PID twice.
	childTracker
	engineEnv
}

// NewMLXServerExecutor creates an executor that spawns one mlx-server
// process on the given fixed port.
func NewMLXServerExecutor(bin, modelStorePath string, port int, logger *zap.SugaredLogger) *MLXServerExecutor {
	return &MLXServerExecutor{
		bin:            bin,
		modelStorePath: modelStorePath,
		port:           port,
		logger:         logger,
		startupTimeout: DefaultMLXServerStartupTimeout,
	}
}

// SetStartupTimeout overrides the default mlx-server startup timeout.
// Values <= 0 are coerced back to DefaultMLXServerStartupTimeout.
func (e *MLXServerExecutor) SetStartupTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultMLXServerStartupTimeout
	}
	e.startupTimeout = d
}

// StartProcess resolves the model directory, spawns mlx-server on the
// executor's fixed port, and blocks until /health answers 200, the child
// exits, or startupTimeout fires.
func (e *MLXServerExecutor) StartProcess(_ context.Context, config ExecutorConfig) (*ManagedProcess, error) {
	modelPath := e.resolveModelPath(config)
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf(
			"mlx-server model directory not found at %s: %w "+
				"(the host metal-agent does not download MLX/HF model directories; "+
				"pre-download the model directory before deploying)",
			modelPath, err)
	}

	args := buildMLXServerArgs(modelPath, e.port, config)

	e.logger.Infow("starting mlx-server",
		"bin", e.bin, "modelPath", modelPath, "port", e.port)

	cmd := exec.Command(e.bin, args...)
	// Its own process group, so launchd's termination of the agent's group does
	// not reach the engine and a restarted agent can adopt it (#2079).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = e.modelStorePath // relative paths in engine flags resolve inside the model store
	cmd.Env = e.childEnv()

	// Capture child stdout/stderr to a per-process log file. Without this a
	// model-load failure becomes a silent crashloop with no trail. The path
	// is stable per (namespace, name) so operators can tail it across restarts.
	logPath := e.processLogPath(config.Namespace, config.Name)
	logFile, err := openEngineLog("mlx-server", logPath)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("failed to start mlx-server: %w", err)
	}
	// The child holds the fd; close our handle. The OS keeps the inode alive
	// until the child exits.
	_ = logFile.Close()
	exit := e.trackChild(cmd)
	if err := e.afterSpawn(config, cmd, exit, e.port, modelPath, logPath); err != nil {
		return nil, err
	}

	process := &ManagedProcess{
		Name:      config.Name,
		Namespace: config.Namespace,
		PID:       cmd.Process.Pid,
		Port:      e.port,
		ModelPath: modelPath,
		ModelID:   filepath.Base(modelPath),
		StartedAt: time.Now(),
		Healthy:   false,
	}

	if err := waitForChildHealthy(e.port, e.startupTimeout, exit.done); err != nil {
		if errors.Is(err, errChildExited) {
			// Already reaped: nothing to stop, and its PID may be reused.
			e.untrackChild(process.PID)
			return nil, withLogTail(
				fmt.Errorf("mlx-server %w (%s)", err, exit.status()), logPath)
		}
		if stopErr := e.StopProcess(process.PID); stopErr != nil {
			e.logger.Warnw("failed to stop unhealthy mlx-server process",
				"pid", process.PID, "port", e.port, "error", stopErr)
		}
		return nil, withLogTail(
			fmt.Errorf("mlx-server failed health check after %s: %w", e.startupTimeout, err), logPath)
	}

	process.Healthy = true
	e.logger.Infow("mlx-server ready", "pid", process.PID, "port", e.port, "modelID", process.ModelID)
	return process, nil
}

// StopProcess sends SIGTERM with a 10s grace period before SIGKILL, through
// the reaper for a child this executor spawned.
func (e *MLXServerExecutor) StopProcess(pid int) error {
	return e.stopChild(pid)
}

// processLogPath returns the per-process log file path for mlx-server's
// stdout/stderr capture, anchored under modelStorePath alongside the model
// artifacts. The (namespace, name) pair is stable across restarts.
func (e *MLXServerExecutor) processLogPath(namespace, name string) string {
	return filepath.Join(e.modelStorePath, fmt.Sprintf("mlx-server-%s-%s.log", namespace, name))
}

// resolveModelPath returns the on-disk path to the model directory.
// mlx-server takes a directory containing config.json + safetensors/MLX
// weights. If ModelSource is an absolute path use it as-is; otherwise treat
// it as relative to modelStorePath.
//
// Symlinks are resolved aggressively: MLX's Swift-side architecture detection
// has a known issue loading through a symlinked path, so the canonical path
// is passed to the child. EvalSymlinks returns the original path on failure
// so a missing directory still produces the friendly error from StartProcess.
func (e *MLXServerExecutor) resolveModelPath(config ExecutorConfig) string {
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

// buildMLXServerArgs constructs the command-line argument vector for the
// mlx-server child. Split out from StartProcess so the CRD-field-to-flag
// mapping is unit-testable without spawning a process.
//
// mlx-server-specific flags such as --tool-call-format and --reasoning are
// not set here; they ride through ExtraArgs (InferenceService.spec.extraArgs),
// appended last so user-provided values win.
func buildMLXServerArgs(modelPath string, port int, config ExecutorConfig) []string {
	args := []string{
		"--model", modelPath,
		"--host", bindHost(config),
		"--port", fmt.Sprintf("%d", port),
	}

	if config.ParallelSlots > 1 {
		args = append(args, "--max-slots", fmt.Sprintf("%d", config.ParallelSlots))
	}

	// ExtraArgs comes last so user-provided overrides win.
	if len(config.ExtraArgs) > 0 {
		args = append(args, config.ExtraArgs...)
	}

	return args
}
