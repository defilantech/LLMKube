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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// DefaultTensorFoldStartupTimeout is how long the agent waits for a freshly
// spawned tensorfold to answer /health. A warm load of a 16-19 GB model on an
// M5 Max is 6-10 s, but the first run also compiles kernels and runs the
// load-time exactness self-check, which can take minutes. A child that refuses
// its checkpoint exits, and the exit ends the wait at once, so the long
// timeout only bounds a child that hangs.
const DefaultTensorFoldStartupTimeout = 10 * time.Minute

// tensorFoldNoUpdateCheckEnv disables TensorFold's startup update check. The
// agent pins the engine the operator installed; a background network call on
// every spawn is noise at best and a startup stall on an offline Mac at worst.
const tensorFoldNoUpdateCheckEnv = "TENSORFOLD_NO_UPDATE_CHECK"

// TensorFoldExecutor runs one `tensorfold serve` process per
// InferenceService (github.com/ashhart/TensorFold, an OpenAI-compatible MLX
// server with exact speculative decoding). Each process gets its own
// ephemeral port, like vllm-swift, so several TensorFold services can share a
// Mac. The agent only launches the engine: installing and pinning TensorFold
// (and its MLX version) is the operator's job.
type TensorFoldExecutor struct {
	bin            string
	modelStorePath string
	logger         *zap.SugaredLogger
	startupTimeout time.Duration

	// allocatePort picks the port for a child that did not pin one in
	// extraArgs; a seam so tests can point the health wait at a fake server.
	allocatePort func() (int, error)

	childTracker
	engineEnv
}

// NewTensorFoldExecutor creates an executor that spawns one tensorfold
// process per InferenceService.
func NewTensorFoldExecutor(bin, modelStorePath string, logger *zap.SugaredLogger) *TensorFoldExecutor {
	return &TensorFoldExecutor{
		bin:            bin,
		modelStorePath: modelStorePath,
		logger:         logger,
		startupTimeout: DefaultTensorFoldStartupTimeout,
		allocatePort:   allocateLoopbackPort,
	}
}

// SetStartupTimeout overrides the default startup timeout. Values <= 0 are
// coerced back to DefaultTensorFoldStartupTimeout.
func (e *TensorFoldExecutor) SetStartupTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultTensorFoldStartupTimeout
	}
	e.startupTimeout = d
}

// StartProcess resolves the model directory, spawns tensorfold, and blocks
// until /health answers 200, the child exits, or startupTimeout fires.
func (e *TensorFoldExecutor) StartProcess(_ context.Context, config ExecutorConfig) (*ManagedProcess, error) {
	modelPath := e.resolveModelPath(config)
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf(
			"tensorfold model directory not found at %s: %w "+
				"(the host metal-agent does not download MLX/HF model directories; "+
				"pre-download the model directory before deploying)",
			modelPath, err)
	}

	port, err := e.port(config.ExtraArgs)
	if err != nil {
		return nil, err
	}

	args := buildTensorFoldArgs(modelPath, port, config)
	e.logger.Infow("starting tensorfold",
		"bin", e.bin, "modelPath", modelPath, "port", port, "ctx", config.ContextSize)

	cmd := exec.Command(e.bin, args...)
	// Its own process group, so launchd's termination of the agent's group does
	// not reach the engine and a restarted agent can adopt it (#2079).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = e.modelStorePath // relative paths in engine flags resolve inside the model store
	cmd.Env = tensorFoldEnv(os.Environ(), e.envPassthrough)

	// Per-process log, truncated on every start so it holds only this run.
	// TensorFold prints why it refused a checkpoint and exits; without the
	// capture that reason would be lost.
	logPath := e.processLogPath(config.Namespace, config.Name)
	logFile, err := openEngineLog("tensorfold", logPath)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("failed to start tensorfold: %w", err)
	}
	// The child holds the fd; close our handle.
	_ = logFile.Close()
	exit := e.trackChild(cmd)
	if err := e.afterSpawn(config, cmd, exit, port, modelPath, logPath); err != nil {
		return nil, err
	}

	process := &ManagedProcess{
		Name:      config.Name,
		Namespace: config.Namespace,
		PID:       cmd.Process.Pid,
		Port:      port,
		ModelPath: modelPath,
		ModelID:   tensorFoldServedName(config),
		StartedAt: time.Now(),
	}

	if err := waitForChildHealthy(port, e.startupTimeout, exit.done); err != nil {
		if errors.Is(err, errChildExited) {
			// Already reaped: nothing to stop, and its PID may be reused.
			e.untrackChild(process.PID)
			return nil, withLogTail(
				fmt.Errorf("tensorfold %w (%s)", err, exit.status()), logPath)
		}
		if stopErr := e.StopProcess(process.PID); stopErr != nil {
			e.logger.Warnw("failed to stop unhealthy tensorfold process",
				"pid", process.PID, "port", port, "error", stopErr)
		}
		return nil, withLogTail(
			fmt.Errorf("tensorfold failed health check after %s: %w", e.startupTimeout, err), logPath)
	}

	process.Healthy = true
	e.logger.Infow("tensorfold ready", "pid", process.PID, "port", port, "modelID", process.ModelID)
	return process, nil
}

