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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
)

// DefaultOMLXStartupTimeout is how long the agent waits for the oMLX daemon
// to become healthy after launching it. The original default was 30s, which
// is too short on real hardware: Apple Silicon spinup for the oMLX daemon
// includes scanning the model directory, sizing the engine pool, and loading
// the MLX framework — empirically ~35s on M5 Max even with no models pinned.
// 120s gives generous headroom for the slow-first-load case while still
// failing fast if oMLX is genuinely broken.
const DefaultOMLXStartupTimeout = 120 * time.Second

// OMLXExecutor manages the shared oMLX daemon and individual model lifecycles.
// Unlike MetalExecutor (one process per model), oMLX runs a single daemon that
// serves all models from a shared model directory.
type OMLXExecutor struct {
	omlxBin        string
	modelDir       string
	port           int
	process        *os.Process
	mu             sync.Mutex
	logger         *zap.SugaredLogger
	httpClient     *http.Client
	startupTimeout time.Duration
	// turboQuantBits is the KV cache quantization bit width for the oMLX
	// daemon (--kv-cache-quant). Set once when the daemon starts; shared
	// across all models served by this daemon. Zero means no TurboQuant.
	turboQuantBits int
	// pagedSSDCacheDir is the directory for the oMLX paged SSD cache
	// (--paged-ssd-cache-dir). Set once when the daemon starts; shared
	// across all models served by this daemon. Empty means no paged SSD cache.
	pagedSSDCacheDir string
	// hotCacheMaxSize is the maximum size of the oMLX hot cache
	// (--hot-cache-max-size). Set once when the daemon starts; shared
	// across all models served by this daemon. Empty means no limit.
	hotCacheMaxSize string
	// pagedSSDCacheMaxSize is the maximum size of the oMLX paged SSD cache
	// (--paged-ssd-cache-max-size). Set once when the daemon starts; shared
	// across all models served by this daemon. Empty means no limit.
	pagedSSDCacheMaxSize string
	// bindHost is the ExecutorConfig.BindHost override for the daemon's
	// --host flag. Set once when the daemon starts; shared across all models
	// served by this daemon. Empty resolves to engineBindHost (loopback).
	bindHost string
	// basePath is the oMLX data directory passed to the daemon as
	// --base-path. The agent registers each model's served name as an oMLX
	// model_alias in {basePath}/model_settings.json (see
	// omlxModelSettingsFile). Empty disables alias registration.
	basePath string
}

// NewOMLXExecutor creates an executor that manages models via the oMLX daemon.
func NewOMLXExecutor(omlxBin, modelDir string, port int, logger *zap.SugaredLogger) *OMLXExecutor {
	return &OMLXExecutor{
		omlxBin:  omlxBin,
		modelDir: modelDir,
		port:     port,
		logger:   logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		startupTimeout: DefaultOMLXStartupTimeout,
		basePath:       omlxDefaultBasePath(),
	}
}

// SetStartupTimeout overrides the default oMLX-daemon startup timeout.
// Values <= 0 are coerced back to DefaultOMLXStartupTimeout.
func (e *OMLXExecutor) SetStartupTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultOMLXStartupTimeout
	}
	e.startupTimeout = d
}

// omlxModelsStatusResponse is the response from GET /v1/models/status.
type omlxModelsStatusResponse struct {
	Models []omlxModelStatus `json:"models"`
}

type omlxModelStatus struct {
	ID        string `json:"id"`
	Loaded    bool   `json:"loaded"`
	IsLoading bool   `json:"is_loading"`
}

