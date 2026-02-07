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

package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestMean(t *testing.T) {
	testCases := []struct {
		name     string
		values   []float64
		expected float64
	}{
		{"empty slice", []float64{}, 0},
		{"single value", []float64{10.0}, 10.0},
		{"two values", []float64{10.0, 20.0}, 15.0},
		{"multiple values", []float64{10.0, 20.0, 30.0, 40.0}, 25.0},
		{"with decimals", []float64{1.5, 2.5, 3.0}, 2.333333},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := mean(tc.values)
			// Use approximate comparison for floating point
			diff := result - tc.expected
			if diff < -0.0001 || diff > 0.0001 {
				t.Errorf("mean(%v) = %v, expected %v", tc.values, result, tc.expected)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	testCases := []struct {
		name       string
		values     []float64
		percentile float64
		expected   float64
	}{
		{"empty slice", []float64{}, 50, 0},
		{"single value P50", []float64{100.0}, 50, 100.0},
		{"single value P99", []float64{100.0}, 99, 100.0},
		{"two values P50", []float64{10.0, 20.0}, 50, 15.0},
		{"sorted P50", []float64{10.0, 20.0, 30.0, 40.0, 50.0}, 50, 30.0},
		{"sorted P95", []float64{10.0, 20.0, 30.0, 40.0, 50.0}, 95, 48.0},
		{"sorted P99", []float64{10.0, 20.0, 30.0, 40.0, 50.0}, 99, 49.6},
		{"sorted P0", []float64{10.0, 20.0, 30.0}, 0, 10.0},
		{"sorted P100", []float64{10.0, 20.0, 30.0}, 100, 30.0},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := percentile(tc.values, tc.percentile)
			// Use approximate comparison for floating point
			diff := result - tc.expected
			if diff < -0.001 || diff > 0.001 {
				t.Errorf("percentile(%v, %v) = %v, expected %v", tc.values, tc.percentile, result, tc.expected)
			}
		})
	}
}

func TestCalculateSummary(t *testing.T) {
	opts := &benchmarkOptions{
		name:       "test-service",
		namespace:  "test-ns",
		iterations: 5,
		maxTokens:  50,
	}

	results := []BenchmarkResult{
		{
			Iteration: 1, TotalTimeMs: 100, GenerationToksPerSec: 50,
			PromptToksPerSec: 500, PromptTokens: 10, CompletionTokens: 20,
		},
		{
			Iteration: 2, TotalTimeMs: 110, GenerationToksPerSec: 55,
			PromptToksPerSec: 520, PromptTokens: 10, CompletionTokens: 22,
		},
		{
			Iteration: 3, TotalTimeMs: 90, GenerationToksPerSec: 60,
			PromptToksPerSec: 480, PromptTokens: 10, CompletionTokens: 18,
		},
		{
			Iteration: 4, TotalTimeMs: 120, GenerationToksPerSec: 45,
			PromptToksPerSec: 510, PromptTokens: 10, CompletionTokens: 25,
		},
		{
			Iteration: 5, TotalTimeMs: 95, GenerationToksPerSec: 52,
			PromptToksPerSec: 490, PromptTokens: 10, CompletionTokens: 19,
		},
	}

	summary := calculateSummary(opts, "http://localhost:8080", results, time.Now())

	// Verify basic fields
	if summary.ServiceName != "test-service" {
		t.Errorf("Expected service name 'test-service', got %s", summary.ServiceName)
	}

	if summary.Namespace != "test-ns" {
		t.Errorf("Expected namespace 'test-ns', got %s", summary.Namespace)
	}

	if summary.Iterations != 5 {
		t.Errorf("Expected 5 iterations, got %d", summary.Iterations)
	}

	if summary.SuccessfulRuns != 5 {
		t.Errorf("Expected 5 successful runs, got %d", summary.SuccessfulRuns)
	}

	if summary.FailedRuns != 0 {
		t.Errorf("Expected 0 failed runs, got %d", summary.FailedRuns)
	}

	// Verify latency calculations
	if summary.LatencyMin != 90 {
		t.Errorf("Expected min latency 90ms, got %.0f", summary.LatencyMin)
	}

	if summary.LatencyMax != 120 {
		t.Errorf("Expected max latency 120ms, got %.0f", summary.LatencyMax)
	}

	// Mean of [90, 95, 100, 110, 120] = 103
	expectedMean := 103.0
	if summary.LatencyMean != expectedMean {
		t.Errorf("Expected mean latency %.0fms, got %.0f", expectedMean, summary.LatencyMean)
	}
}

func TestCalculateSummaryWithFailures(t *testing.T) {
	opts := &benchmarkOptions{
		name:       "test-service",
		namespace:  "default",
		iterations: 5,
		maxTokens:  50,
	}

	results := []BenchmarkResult{
		{Iteration: 1, TotalTimeMs: 100, GenerationToksPerSec: 50, PromptTokens: 10},
		{Iteration: 2, Error: "connection refused"},
		{Iteration: 3, TotalTimeMs: 110, GenerationToksPerSec: 55, PromptTokens: 10},
		{Iteration: 4, Error: "timeout"},
		{Iteration: 5, TotalTimeMs: 90, GenerationToksPerSec: 60, PromptTokens: 10},
	}

	summary := calculateSummary(opts, "http://localhost:8080", results, time.Now())

	if summary.SuccessfulRuns != 3 {
		t.Errorf("Expected 3 successful runs, got %d", summary.SuccessfulRuns)
	}

	if summary.FailedRuns != 2 {
		t.Errorf("Expected 2 failed runs, got %d", summary.FailedRuns)
	}

	// Verify latency stats only include successful runs
	if summary.LatencyMin != 90 {
		t.Errorf("Expected min latency 90ms from successful runs, got %.0f", summary.LatencyMin)
	}

	if summary.LatencyMax != 110 {
		t.Errorf("Expected max latency 110ms from successful runs, got %.0f", summary.LatencyMax)
	}
}

