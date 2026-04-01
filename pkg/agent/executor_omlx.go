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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// OMLXExecutor manages the shared oMLX daemon and individual model lifecycles.
// Unlike MetalExecutor (one process per model), oMLX runs a single daemon that
// serves all models from a shared model directory.
type OMLXExecutor struct {
	omlxBin    string
	modelDir   string
	port       int
	process    *os.Process
	mu         sync.Mutex
	logger     *zap.SugaredLogger
	httpClient *http.Client
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
	}
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

	// Ensure the oMLX daemon is running
	if err := e.ensureOMLXRunning(ctx); err != nil {
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

	url := fmt.Sprintf("http://localhost:%d/v1/models/%s/unload", e.port, modelID)

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create unload request: %w", err)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to unload model %s: %w", modelID, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oMLX unload returned status %d for model %s", resp.StatusCode, modelID)
	}

	e.logger.Infow("unloaded oMLX model", "modelID", modelID)
	return nil
}

// ensureOMLXRunning starts the oMLX daemon if it is not already responding.
func (e *OMLXExecutor) ensureOMLXRunning(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Check if oMLX is already responding
	if e.isHealthy(ctx) {
		e.logger.Debugw("oMLX daemon already running", "port", e.port)
		return nil
	}

	e.logger.Infow("starting oMLX daemon", "bin", e.omlxBin, "modelDir", e.modelDir, "port", e.port)

	cmd := exec.Command(e.omlxBin, "serve",
		"--model-dir", e.modelDir,
		"--port", fmt.Sprint(e.port),
		"--host", "0.0.0.0",
	)
	cmd.Env = os.Environ()

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start oMLX daemon: %w", err)
	}

	e.process = cmd.Process
	e.logger.Infow("oMLX daemon started", "pid", cmd.Process.Pid)

	// Wait for oMLX to become healthy
	if err := e.waitForHealthy(ctx, 30*time.Second); err != nil {
		// Best-effort kill if startup failed
		_ = cmd.Process.Kill()
		e.process = nil
		return fmt.Errorf("oMLX daemon failed to become healthy: %w", err)
	}

	return nil
}

// isHealthy checks if the oMLX daemon is responding at /health.
func (e *OMLXExecutor) isHealthy(ctx context.Context) bool {
	url := fmt.Sprintf("http://localhost:%d/health", e.port)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

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
	url := fmt.Sprintf("http://localhost:%d/v1/chat/completions", e.port)

	payload := map[string]interface{}{
		"model": modelID,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"max_tokens": 1,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal warmup request: %w", err)
	}

	// Use a longer timeout for the warmup since model loading can take a while
	warmupClient := &http.Client{Timeout: 60 * time.Second}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create warmup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := warmupClient.Do(req)
	if err != nil {
		// The warmup may timeout if the model is large — that's fine, we poll
		// /v1/models/status separately.
		e.logger.Warnw("warmup request failed (model may still be loading)", "modelID", modelID, "error", err)
		return nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 500 {
		return fmt.Errorf("warmup request returned server error: %d", resp.StatusCode)
	}

	e.logger.Debugw("warmup request completed", "modelID", modelID, "status", resp.StatusCode)
	return nil
}

// waitForModelLoaded polls /v1/models/status until the target model reports loaded:true.
func (e *OMLXExecutor) waitForModelLoaded(ctx context.Context, modelID string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for model %s to load", modelID)
		case <-ticker.C:
			loaded, err := e.isModelLoaded(ctx, modelID)
			if err != nil {
				e.logger.Debugw("error checking model status", "modelID", modelID, "error", err)
				continue
			}
			if loaded {
				return nil
			}
		}
	}
}

// isModelLoaded checks whether the specified model is loaded in oMLX.
func (e *OMLXExecutor) isModelLoaded(ctx context.Context, modelID string) (bool, error) {
	url := fmt.Sprintf("http://localhost:%d/v1/models/status", e.port)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return false, err
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

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