// StartProcess ensures the oMLX daemon is running and triggers loading of the
// specified model. It returns a ManagedProcess whose PID is the daemon PID and
// whose Port is the shared oMLX port.
func (e *OMLXExecutor) StartProcess(ctx context.Context, config ExecutorConfig) (*ManagedProcess, error) {
	// For oMLX, the model source IS the model directory path.
	// The model ID for oMLX is the directory base name.
	modelPath := config.ModelSource
	if modelPath == "" {
		modelPath = filepath.Join(e.modelDir, config.ModelName)
	}
	modelID := filepath.Base(modelPath)

	configPath := filepath.Join(modelPath, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		return nil, fmt.Errorf(
			"oMLX model not found at %s (config.json missing): models must be pre-downloaded",
			modelPath)
	}

	e.logger.Infow("starting oMLX model", "modelID", modelID, "modelPath", modelPath)

	// Set TurboQuant KV cache quantization on the daemon. This is a daemon-level
	// setting applied once when the daemon starts; shared across all models.
	e.mu.Lock()
	e.turboQuantBits = config.TurboQuantBits
	e.pagedSSDCacheDir = config.PagedSSDCacheDir
	e.hotCacheMaxSize = config.HotCacheMaxSize
	e.pagedSSDCacheMaxSize = config.PagedSSDCacheMaxSize
	e.bindHost = config.BindHost
	e.mu.Unlock()

	// Ensure the oMLX daemon is running and serves this model under its
	// served name (spec.modelRef, else the Model name) as well as under the
	// directory name, the same name llama-server reports via --alias.
	if err := e.ensureOMLXRunning(ctx, modelID, config.ServedModelName); err != nil {
		return nil, fmt.Errorf("failed to start oMLX daemon: %w", err)
	}

	// Trigger model loading via a warmup chat completion request.
	// oMLX lazily loads models on first inference request.
	if err := e.triggerModelLoad(ctx, modelID); err != nil {
		return nil, fmt.Errorf("failed to trigger model load for %s: %w", modelID, err)
	}

	// Wait for the model to report loaded status
	if err := e.waitForModelLoaded(ctx, modelID, 30*time.Second); err != nil {
		return nil, fmt.Errorf("model %s failed to load within timeout: %w", modelID, err)
	}

	e.mu.Lock()
	pid := 0
	if e.process != nil {
		pid = e.process.Pid
	}
	e.mu.Unlock()

	process := &ManagedProcess{
		Name:      config.Name,
		Namespace: config.Namespace,
		PID:       pid,
		Port:      e.port,
		ModelPath: modelPath,
		ModelID:   modelID,
		StartedAt: time.Now(),
		Healthy:   true,
	}

	e.logger.Infow("oMLX model loaded", "modelID", modelID, "port", e.port)
	return process, nil
}

// StopProcess unloads a model from the oMLX daemon. It does NOT kill the daemon
// because other models may still be served by it.
func (e *OMLXExecutor) StopProcess(pid int) error {
	// For oMLX, StopProcess receives the daemon PID but we need the model ID.
	// The caller (deleteProcess) already removed the process from the map, so
	// we cannot look it up here. Instead, we attempt to find the model by
	// iterating loaded models — but this is racy.
	//
	// A better approach: the agent passes the ModelID via the ManagedProcess
	// before calling StopProcess. Since the interface takes only pid, we use
	// StopProcessByModelID for oMLX-specific unload and fall back to a no-op
	// here. The agent calls StopProcess which for oMLX is intentionally a
	// no-op on the daemon itself.
	//
	// The actual unload is done via UnloadModel called from the agent layer.
	e.logger.Debugw("StopProcess called for oMLX (no-op on daemon)", "pid", pid)
	return nil
}

// UnloadModel sends a POST /v1/models/{modelID}/unload request to the oMLX daemon.
func (e *OMLXExecutor) UnloadModel(ctx context.Context, modelID string) error {
	if modelID == "" {
		return fmt.Errorf("cannot unload model: empty model ID")
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/v1/models/%s/unload", e.port, modelID)

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create unload request: %w", err)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to unload model %s: %w", modelID, err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oMLX unload returned status %d for model %s", resp.StatusCode, modelID)
	}

	e.logger.Infow("unloaded oMLX model", "modelID", modelID)
	return nil
}

// buildOMLXServeArgs constructs the command-line argument vector for the
// oMLX daemon serve subcommand. It is split out from ensureOMLXRunning so it
// can be unit tested without spawning a real process. The function is pure:
// it takes the model directory, port, and a config struct and returns the
// full arg slice. Conditional flags are emitted only when their corresponding
// config field is set (non-empty for strings, > 0 for turboQuantBits).
func buildOMLXServeArgs(modelDir string, port int, cfg omlxServeConfig) []string {
	args := []string{
		"serve",
		"--model-dir", modelDir,
		"--port", fmt.Sprint(port),
		"--host", resolveBindHost(cfg.bindHost),
	}

	// TurboQuant KV cache quantization (oMLX v0.3.4+). Maps to --kv-cache-quant
	// flag. The bits value (3, 6, or 8) is set via StartProcess before this
	// call. When turboQuantBits is set, the flag is emitted; when omitted,
	// oMLX uses its default (unquantized).
	if cfg.turboQuantBits > 0 {
		args = append(args, "--kv-cache-quant", fmt.Sprint(cfg.turboQuantBits))
	}

	// Paged SSD cache directory. Maps to --paged-ssd-cache-dir. When set,
	// the oMLX daemon uses a paged cache backed by the specified directory,
	// allowing models to exceed available RAM by paging KV cache blocks to SSD.
	if cfg.pagedSSDCacheDir != "" {
		args = append(args, "--paged-ssd-cache-dir", cfg.pagedSSDCacheDir)
	}

	// Hot cache max size. Maps to --hot-cache-max-size. A string value like
	// "100GB" or "50GB". When set, limits the size of the hot cache in RAM.
	if cfg.hotCacheMaxSize != "" {
		args = append(args, "--hot-cache-max-size", cfg.hotCacheMaxSize)
	}

	// Paged SSD cache max size. Maps to --paged-ssd-cache-max-size. A string
	// value like "200GB" or "500GB". When set, limits the size of the paged
	// cache on SSD.
	if cfg.pagedSSDCacheMaxSize != "" {
		args = append(args, "--paged-ssd-cache-max-size", cfg.pagedSSDCacheMaxSize)
	}

	// Base path. Maps to --base-path. Pinned explicitly so the daemon reads
	// the model_settings.json the agent writes model aliases into, rather
	// than relying on both sides agreeing on oMLX's ~/.omlx default.
	if cfg.basePath != "" {
		args = append(args, "--base-path", cfg.basePath)
	}

	return args
}