func TestCalculateSummaryAllFailures(t *testing.T) {
	opts := &benchmarkOptions{
		name:       "test-service",
		namespace:  "default",
		iterations: 3,
		maxTokens:  50,
	}

	results := []BenchmarkResult{
		{Iteration: 1, Error: "connection refused"},
		{Iteration: 2, Error: "timeout"},
		{Iteration: 3, Error: "network error"},
	}

	summary := calculateSummary(opts, "http://localhost:8080", results, time.Now())

	if summary.SuccessfulRuns != 0 {
		t.Errorf("Expected 0 successful runs, got %d", summary.SuccessfulRuns)
	}

	if summary.FailedRuns != 3 {
		t.Errorf("Expected 3 failed runs, got %d", summary.FailedRuns)
	}

	// All stats should be 0
	if summary.LatencyMean != 0 {
		t.Errorf("Expected 0 mean latency for all failures, got %.0f", summary.LatencyMean)
	}
}

func TestNewBenchmarkCommand(t *testing.T) {
	cmd := NewBenchmarkCommand()

	if cmd.Use != "benchmark [SERVICE_NAME]" {
		t.Errorf("Unexpected command use: %s", cmd.Use)
	}

	// Check that all expected flags exist
	expectedFlags := []string{
		"namespace",
		"iterations",
		"warmup",
		"prompt",
		"max-tokens",
		"concurrent",
		"output",
		"endpoint",
		"timeout",
		"port-forward",
	}

	for _, flag := range expectedFlags {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("Expected flag '%s' not found", flag)
		}
	}
}

func TestBenchmarkCommandDefaults(t *testing.T) {
	cmd := NewBenchmarkCommand()

	// Verify default values
	testCases := []struct {
		flag     string
		expected string
	}{
		{"namespace", "default"},
		{"iterations", "10"},
		{"warmup", "2"},
		{"max-tokens", "50"},
		{"concurrent", "1"},
		{"output", "table"},
		{"endpoint", ""},
		{"timeout", "1m0s"},
		{"port-forward", "true"},
	}

	for _, tc := range testCases {
		t.Run(tc.flag, func(t *testing.T) {
			flag := cmd.Flags().Lookup(tc.flag)
			if flag == nil {
				t.Fatalf("Flag '%s' not found", tc.flag)
				return
			}
			if flag.DefValue != tc.expected {
				t.Errorf("Expected default value '%s' for flag '%s', got '%s'",
					tc.expected, tc.flag, flag.DefValue)
			}
		})
	}
}

