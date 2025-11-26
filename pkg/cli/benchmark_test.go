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
	"testing"
	"time"
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
		{Iteration: 1, TotalTimeMs: 100, GenerationToksPerSec: 50, PromptToksPerSec: 500, PromptTokens: 10, CompletionTokens: 20},
		{Iteration: 2, TotalTimeMs: 110, GenerationToksPerSec: 55, PromptToksPerSec: 520, PromptTokens: 10, CompletionTokens: 22},
		{Iteration: 3, TotalTimeMs: 90, GenerationToksPerSec: 60, PromptToksPerSec: 480, PromptTokens: 10, CompletionTokens: 18},
		{Iteration: 4, TotalTimeMs: 120, GenerationToksPerSec: 45, PromptToksPerSec: 510, PromptTokens: 10, CompletionTokens: 25},
		{Iteration: 5, TotalTimeMs: 95, GenerationToksPerSec: 52, PromptToksPerSec: 490, PromptTokens: 10, CompletionTokens: 19},
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
		json.NewEncoder(w).Encode(response)
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
		w.Write([]byte("Internal server error"))
	}))
	defer server.Close()

	opts := &benchmarkOptions{
		prompt:    "Test prompt",
		maxTokens: 50,
		timeout:   10 * time.Second,
	}

	result, err := sendBenchmarkRequest(t.Context(), server.URL, opts, 1)
	if err == nil {
		t.Fatal("Expected error for 500 response, got nil")
	}

	if result.Error != "" {
		// Error should be in the err return, not result.Error
		// (result.Error is only set when we catch the error in the benchmark loop)
	}
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