// omlxServeConfig holds the daemon-level configuration for the oMLX serve
// subcommand. It mirrors the fields on OMLXExecutor that are set once when
// the daemon starts and shared across all models.
type omlxServeConfig struct {
	turboQuantBits       int
	pagedSSDCacheDir     string
	hotCacheMaxSize      string
	pagedSSDCacheMaxSize string
	// bindHost is the ExecutorConfig.BindHost override for the daemon's
	// --host flag. Empty resolves to engineBindHost (loopback) via
	// resolveBindHost in buildOMLXServeArgs.
	bindHost string
	// basePath is the oMLX data directory (--base-path). Empty leaves the
	// flag off and oMLX uses its own default.
	basePath string
}

// ensureOMLXRunning registers servedName as the oMLX alias of modelID and
// starts the oMLX daemon if it is not already responding. oMLX reads aliases
// only at startup, so when the alias changed and this agent owns the running
// daemon, the daemon is restarted to load it. Other models the daemon served
// are unloaded by the restart and load again lazily on their next request.
func (e *OMLXExecutor) ensureOMLXRunning(ctx context.Context, modelID, servedName string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	aliasChanged, err := e.registerAliasLocked(modelID, servedName)
	if err != nil {
		return err
	}

	// Check if oMLX is already responding
	if e.isHealthy(ctx) {
		if !aliasChanged {
			e.logger.Debugw("oMLX daemon already running", "port", e.port)
			return nil
		}
		if e.process == nil {
			e.logger.Warnw("oMLX daemon on this port was not started by the agent; "+
				"restart it so it serves the model under its new alias",
				"port", e.port, "modelID", modelID, "alias", servedName)
			return nil
		}
		e.logger.Infow("restarting oMLX daemon to load a changed model alias",
			"pid", e.process.Pid, "modelID", modelID, "alias", servedName)
		if err := e.stopDaemonLocked(ctx); err != nil {
			return fmt.Errorf("failed to restart oMLX daemon for alias %q: %w", servedName, err)
		}
	}

	e.logger.Infow("starting oMLX daemon", "bin", e.omlxBin, "modelDir", e.modelDir, "port", e.port)

	cfg := omlxServeConfig{
		turboQuantBits:       e.turboQuantBits,
		pagedSSDCacheDir:     e.pagedSSDCacheDir,
		hotCacheMaxSize:      e.hotCacheMaxSize,
		pagedSSDCacheMaxSize: e.pagedSSDCacheMaxSize,
		bindHost:             e.bindHost,
		basePath:             e.basePath,
	}
	cmd := exec.Command(e.omlxBin, buildOMLXServeArgs(e.modelDir, e.port, cfg)...)
	cmd.Dir = e.modelDir // relative paths in engine flags resolve inside the model store
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start oMLX daemon: %w", err)
	}

	e.process = cmd.Process
	e.logger.Infow("oMLX daemon started", "pid", cmd.Process.Pid)

	// Wait for oMLX to become healthy. On real hardware the first model load
	// can take 30+ seconds — see DefaultOMLXStartupTimeout for the rationale.
	if err := e.waitForHealthy(ctx, e.startupTimeout); err != nil {
		// Best-effort kill if startup failed
		_ = cmd.Process.Kill()
		e.process = nil
		return fmt.Errorf("oMLX daemon failed to become healthy after %s: %w",
			e.startupTimeout, err)
	}

	return nil
}