func TestSendBenchmarkRequest(t *testing.T) {
	// Create a mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("Expected POST method, got %s", r.Method)
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("Expected path /v1/chat/completions, got %s", r.URL.Path)
		}

		// Return a mock response
		response := ChatCompletionResponse{
			ID:      "test-id",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   "test-model",
			Choices: []struct {
				Index   int `json:"index"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}{
				{
					Index: 0,
					Message: struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					}{
						Role:    "assistant",
						Content: "Machine learning is a subset of AI.",
					},
					FinishReason: "stop",
				},
			},
			Usage: struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}{
				PromptTokens:     10,
				CompletionTokens: 20,
				TotalTokens:      30,
			},
			Timings: struct {
				PromptN             int     `json:"prompt_n"`
				PromptMs            float64 `json:"prompt_ms"`
				PromptPerTokenMs    float64 `json:"prompt_per_token_ms"`
				PromptPerSecond     float64 `json:"prompt_per_second"`
				PredictedN          int     `json:"predicted_n"`
				PredictedMs         float64 `json:"predicted_ms"`
				PredictedPerTokenMs float64 `json:"predicted_per_token_ms"`
				PredictedPerSecond  float64 `json:"predicted_per_second"`
			}{
				PromptN:             10,
				PromptMs:            20.0,
				PromptPerTokenMs:    2.0,
				PromptPerSecond:     500.0,
				PredictedN:          20,
				PredictedMs:         400.0,
				PredictedPerTokenMs: 20.0,
				PredictedPerSecond:  50.0,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	opts := &benchmarkOptions{
		prompt:    "Test prompt",
		maxTokens: 50,
		timeout:   10 * time.Second,
	}

	result, err := sendBenchmarkRequest(t.Context(), server.URL, opts, 1)
	if err != nil {
		t.Fatalf("sendBenchmarkRequest failed: %v", err)
	}

	if result.Iteration != 1 {
		t.Errorf("Expected iteration 1, got %d", result.Iteration)
	}

	if result.PromptTokens != 10 {
		t.Errorf("Expected 10 prompt tokens, got %d", result.PromptTokens)
	}

	if result.CompletionTokens != 20 {
		t.Errorf("Expected 20 completion tokens, got %d", result.CompletionTokens)
	}

	if result.PromptToksPerSec != 500.0 {
		t.Errorf("Expected 500 prompt tok/s, got %.0f", result.PromptToksPerSec)
	}

	if result.GenerationToksPerSec != 50.0 {
		t.Errorf("Expected 50 generation tok/s, got %.0f", result.GenerationToksPerSec)
	}
}

func TestSendBenchmarkRequestError(t *testing.T) {
	// Create a mock server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Internal server error"))
	}))
	defer server.Close()

	opts := &benchmarkOptions{
		prompt:    "Test prompt",
		maxTokens: 50,
		timeout:   10 * time.Second,
	}

	_, err := sendBenchmarkRequest(t.Context(), server.URL, opts, 1)
	if err == nil {
		t.Fatal("Expected error for 500 response, got nil")
	}
	// Error should be in the err return, not result.Error
	// (result.Error is only set when we catch the error in the benchmark loop)
}

func TestOutputJSON(t *testing.T) {
	summary := BenchmarkSummary{
		ServiceName:              "test-service",
		Namespace:                "default",
		Endpoint:                 "http://localhost:8080",
		Iterations:               5,
		SuccessfulRuns:           5,
		FailedRuns:               0,
		LatencyMin:               90,
		LatencyMax:               120,
		LatencyMean:              100,
		LatencyP50:               100,
		LatencyP95:               115,
		LatencyP99:               119,
		GenerationToksPerSecMean: 55,
		GenerationToksPerSecMin:  45,
		GenerationToksPerSecMax:  65,
		Timestamp:                time.Now(),
		Duration:                 30 * time.Second,
	}

	// Capture output
	var buf bytes.Buffer
	original := json.NewEncoder(&buf)
	original.SetIndent("", "  ")
	err := original.Encode(summary)
	if err != nil {
		t.Fatalf("Failed to encode summary: %v", err)
	}

	// Verify it's valid JSON
	var decoded BenchmarkSummary
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("Failed to decode JSON output: %v", err)
	}

	if decoded.ServiceName != "test-service" {
		t.Errorf("Expected service name 'test-service', got %s", decoded.ServiceName)
	}

	if decoded.SuccessfulRuns != 5 {
		t.Errorf("Expected 5 successful runs, got %d", decoded.SuccessfulRuns)
	}
}

func TestBenchmarkResultJSONSerialization(t *testing.T) {
	result := BenchmarkResult{
		Iteration:            1,
		PromptTokens:         10,
		CompletionTokens:     20,
		TotalTokens:          30,
		PromptTimeMs:         25.5,
		GenerationTimeMs:     400.0,
		TotalTimeMs:          425.5,
		PromptToksPerSec:     392.16,
		GenerationToksPerSec: 50.0,
	}

	// Serialize
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Failed to marshal result: %v", err)
	}

	// Deserialize
	var decoded BenchmarkResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if decoded.Iteration != result.Iteration {
		t.Errorf("Expected iteration %d, got %d", result.Iteration, decoded.Iteration)
	}

	if decoded.GenerationToksPerSec != result.GenerationToksPerSec {
		t.Errorf("Expected %.2f tok/s, got %.2f", result.GenerationToksPerSec, decoded.GenerationToksPerSec)
	}
}

func TestBenchmarkResultWithError(t *testing.T) {
	result := BenchmarkResult{
		Iteration: 1,
		Error:     "connection timeout",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Failed to marshal result with error: %v", err)
	}

	// Verify error field is included
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if errVal, ok := decoded["error"]; !ok || errVal != "connection timeout" {
		t.Errorf("Expected error 'connection timeout' in JSON, got %v", decoded)
	}
}

func TestChatCompletionRequestSerialization(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "llama-3.1-8b",
		Messages: []ChatMessage{
			{Role: "user", Content: "Hello, world!"},
		},
		MaxTokens:   50,
		Temperature: 0.7,
		Stream:      false,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Failed to marshal request: %v", err)
	}

	// Verify structure
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal: %v", err)
	}

	if decoded["model"] != "llama-3.1-8b" {
		t.Errorf("Expected model 'llama-3.1-8b', got %v", decoded["model"])
	}

	messages, ok := decoded["messages"].([]interface{})
	if !ok || len(messages) != 1 {
		t.Errorf("Expected 1 message, got %v", decoded["messages"])
	}
}

func TestDefaultBenchmarkPrompt(t *testing.T) {
	if defaultBenchmarkPrompt == "" {
		t.Error("Default benchmark prompt should not be empty")
	}

	// Verify prompt is designed to generate a reasonable response
	if len(defaultBenchmarkPrompt) < 10 {
		t.Error("Default benchmark prompt seems too short")
	}
}

func TestFindAvailablePort(t *testing.T) {
	port, err := findAvailablePort()
	if err != nil {
		t.Fatalf("findAvailablePort() failed: %v", err)
	}

	// Port should be in valid range
	if port < 1024 || port > 65535 {
		t.Errorf("Expected port in range 1024-65535, got %d", port)
	}

	// Should be able to find multiple different ports
	port2, err := findAvailablePort()
	if err != nil {
		t.Fatalf("findAvailablePort() second call failed: %v", err)
	}

	// Ports should typically be different (not guaranteed but very likely)
	t.Logf("Found ports: %d, %d", port, port2)
}

func TestStressTestPrompts(t *testing.T) {
	if len(stressTestPrompts) == 0 {
		t.Error("stressTestPrompts should not be empty")
	}

	// Verify we have a mix of short, medium, and long prompts
	var short, medium, long int
	for _, p := range stressTestPrompts {
		switch {
		case len(p) < 50:
			short++
		case len(p) < 200:
			medium++
		default:
			long++
		}
	}

	if short == 0 {
		t.Error("Expected at least one short prompt")
	}
	if medium == 0 {
		t.Error("Expected at least one medium prompt")
	}
	if long == 0 {
		t.Error("Expected at least one long prompt")
	}
}

func TestLoadPromptsDefault(t *testing.T) {
	// Test that stress test mode uses varied prompts by default
	opts := &benchmarkOptions{
		prompt:     defaultBenchmarkPrompt,
		concurrent: 4,
	}

	prompts, err := loadPrompts(opts)
	if err != nil {
		t.Fatalf("loadPrompts failed: %v", err)
	}

	if len(prompts) != len(stressTestPrompts) {
		t.Errorf("Expected %d prompts for stress test, got %d", len(stressTestPrompts), len(prompts))
	}
}

func TestLoadPromptsCustomPrompt(t *testing.T) {
	// Test that custom prompt overrides defaults
	opts := &benchmarkOptions{
		prompt:     "My custom prompt",
		concurrent: 4,
	}

	prompts, err := loadPrompts(opts)
	if err != nil {
		t.Fatalf("loadPrompts failed: %v", err)
	}

	if len(prompts) != 1 {
		t.Errorf("Expected 1 prompt for custom prompt, got %d", len(prompts))
	}
	if prompts[0] != "My custom prompt" {
		t.Errorf("Expected 'My custom prompt', got '%s'", prompts[0])
	}
}

func TestLoadPromptsSingleMode(t *testing.T) {
	// Test that single benchmark mode uses single prompt
	opts := &benchmarkOptions{
		prompt:     defaultBenchmarkPrompt,
		concurrent: 1,
	}

	prompts, err := loadPrompts(opts)
	if err != nil {
		t.Fatalf("loadPrompts failed: %v", err)
	}

	if len(prompts) != 1 {
		t.Errorf("Expected 1 prompt for single mode, got %d", len(prompts))
	}
}

func TestSendBenchmarkRequestWithPrompt(t *testing.T) {
	// Create a mock server
	receivedPrompt := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			if len(req.Messages) > 0 {
				receivedPrompt = req.Messages[0].Content
			}
		}

		response := ChatCompletionResponse{
			ID:      "test-id",
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Model:   "test-model",
			Choices: []struct {
				Index   int `json:"index"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}{
				{
					Index: 0,
					Message: struct {
						Role    string `json:"role"`
						Content string `json:"content"`
					}{
						Role:    "assistant",
						Content: "Test response",
					},
					FinishReason: "stop",
				},
			},
			Usage: struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}{
				PromptTokens:     10,
				CompletionTokens: 20,
				TotalTokens:      30,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	opts := &benchmarkOptions{
		maxTokens: 50,
		timeout:   10 * time.Second,
	}

	customPrompt := "What is 2+2?"
	result, err := sendBenchmarkRequestWithPrompt(t.Context(), server.URL, opts, 1, customPrompt)
	if err != nil {
		t.Fatalf("sendBenchmarkRequestWithPrompt failed: %v", err)
	}

	if receivedPrompt != customPrompt {
		t.Errorf("Expected prompt '%s', got '%s'", customPrompt, receivedPrompt)
	}

	if result.Iteration != 1 {
		t.Errorf("Expected iteration 1, got %d", result.Iteration)
	}

	if result.PromptTokens != 10 {
		t.Errorf("Expected 10 prompt tokens, got %d", result.PromptTokens)
	}
}