// StopProcess sends SIGTERM with a 10s grace period before SIGKILL, through
// the reaper for a child this executor spawned.
func (e *TensorFoldExecutor) StopProcess(pid int) error {
	return e.stopChild(pid)
}

// port returns the port the child will bind: the user's --port from
// extraArgs when set (buildTensorFoldArgs then leaves ours out, and the health
// wait and registered endpoint must follow it), otherwise an ephemeral one.
func (e *TensorFoldExecutor) port(extraArgs []string) (int, error) {
	for i, v := range extraArgs {
		var raw string
		switch {
		case v == "--port":
			if i+1 >= len(extraArgs) {
				return 0, fmt.Errorf("extraArgs --port has no value")
			}
			raw = extraArgs[i+1]
		case strings.HasPrefix(v, "--port="):
			raw = strings.TrimPrefix(v, "--port=")
		default:
			continue
		}
		p, err := strconv.Atoi(raw)
		if err != nil || p <= 0 || p > 65535 {
			return 0, fmt.Errorf("extraArgs --port %q is not a valid port", raw)
		}
		return p, nil
	}
	p, err := e.allocatePort()
	if err != nil {
		return 0, fmt.Errorf("failed to allocate port: %w", err)
	}
	return p, nil
}

// processLogPath returns the per-process log for tensorfold's stdout/stderr,
// named like the llama-server, mlx-server and vllm-swift logs beside it.
func (e *TensorFoldExecutor) processLogPath(namespace, name string) string {
	return filepath.Join(e.modelStorePath, fmt.Sprintf("tensorfold-%s-%s.log", namespace, name))
}

// resolveModelPath returns the model directory tensorfold serves: an absolute
// spec.source as-is, a relative one under the model store, and an empty one as
// <store>/<Model name>, the same layout mlx-server and vllm-swift use.
// Symlinks are left alone: the Swift-side loader bug those executors work
// around does not apply to TensorFold, and a Hugging Face cache snapshot
// directory is meant to be read through its links.
func (e *TensorFoldExecutor) resolveModelPath(config ExecutorConfig) string {
	switch {
	case config.ModelSource == "":
		return filepath.Join(e.modelStorePath, config.ModelName)
	case filepath.IsAbs(config.ModelSource):
		return config.ModelSource
	default:
		return filepath.Join(e.modelStorePath, config.ModelSource)
	}
}

// tensorFoldServedName is the model ID clients send: the served model name the
// agent derives from spec.modelRef, or the InferenceService name.
func tensorFoldServedName(config ExecutorConfig) string {
	if config.ServedModelName != "" {
		return config.ServedModelName
	}
	return config.Name
}

// buildTensorFoldArgs constructs the `tensorfold serve` argument vector. Split
// out from StartProcess so the CRD-to-flag mapping is testable without a
// process. A flag the user already set in extraArgs is not emitted here, so
// the command line never carries it twice; extraArgs come last.
//
// TensorFold-specific knobs (--drafter, --thinking/--no-thinking,
// --max-tokens) are not modelled on the CRD and ride through extraArgs.
// --alias and --name override the model id the agent sets, so the extraArgs
// policy refuses them unless --allow-unsafe-extra-args is set.
func buildTensorFoldArgs(modelPath string, port int, config ExecutorConfig) []string {
	extra := config.ExtraArgs
	args := []string{"serve", modelPath}
	if !hasMatchingExtraArg(extra, "host") {
		args = append(args, "--host", bindHost(config))
	}
	if !hasMatchingExtraArg(extra, "port") {
		args = append(args, "--port", strconv.Itoa(port))
	}
	if !hasMatchingExtraArg(extra, "name") {
		args = append(args, "--name", tensorFoldServedName(config))
	}
	if !hasMatchingExtraArg(extra, "no-update-check") {
		args = append(args, "--no-update-check")
	}
	if config.ContextSize > 0 && !hasMatchingExtraArg(extra, "context") {
		args = append(args, "--context", strconv.Itoa(config.ContextSize))
	}
	return append(args, extra...)
}

// tensorFoldEnv returns the engine environment built from parent (see
// buildEngineEnv) with the update check turned off, replacing any value the
// parent carried. HOME and HF_HOME stay: TensorFold finds its drafts in the
// Hugging Face cache through them.
func tensorFoldEnv(parent, passthrough []string) []string {
	return buildEngineEnv(parent, passthrough, tensorFoldNoUpdateCheckEnv+"=1")
}

// allocateLoopbackPort asks the kernel for an unused TCP port. Same TOCTOU
// window as MetalExecutor.allocatePort: the port is free when we look and the
// child binds it microseconds later.
func allocateLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