// registerAliasLocked writes servedName as the oMLX model_alias of modelID
// and reports whether the settings file changed. A served name equal to the
// directory name clears any stale alias, so /v1/models lists the name clients
// should send. The directory name keeps working either way, because oMLX
// matches it before any alias. Must be called with e.mu held.
func (e *OMLXExecutor) registerAliasLocked(modelID, servedName string) (bool, error) {
	if servedName == "" {
		return false, nil
	}
	if e.basePath == "" {
		e.logger.Warnw("oMLX base path unknown; model is served under its directory name only",
			"modelID", modelID, "servedName", servedName)
		return false, nil
	}
	alias := servedName
	if alias == modelID {
		alias = ""
	}
	changed, err := setOMLXModelAlias(filepath.Join(e.basePath, omlxModelSettingsFile), modelID, alias)
	if err != nil {
		return false, fmt.Errorf("failed to register oMLX alias %q for model %s: %w", servedName, modelID, err)
	}
	if changed {
		e.logger.Infow("registered oMLX model alias", "modelID", modelID, "alias", alias)
	}
	return changed, nil
}

// omlxStopTimeout bounds how long stopDaemonLocked waits for the daemon to
// exit on SIGTERM before killing it.
const omlxStopTimeout = 15 * time.Second

// stopDaemonLocked stops the daemon this executor started and waits until it
// no longer answers /health. Must be called with e.mu held and e.process set.
func (e *OMLXExecutor) stopDaemonLocked(ctx context.Context) error {
	p := e.process
	e.process = nil

	exited := make(chan struct{})
	go func() {
		_, _ = p.Wait()
		close(exited)
	}()
	_ = p.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(omlxStopTimeout):
		_ = p.Kill()
		<-exited
	case <-ctx.Done():
		_ = p.Kill()
		return ctx.Err()
	}

	return pollUntil(ctx, 100*time.Millisecond, omlxStopTimeout, func(ctx context.Context) (bool, error) {
		return !e.isHealthy(ctx), nil
	})
}

// isHealthy checks if the oMLX daemon is responding at /health.
func (e *OMLXExecutor) isHealthy(ctx context.Context) bool {
	url := fmt.Sprintf("http://127.0.0.1:%d/health", e.port)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer drainAndClose(resp)

	return resp.StatusCode == http.StatusOK
}

// waitForHealthy polls /health until the daemon responds with 200.
func (e *OMLXExecutor) waitForHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for oMLX health after %s", timeout)
		case <-ticker.C:
			if e.isHealthy(ctx) {
				return nil
			}
		}
	}
}

// triggerModelLoad sends a minimal chat completion request to oMLX which causes
// it to lazily load the requested model.
func (e *OMLXExecutor) triggerModelLoad(ctx context.Context, modelID string) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", e.port)

	payload := map[string]interface{}{
		"model": modelID,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 1,
	}

	// Use a longer timeout for the warmup since model loading can take a while
	warmupClient := &http.Client{Timeout: 60 * time.Second}

	resp, err := postJSON(ctx, warmupClient, url, payload)
	if err != nil {
		// The warmup may timeout if the model is large — that's fine, we poll
		// /v1/models/status separately.
		e.logger.Warnw("warmup request failed (model may still be loading)", "modelID", modelID, "error", err)
		return nil
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 500 {
		return fmt.Errorf("warmup request returned server error: %d", resp.StatusCode)
	}

	e.logger.Debugw("warmup request completed", "modelID", modelID, "status", resp.StatusCode)
	return nil
}

// waitForModelLoaded polls /v1/models/status until the target model reports loaded:true.
func (e *OMLXExecutor) waitForModelLoaded(ctx context.Context, modelID string, timeout time.Duration) error {
	return pollUntil(ctx, 500*time.Millisecond, timeout, func(ctx context.Context) (bool, error) {
		loaded, err := e.isModelLoaded(ctx, modelID)
		if err != nil {
			e.logger.Debugw("error checking model status", "modelID", modelID, "error", err)
			return false, nil
		}
		return loaded, nil
	})
}

// isModelLoaded checks whether the specified model is loaded in oMLX.
func (e *OMLXExecutor) isModelLoaded(ctx context.Context, modelID string) (bool, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/models/status", e.port)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("models/status returned %d", resp.StatusCode)
	}

	var status omlxModelsStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return false, fmt.Errorf("failed to decode models/status: %w", err)
	}

	for _, m := range status.Models {
		if m.ID == modelID && m.Loaded {
			return true, nil
		}
	}

	return false, nil
}