func TestCalculateStressSummary(t *testing.T) {
	opts := &benchmarkOptions{
		name:       "test-service",
		namespace:  "test-ns",
		iterations: 100,
		maxTokens:  50,
		duration:   30 * time.Second,
	}

	// Create test results
	results := make([]BenchmarkResult, 100)
	for i := 0; i < 100; i++ {
		results[i] = BenchmarkResult{
			Iteration:            i + 1,
			TotalTimeMs:          float64(100 + i),
			GenerationToksPerSec: float64(45 + i%20),
			PromptToksPerSec:     float64(400 + i%50),
			PromptTokens:         10,
			CompletionTokens:     20,
		}
	}
	// Add some errors
	results[50].Error = "timeout"
	results[75].Error = "connection refused"

	startTime := time.Now().Add(-30 * time.Second)
	summary := calculateStressSummary(opts, "http://localhost:8080", results, startTime, 4)

	if summary.Concurrency != 4 {
		t.Errorf("Expected concurrency 4, got %d", summary.Concurrency)
	}

	if summary.TotalRequests != 100 {
		t.Errorf("Expected 100 total requests, got %d", summary.TotalRequests)
	}

	if summary.SuccessfulRuns != 98 {
		t.Errorf("Expected 98 successful runs, got %d", summary.SuccessfulRuns)
	}

	if summary.FailedRuns != 2 {
		t.Errorf("Expected 2 failed runs, got %d", summary.FailedRuns)
	}

	if summary.ErrorRate != 2.0 {
		t.Errorf("Expected error rate 2.0%%, got %.1f%%", summary.ErrorRate)
	}

	if summary.RequestsPerSec <= 0 {
		t.Errorf("Expected positive requests/sec, got %.2f", summary.RequestsPerSec)
	}

	if summary.PeakToksPerSec <= 0 {
		t.Errorf("Expected positive peak tok/s, got %.2f", summary.PeakToksPerSec)
	}
}

