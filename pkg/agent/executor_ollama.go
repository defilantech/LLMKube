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
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// OllamaExecutor manages models via a shared Ollama daemon. Unlike MetalExecutor
// (one process per model), Ollama runs as a system service that handles model
// downloads, loading, and inference internally.
type OllamaExecutor struct {
	port       int
	mu         sync.Mutex
	logger     *zap.SugaredLogger
	httpClient *http.Client
}

// NewOllamaExecutor creates an executor that manages models via the Ollama daemon.
func NewOllamaExecutor(port int, logger *zap.SugaredLogger) *OllamaExecutor {
	return &OllamaExecutor{
		port:   port,
		logger: logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// ollamaModelMap maps LLMKube model names to Ollama model tags.
var ollamaModelMap = map[string]string{
	"llama-3.2-3b":       "llama3.2:3b",
	"llama-3.1-8b":       "llama3.1:8b",
	"llama-3.3-70b":      "llama3.3:70b",
	"qwen-2.5-14b":       "qwen2.5:14b",
	"qwen-2.5-32b":       "qwen2.5:32b",
	"qwen-2.5-coder-7b":  "qwen2.5-coder:7b",
	"qwen-2.5-coder-32b": "qwen2.5-coder:32b",
	"qwen-3-32b":         "qwen3:32b",
	"mistral-7b":         "mistral:7b",
	"mistral-small-24b":  "mistral-small:24b",
	"gemma-3-4b":         "gemma3:4b",
	"gemma-3-12b":        "gemma3:12b",
	"phi-4-mini":         "phi4-mini:latest",
	"deepseek-r1-7b":     "deepseek-r1:7b",
	"deepseek-r1-14b":    "deepseek-r1:14b",
	"deepseek-r1-32b":    "deepseek-r1:32b",
}

// ollamaPsResponse is the response from GET /api/ps.
type ollamaPsResponse struct {
	Models []ollamaPsModel `json:"models"`
}

type ollamaPsModel struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

type ollamaPullRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type ollamaGenerateRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	KeepAlive *int   `json:"keep_alive,omitempty"`
}

// resolveOllamaModel maps a LLMKube model name to an Ollama model tag.
// If no mapping exists, the original name is returned as-is (allowing users
// to specify arbitrary Ollama model names in their Model CRD).
func resolveOllamaModel(modelName string) string {
	if mapped, ok := ollamaModelMap[modelName]; ok {
		return mapped
	}
	return modelName
}

// StartProcess ensures the Ollama daemon is running, pulls the model, and
// pre-loads it into memory. It returns a ManagedProcess with PID=0 since we
// do not manage the Ollama process lifecycle.
func (e *OllamaExecutor) StartProcess(
	ctx context.Context, config ExecutorConfig,
) (*ManagedProcess, error) {
	ollamaModel := resolveOllamaModel(config.ModelName)

	e.logger.Infow("starting Ollama model",
		"llmkubeModel", config.ModelName, "ollamaModel", ollamaModel)

	// 1. Check if Ollama is already running
	if !e.isHealthy(ctx) {
		return nil, fmt.Errorf(
			"ollama is not running on port %d; start with: ollama serve", e.port)
	}

	// 2. Pull model (handles download if not already cached)
	if err := e.pullModel(ctx, ollamaModel); err != nil {
		return nil, fmt.Errorf("failed to pull model %s: %w", ollamaModel, err)
	}

	// 3. Pre-load model into memory
	if err := e.loadModel(ctx, ollamaModel); err != nil {
		return nil, fmt.Errorf(
			"failed to pre-load model %s: %w", ollamaModel, err)
	}

	// 4. Verify model is loaded
	if err := e.waitForModelLoaded(ctx, ollamaModel, 60*time.Second); err != nil {
		return nil, fmt.Errorf(
			"model %s failed to load within timeout: %w", ollamaModel, err)
	}

	process := &ManagedProcess{
		Name:      config.Name,
		Namespace: config.Namespace,
		PID:       0, // We don't manage the Ollama process
		Port:      e.port,
		ModelPath: "", // Ollama manages its own model storage
		ModelID:   ollamaModel,
		StartedAt: time.Now(),
		Healthy:   true,
	}

	e.logger.Infow("Ollama model loaded",
		"ollamaModel", ollamaModel, "port", e.port)
	return process, nil
}

// StopProcess is a no-op for Ollama. The daemon is shared and not managed by
// the agent. Model unloading is handled via UnloadModel.
func (e *OllamaExecutor) StopProcess(pid int) error {
	e.logger.Debugw("StopProcess called for Ollama (no-op on daemon)", "pid", pid)
	return nil
}

// UnloadModel sends a generate request with keep_alive=0 to unload the model
// from Ollama's memory.
func (e *OllamaExecutor) UnloadModel(ctx context.Context, modelID string) error {
	if modelID == "" {
		return fmt.Errorf("cannot unload model: empty model ID")
	}

	keepAlive := 0
	payload := ollamaGenerateRequest{
		Model:     modelID,
		Prompt:    "",
		KeepAlive: &keepAlive,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal unload request: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%d/api/generate", e.port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create unload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to unload model %s: %w", modelID, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"ollama unload returned status %d for model %s",
			resp.StatusCode, modelID)
	}

	e.logger.Infow("unloaded Ollama model", "modelID", modelID)
	return nil
}