func TestStressTestSummaryJSONSerialization(t *testing.T) {
	summary := StressTestSummary{
		BenchmarkSummary: BenchmarkSummary{
			ServiceName:              "test-service",
			Namespace:                "default",
			Endpoint:                 "http://localhost:8080",
			Iterations:               100,
			SuccessfulRuns:           98,
			FailedRuns:               2,
			LatencyMin:               90,
			LatencyMax:               200,
			LatencyMean:              120,
			GenerationToksPerSecMean: 50,
		},
		Concurrency:      4,
		TargetDuration:   30 * time.Second,
		TotalRequests:    100,
		RequestsPerSec:   3.33,
		ErrorRate:        2.0,
		PeakToksPerSec:   65.0,
		ToksPerSecStdDev: 5.2,
	}

	data, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("Failed to marshal StressTestSummary: %v", err)
	}

	var decoded StressTestSummary
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal StressTestSummary: %v", err)
	}

	if decoded.Concurrency != 4 {
		t.Errorf("Expected concurrency 4, got %d", decoded.Concurrency)
	}

	if decoded.TotalRequests != 100 {
		t.Errorf("Expected 100 total requests, got %d", decoded.TotalRequests)
	}

	if decoded.ErrorRate != 2.0 {
		t.Errorf("Expected error rate 2.0, got %.1f", decoded.ErrorRate)
	}
}

func TestNewBenchmarkCommandStressTestFlags(t *testing.T) {
	cmd := NewBenchmarkCommand()

	// Check that stress test flags exist
	stressFlags := []string{
		"duration",
		"prompt-file",
	}

	for _, flag := range stressFlags {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("Expected stress test flag '%s' not found", flag)
		}
	}

	// Verify duration default is 0
	durationFlag := cmd.Flags().Lookup("duration")
	if durationFlag.DefValue != "0s" {
		t.Errorf("Expected duration default '0s', got '%s'", durationFlag.DefValue)
	}

	// Verify prompt-file default is empty
	promptFileFlag := cmd.Flags().Lookup("prompt-file")
	if promptFileFlag.DefValue != "" {
		t.Errorf("Expected prompt-file default '', got '%s'", promptFileFlag.DefValue)
	}
}

func TestIsPodReady(t *testing.T) {
	testCases := []struct {
		name     string
		pod      *corev1.Pod
		expected bool
	}{
		{
			name: "running pod with ready condition",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "running pod without ready condition",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:      corev1.PodRunning,
					Conditions: []corev1.PodCondition{},
				},
			},
			expected: false,
		},
		{
			name: "running pod with ready=false",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "pending pod",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "failed pod",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			expected: false,
		},
		{
			name: "succeeded pod",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodSucceeded,
				},
			},
			expected: false,
		},
		{
			name: "running pod with multiple conditions",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodScheduled,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   corev1.ContainersReady,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expected: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result := isPodReady(tc.pod)
			if result != tc.expected {
				t.Errorf("isPodReady() = %v, expected %v", result, tc.expected)
			}
		})
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	tests := []struct {
		name      string
		output    string
		wantNil   bool
		wantMemMB int
		wantUtil  int
		wantTemp  int
		wantPower int
	}{
		{
			name:      "single GPU",
			output:    "4096, 8192, 75, 65, 120.5",
			wantMemMB: 4096,
			wantUtil:  75,
			wantTemp:  65,
			wantPower: 120,
		},
		{
			name:      "two GPUs",
			output:    "4096, 8192, 75, 65, 120.5\n2048, 8192, 90, 70, 110.0",
			wantMemMB: 6144,
			wantUtil:  90,
			wantTemp:  70,
			wantPower: 230,
		},
		{
			name:    "empty output",
			output:  "",
			wantNil: true,
		},
		{
			name:      "minimal fields (3 fields)",
			output:    "2048, 8192, 50",
			wantMemMB: 2048,
			wantUtil:  50,
			wantTemp:  0,
			wantPower: 0,
		},
		{
			name:    "too few fields",
			output:  "2048, 8192",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseNvidiaSMI(tt.output)
			if tt.wantNil {
				if result != nil && result.MemoryUsedMB == 0 && result.UtilPercent == 0 {
					return
				}
				if result != nil && result.MemoryUsedMB > 0 {
					t.Errorf("parseNvidiaSMI(%q) = non-nil with data, want nil-equivalent", tt.output)
				}
				return
			}
			if result == nil {
				t.Fatalf("parseNvidiaSMI(%q) = nil, want non-nil", tt.output)
			}
			if result.MemoryUsedMB != tt.wantMemMB {
				t.Errorf("MemoryUsedMB = %d, want %d", result.MemoryUsedMB, tt.wantMemMB)
			}
			if result.UtilPercent != tt.wantUtil {
				t.Errorf("UtilPercent = %d, want %d", result.UtilPercent, tt.wantUtil)
			}
			if result.TempCelsius != tt.wantTemp {
				t.Errorf("TempCelsius = %d, want %d", result.TempCelsius, tt.wantTemp)
			}
			if result.PowerWatts != tt.wantPower {
				t.Errorf("PowerWatts = %d, want %d", result.PowerWatts, tt.wantPower)
			}
		})
	}
}

func TestParseSweepValues(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		expected  []int
		wantError bool
	}{
		{"valid CSV", "1,2,4,8", []int{1, 2, 4, 8}, false},
		{"single value", "16", []int{16}, false},
		{"with spaces", "1, 2, 4", []int{1, 2, 4}, false},
		{"empty string", "", nil, false},
		{"invalid value", "1,abc,3", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseSweepValues(tt.input)
			if tt.wantError {
				if err == nil {
					t.Errorf("parseSweepValues(%q) = nil error, want error", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSweepValues(%q) error = %v", tt.input, err)
			}
			if len(result) != len(tt.expected) {
				t.Fatalf("parseSweepValues(%q) length = %d, want %d", tt.input, len(result), len(tt.expected))
			}
			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("parseSweepValues(%q)[%d] = %d, want %d", tt.input, i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestOutputTable(t *testing.T) {
	summary := BenchmarkSummary{
		ServiceName:              "test-svc",
		Namespace:                "test-ns",
		Iterations:               10,
		SuccessfulRuns:           10,
		GenerationToksPerSecMean: 25.5,
		GenerationToksPerSecMin:  20.0,
		GenerationToksPerSecMax:  30.0,
		LatencyP50:               100.0,
		LatencyP95:               150.0,
		LatencyP99:               200.0,
		LatencyMin:               80.0,
		LatencyMax:               250.0,
		LatencyMean:              110.0,
		Duration:                 30 * time.Second,
	}

	// Capture stdout
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputTable(summary)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "Benchmark Results") {
		t.Error("outputTable should contain 'Benchmark Results'")
	}
	if !strings.Contains(output, "10/10") {
		t.Error("outputTable should show success rate")
	}
}

func TestOutputTableNoSuccess(t *testing.T) {
	summary := BenchmarkSummary{
		Iterations:     5,
		SuccessfulRuns: 0,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputTable(summary)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "No successful runs") {
		t.Error("outputTable should indicate no successful runs")
	}
}

func TestOutputMarkdown(t *testing.T) {
	summary := BenchmarkSummary{
		ServiceName:              "test-svc",
		Namespace:                "test-ns",
		Iterations:               5,
		SuccessfulRuns:           5,
		GenerationToksPerSecMean: 25.5,
		GenerationToksPerSecMin:  20.0,
		GenerationToksPerSecMax:  30.0,
		PromptToksPerSecMean:     100.0,
		LatencyP50:               100.0,
		LatencyP95:               150.0,
		LatencyP99:               200.0,
		LatencyMin:               80.0,
		LatencyMax:               250.0,
		LatencyMean:              110.0,
		Timestamp:                time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC),
		Duration:                 30 * time.Second,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputMarkdown(summary)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "# LLMKube Benchmark Results") {
		t.Error("outputMarkdown should contain markdown header")
	}
	if !strings.Contains(output, "test-svc") {
		t.Error("outputMarkdown should contain service name")
	}
	if !strings.Contains(output, "## Throughput") {
		t.Error("outputMarkdown should contain throughput section")
	}
	if !strings.Contains(output, "## Latency") {
		t.Error("outputMarkdown should contain latency section")
	}
	if !strings.Contains(output, "Prompt (tok/s)") {
		t.Error("outputMarkdown should contain prompt throughput when non-zero")
	}
}

func TestOutputMarkdownNoSuccess(t *testing.T) {
	summary := BenchmarkSummary{
		ServiceName:    "fail-svc",
		Namespace:      "test-ns",
		Iterations:     5,
		SuccessfulRuns: 0,
		Timestamp:      time.Now(),
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputMarkdown(summary)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "No successful runs") {
		t.Error("outputMarkdown should indicate no successful runs")
	}
}

func TestOutputStressTable(t *testing.T) {
	summary := StressTestSummary{
		BenchmarkSummary: BenchmarkSummary{
			ServiceName:              "stress-svc",
			Namespace:                "test-ns",
			Iterations:               100,
			SuccessfulRuns:           95,
			GenerationToksPerSecMean: 50.0,
			GenerationToksPerSecMin:  30.0,
			GenerationToksPerSecMax:  70.0,
			LatencyP50:               200.0,
			LatencyP95:               500.0,
			LatencyP99:               800.0,
			LatencyMin:               100.0,
			LatencyMax:               1000.0,
			LatencyMean:              250.0,
			Duration:                 60 * time.Second,
		},
		Concurrency:    4,
		TotalRequests:  100,
		RequestsPerSec: 1.67,
		ErrorRate:      5.0,
		PeakToksPerSec: 70.0,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputStressTable(summary)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "Stress Test Results") {
		t.Error("outputStressTable should contain header")
	}
	if !strings.Contains(output, "100") {
		t.Error("outputStressTable should show total requests")
	}
}

func TestOutputStressMarkdown(t *testing.T) {
	summary := StressTestSummary{
		BenchmarkSummary: BenchmarkSummary{
			ServiceName:              "stress-svc",
			Namespace:                "test-ns",
			Iterations:               100,
			SuccessfulRuns:           100,
			GenerationToksPerSecMean: 50.0,
			GenerationToksPerSecMin:  30.0,
			GenerationToksPerSecMax:  70.0,
			LatencyP50:               200.0,
			LatencyP95:               500.0,
			LatencyP99:               800.0,
			LatencyMin:               100.0,
			LatencyMax:               1000.0,
			LatencyMean:              250.0,
			Timestamp:                time.Now(),
			Duration:                 60 * time.Second,
		},
		Concurrency:    4,
		TotalRequests:  100,
		RequestsPerSec: 1.67,
		ErrorRate:      0.0,
		PeakToksPerSec: 70.0,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputStressMarkdown(summary)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "# LLMKube Stress Test Results") {
		t.Error("outputStressMarkdown should contain header")
	}
	if !strings.Contains(output, "## Throughput") {
		t.Error("outputStressMarkdown should contain throughput section")
	}
}

func TestOutputStressJSON(t *testing.T) {
	summary := StressTestSummary{
		BenchmarkSummary: BenchmarkSummary{
			ServiceName:    "test-svc",
			Namespace:      "test-ns",
			Iterations:     10,
			SuccessfulRuns: 10,
		},
		Concurrency:   2,
		TotalRequests: 10,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputStressJSON(summary)

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("outputStressJSON returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var decoded StressTestSummary
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("Failed to parse JSON output: %v", err)
	}
	if decoded.ServiceName != "test-svc" {
		t.Errorf("ServiceName = %q, want %q", decoded.ServiceName, "test-svc")
	}
}

func TestOutputComparisonTable(t *testing.T) {
	report := ComparisonReport{
		Models: []ModelBenchmark{
			{
				ModelID:              "llama-8b",
				ModelName:            "Llama 8B",
				ModelSize:            "8B",
				Status:               statusSuccess,
				GenerationToksPerSec: 25.0,
				LatencyP50Ms:         100.0,
				LatencyP99Ms:         200.0,
				VRAMEstimate:         "6GB",
			},
			{
				ModelID: "big-model",
				Status:  statusFailed,
				Error:   "insufficient GPU",
			},
		},
		Iterations:  5,
		MaxTokens:   256,
		GPUEnabled:  true,
		GPUCount:    1,
		Accelerator: "cuda",
		Duration:    2 * time.Minute,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputComparisonTable(report)

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("outputComparisonTable returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "Benchmark Comparison Results") {
		t.Error("outputComparisonTable should contain header")
	}
	if !strings.Contains(output, "1/2 benchmarked") {
		t.Error("outputComparisonTable should show success count")
	}
	if !strings.Contains(output, "insufficient GPU") {
		t.Error("outputComparisonTable should show errors")
	}
}

func TestOutputComparisonTableStressTest(t *testing.T) {
	report := ComparisonReport{
		Models: []ModelBenchmark{
			{
				ModelID:        "llama-8b",
				ModelSize:      "8B",
				Status:         statusSuccess,
				TotalRequests:  50,
				RequestsPerSec: 2.5,
				ErrorRate:      0.0,
			},
		},
		IsStressTest: true,
		Concurrency:  4,
		Iterations:   50,
		MaxTokens:    256,
		Duration:     time.Minute,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	_ = outputComparisonTable(report)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "Stress Test Comparison") {
		t.Error("outputComparisonTable should show stress test header")
	}
}

func TestOutputComparisonJSON(t *testing.T) {
	report := ComparisonReport{
		Models: []ModelBenchmark{
			{ModelID: "test", Status: statusSuccess},
		},
		Iterations: 5,
		MaxTokens:  256,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputComparisonJSON(report)

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("outputComparisonJSON returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var decoded ComparisonReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("Failed to parse JSON: %v", err)
	}
	if len(decoded.Models) != 1 {
		t.Errorf("Models count = %d, want 1", len(decoded.Models))
	}
}

func TestOutputComparisonMarkdown(t *testing.T) {
	report := ComparisonReport{
		Models: []ModelBenchmark{
			{
				ModelID:              "llama-8b",
				ModelName:            "Llama 8B",
				ModelSize:            "8B",
				Status:               statusSuccess,
				GenerationToksPerSec: 25.0,
				LatencyP50Ms:         100.0,
				LatencyP99Ms:         200.0,
				VRAMEstimate:         "6GB",
			},
		},
		Timestamp:  time.Now(),
		Iterations: 5,
		MaxTokens:  256,
		GPUEnabled: true,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := outputComparisonMarkdown(report)

	_ = w.Close()
	os.Stdout = old

	if err != nil {
		t.Fatalf("outputComparisonMarkdown returned error: %v", err)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "# LLMKube Benchmark Comparison") {
		t.Error("outputComparisonMarkdown should contain markdown header")
	}
}

func TestOutputSweepTable(t *testing.T) {
	report := SweepReport{
		SweepType: "Concurrency",
		Results: []SweepResult{
			{
				Value: "1",
				Summary: &BenchmarkSummary{
					Iterations:               5,
					SuccessfulRuns:           5,
					GenerationToksPerSecMean: 25.0,
					LatencyP50:               100.0,
					LatencyP99:               200.0,
				},
			},
			{
				Value: "4",
				Stress: &StressTestSummary{
					BenchmarkSummary: BenchmarkSummary{
						GenerationToksPerSecMean: 50.0,
						LatencyP50:               200.0,
						LatencyP99:               400.0,
					},
					TotalRequests:  20,
					RequestsPerSec: 5.0,
					ErrorRate:      2.0,
				},
			},
			{
				Value: "8",
				Error: "timeout",
			},
		},
		Duration: 30 * time.Second,
	}

	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	outputSweepTable(report)

	_ = w.Close()
	os.Stdout = old

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	output := buf.String()

	if !strings.Contains(output, "Concurrency Sweep Results") {
		t.Error("outputSweepTable should contain sweep type in header")
	}
}

func TestGetReportPath(t *testing.T) {
	t.Run("explicit path", func(t *testing.T) {
		opts := &benchmarkOptions{report: "/tmp/report.md"}
		path, err := getReportPath(opts)
		if err != nil {
			t.Fatalf("getReportPath error: %v", err)
		}
		if path != "/tmp/report.md" {
			t.Errorf("path = %q, want /tmp/report.md", path)
		}
	})

	t.Run("report dir", func(t *testing.T) {
		tmpDir := t.TempDir()
		opts := &benchmarkOptions{reportDir: filepath.Join(tmpDir, "reports")}
		path, err := getReportPath(opts)
		if err != nil {
			t.Fatalf("getReportPath error: %v", err)
		}
		if path == "" {
			t.Error("path should not be empty")
		}
		if !strings.HasPrefix(path, filepath.Join(tmpDir, "reports")) {
			t.Errorf("path %q should start with report dir", path)
		}
	})

	t.Run("no report", func(t *testing.T) {
		opts := &benchmarkOptions{}
		path, err := getReportPath(opts)
		if err != nil {
			t.Fatalf("getReportPath error: %v", err)
		}
		if path != "" {
			t.Errorf("path = %q, want empty", path)
		}
	})
}

func TestAvailableSuites(t *testing.T) {
	suites := AvailableSuites()

	expectedSuites := []string{"quick", "stress", "full", "context", "scaling"}
	for _, name := range expectedSuites {
		suite, ok := suites[name]
		if !ok {
			t.Errorf("Missing suite %q", name)
			continue
		}
		if suite.Name == "" {
			t.Errorf("Suite %q has empty Name", name)
		}
		if suite.Description == "" {
			t.Errorf("Suite %q has empty Description", name)
		}
		if len(suite.Phases) == 0 {
			t.Errorf("Suite %q has no phases", name)
		}
	}
}

func TestSuiteHelp(t *testing.T) {
	help := SuiteHelp()
	if help == "" {
		t.Error("SuiteHelp returned empty string")
	}
	if !strings.Contains(help, "quick") {
		t.Error("SuiteHelp should mention 'quick' suite")
	}
	if !strings.Contains(help, "full") {
		t.Error("SuiteHelp should mention 'full' suite")
	}
}

func TestNewGPUMonitor(t *testing.T) {
	gm := newGPUMonitor()
	if gm == nil {
		t.Fatal("newGPUMonitor returned nil")
	}
	if gm.metrics == nil {
		t.Error("metrics should be initialized")
	}
	if gm.stopChan == nil {
		t.Error("stopChan should be initialized")
	}
}

func TestGetHostname(t *testing.T) {
	hostname := getHostname()
	if hostname == "" {
		t.Error("getHostname returned empty string")
	}
}

func TestNewReportWriterNoPath(t *testing.T) {
	opts := &benchmarkOptions{}
	rw, err := newReportWriter(opts)
	if err != nil {
		t.Fatalf("newReportWriter error: %v", err)
	}
	if rw != nil {
		t.Error("newReportWriter should return nil when no report path is set")
	}
}

func TestNewReportWriterWithPath(t *testing.T) {
	tmpDir := t.TempDir()
	reportPath := filepath.Join(tmpDir, "test-report.md")
	opts := &benchmarkOptions{report: reportPath}

	rw, err := newReportWriter(opts)
	if err != nil {
		t.Fatalf("newReportWriter error: %v", err)
	}
	if rw == nil {
		t.Fatal("newReportWriter returned nil")
	}

	if err := rw.close(); err != nil {
		t.Errorf("close error: %v", err)
	}

	content, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("Failed to read report: %v", err)
	}
	if !strings.Contains(string(content), "# LLMKube Benchmark Report") {
		t.Error("Report should contain header")
	}
}

func TestReportWriterMethods(t *testing.T) {
	tmpDir := t.TempDir()
	reportPath := filepath.Join(tmpDir, "test-report.md")
	opts := &benchmarkOptions{report: reportPath, name: "test-svc", namespace: "test-ns"}

	rw, err := newReportWriter(opts)
	if err != nil {
		t.Fatalf("newReportWriter error: %v", err)
	}
	defer func() { _ = rw.close() }()

	if err := rw.writeSection("Test Section", "Test content here"); err != nil {
		t.Errorf("writeSection error: %v", err)
	}

	summary := &BenchmarkSummary{
		ServiceName:              "test-svc",
		Iterations:               5,
		SuccessfulRuns:           5,
		GenerationToksPerSecMean: 25.0,
		LatencyP50:               100.0,
		LatencyP95:               150.0,
		LatencyP99:               200.0,
		Duration:                 10 * time.Second,
	}
	if err := rw.writeBenchmarkResult(summary); err != nil {
		t.Errorf("writeBenchmarkResult error: %v", err)
	}

	stressSummary := &StressTestSummary{
		BenchmarkSummary: *summary,
		Concurrency:      4,
		TotalRequests:    20,
		RequestsPerSec:   2.0,
		ErrorRate:        5.0,
		PeakToksPerSec:   30.0,
	}
	if err := rw.writeStressResult(stressSummary); err != nil {
		t.Errorf("writeStressResult error: %v", err)
	}

	sweepReport := &SweepReport{
		SweepType: "Concurrency",
		Results: []SweepResult{
			{Value: "1", Summary: summary},
		},
		Duration: 30 * time.Second,
	}
	if err := rw.writeSweepResults(sweepReport); err != nil {
		t.Errorf("writeSweepResults error: %v", err)
	}

	gpuMetrics := []GPUMetric{
		{Timestamp: time.Now(), MemoryUsedMB: 4096, MemoryTotalMB: 8192, UtilPercent: 75},
	}
	if err := rw.writeGPUMetrics(gpuMetrics); err != nil {
		t.Errorf("writeGPUMetrics error: %v", err)
	}

	compReport := ComparisonReport{
		Models: []ModelBenchmark{
			{ModelID: "test", Status: statusSuccess, GenerationToksPerSec: 25.0},
		},
	}
	if err := rw.writeComparisonReport(compReport); err != nil {
		t.Errorf("writeComparisonReport error: %v", err)
	}
}