// isHealthy checks if the Ollama daemon is responding at GET /.
func (e *OllamaExecutor) isHealthy(ctx context.Context) bool {
	url := fmt.Sprintf("http://localhost:%d/", e.port)
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

// pullModel sends POST /api/pull to download the model if not already cached.
// Uses a 5-minute timeout since model downloads can be large.
func (e *OllamaExecutor) pullModel(ctx context.Context, ollamaModel string) error {
	payload := ollamaPullRequest{
		Model:  ollamaModel,
		Stream: false,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal pull request: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%d/api/pull", e.port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create pull request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a longer timeout for pulls since downloads can take minutes
	pullClient := &http.Client{Timeout: 5 * time.Minute}

	e.logger.Infow("pulling Ollama model (this may take a while)",
		"model", ollamaModel)

	resp, err := pullClient.Do(req)
	if err != nil {
		return fmt.Errorf("pull request failed: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull returned status %d for model %s",
			resp.StatusCode, ollamaModel)
	}

	e.logger.Infow("Ollama model pull complete", "model", ollamaModel)
	return nil
}

// loadModel sends POST /api/generate with an empty prompt to pre-load the
// model into Ollama's memory. Uses a 2-minute timeout since model loading
// can take time on large models.
func (e *OllamaExecutor) loadModel(ctx context.Context, ollamaModel string) error {
	payload := ollamaGenerateRequest{
		Model:  ollamaModel,
		Prompt: "",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal load request: %w", err)
	}

	url := fmt.Sprintf("http://localhost:%d/api/generate", e.port)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to create load request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a 2-minute timeout for model loading
	loadClient := &http.Client{Timeout: 2 * time.Minute}

	resp, err := loadClient.Do(req)
	if err != nil {
		// Loading may timeout for large models — we poll /api/ps separately.
		e.logger.Warnw("load request failed (model may still be loading)",
			"model", ollamaModel, "error", err)
		return nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 500 {
		return fmt.Errorf("load request returned server error: %d",
			resp.StatusCode)
	}

	e.logger.Debugw("load request completed",
		"model", ollamaModel, "status", resp.StatusCode)
	return nil
}

// waitForModelLoaded polls GET /api/ps until the target model appears in the
// list of loaded models.
func (e *OllamaExecutor) waitForModelLoaded(
	ctx context.Context, ollamaModel string, timeout time.Duration,
) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf(
				"timeout waiting for model %s to load", ollamaModel)
		case <-ticker.C:
			loaded, err := e.isModelLoaded(ctx, ollamaModel)
			if err != nil {
				e.logger.Debugw("error checking model status",
					"model", ollamaModel, "error", err)
				continue
			}
			if loaded {
				return nil
			}
		}
	}
}

// isModelLoaded checks whether the specified model is loaded in Ollama by
// querying GET /api/ps and checking the models list.
func (e *OllamaExecutor) isModelLoaded(
	ctx context.Context, ollamaModel string,
) (bool, error) {
	url := fmt.Sprintf("http://localhost:%d/api/ps", e.port)
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
		return false, fmt.Errorf("api/ps returned %d", resp.StatusCode)
	}

	var ps ollamaPsResponse
	if err := json.NewDecoder(resp.Body).Decode(&ps); err != nil {
		return false, fmt.Errorf("failed to decode api/ps: %w", err)
	}

	for _, m := range ps.Models {
		// Ollama model names may include a tag suffix (e.g., "llama3.2:3b")
		// that matches exactly, or the Name field may have additional metadata.
		// Check both Name and Model fields.
		if m.Name == ollamaModel || m.Model == ollamaModel ||
			strings.HasPrefix(m.Name, ollamaModel) ||
			strings.HasPrefix(m.Model, ollamaModel) {
			return true, nil
		}
	}

	return false, nil
}
