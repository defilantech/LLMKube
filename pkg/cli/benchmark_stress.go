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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var stressTestPrompts = []string{
	// Short prompts (fast prefill, test generation throughput)
	"What is 2+2?",
	"Name three colors.",
	"What is the capital of France?",
	"Say hello in Spanish.",
	// Medium prompts (balanced)
	"Explain what machine learning is in exactly three sentences.",
	"Write a haiku about programming.",
	"What are the main differences between Python and Go?",
	"Describe the water cycle in simple terms.",
	// Long prompts (stress prefill, test compute)
	"You are a senior software architect reviewing a microservices architecture. " +
		"Analyze the following scenario: A company wants to migrate from a monolithic " +
		"application to microservices. They currently have a single database serving all " +
		"components. The application handles user authentication, order processing, " +
		"inventory management, and reporting. What would be your recommended approach for " +
		"decomposing this system into microservices? Consider data consistency, service " +
		"boundaries, and communication patterns.",
	"Imagine you are explaining quantum computing to a college student studying computer " +
		"science. Cover the following topics in detail: qubits vs classical bits, " +
		"superposition, entanglement, quantum gates, and why quantum computers might be " +
		"faster for certain problems. Use analogies where helpful and provide concrete examples.",
	"Write a detailed technical specification for a distributed caching system that needs " +
		"to handle 100,000 requests per second with sub-millisecond latency. Include " +
		"considerations for cache invalidation strategies, replication, partitioning, " +
		"consistency models, and failure handling. The system should support both " +
		"read-through and write-through caching patterns.",
}

func makeStopCondition(opts *benchmarkOptions, iteration *int64) func() bool {
	if opts.duration > 0 {
		deadline := time.Now().Add(opts.duration)
		return func() bool {
			return time.Now().After(deadline)
		}
	}
	totalIterations := int64(opts.iterations)
	return func() bool {
		return atomic.LoadInt64(iteration) >= totalIterations
	}
}

func printStressProgress(
	opts *benchmarkOptions, startTime time.Time,
	completed, errors, totalToks int64,
) {
	elapsed := time.Since(startTime).Seconds()
	total := completed + errors
	rps := float64(total) / elapsed
	tps := float64(totalToks) / elapsed
	errRate := float64(0)
	if total > 0 {
		errRate = float64(errors) / float64(total) * 100
	}

	if opts.duration > 0 {
		remaining := opts.duration - time.Since(startTime)
		if remaining < 0 {
			remaining = 0
		}
		fmt.Printf("\r⏱  %s remaining | %d req (%.1f/s) | %.1f tok/s | %.1f%% errors     ",
			remaining.Round(time.Second), total, rps, tps, errRate)
	} else {
		fmt.Printf("\r📊 %d/%d (%.1f%%) | %.1f req/s | %.1f tok/s | %.1f%% errors     ",
			total, opts.iterations, float64(total)/float64(opts.iterations)*100, rps, tps, errRate)
	}
}

func runStressTestInternal(
	ctx context.Context, endpoint string, opts *benchmarkOptions, startTime time.Time,
) (*StressTestSummary, error) {
	prompts, err := loadPrompts(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to load prompts: %w", err)
	}

	concurrency := opts.concurrent
	if concurrency < 1 {
		concurrency = 1
	}

	fmt.Printf("\n🔥 LLMKube Stress Test\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Service:     %s\n", opts.name)
	fmt.Printf("Namespace:   %s\n", opts.namespace)
	fmt.Printf("Endpoint:    %s\n", endpoint)
	fmt.Printf("Concurrency: %d\n", concurrency)
	if opts.duration > 0 {
		fmt.Printf("Duration:    %s\n", opts.duration)
	} else {
		fmt.Printf("Iterations:  %d\n", opts.iterations)
	}
	fmt.Printf("Prompts:     %d variants\n", len(prompts))
	fmt.Printf("Max Tokens:  %d\n", opts.maxTokens)
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

	if opts.warmup > 0 {
		fmt.Printf("🔥 Running %d warmup requests...\n", opts.warmup)
		for i := 0; i < opts.warmup; i++ {
			_, err := sendBenchmarkRequestWithPrompt(ctx, endpoint, opts, i+1, prompts[i%len(prompts)])
			if err != nil {
				fmt.Printf("   Warmup %d: failed (%v)\n", i+1, err)
			} else {
				fmt.Printf("   Warmup %d: ok\n", i+1)
			}
		}
		fmt.Println()
	}

	var (
		results     []BenchmarkResult
		resultsMu   sync.Mutex
		completed   int64
		errors      int64
		totalToks   int64
		wg          sync.WaitGroup
		stopChan    = make(chan struct{})
		iteration   int64
		lastPrintAt = time.Now()
		printMu     sync.Mutex
	)

	stopCondition := makeStopCondition(opts, &iteration)
	if opts.duration > 0 {
		fmt.Printf("📊 Running stress test for %s with %d concurrent workers...\n\n", opts.duration, concurrency)
	} else {
		fmt.Printf("📊 Running %d iterations with %d concurrent workers...\n\n", opts.iterations, concurrency)
	}

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopChan:
					return
				default:
					if stopCondition() {
						return
					}

					i := int(atomic.AddInt64(&iteration, 1))
					prompt := prompts[(i-1)%len(prompts)]

					result, err := sendBenchmarkRequestWithPrompt(ctx, endpoint, opts, i, prompt)
					if err != nil {
						result = BenchmarkResult{
							Iteration: i,
							Error:     err.Error(),
						}
						atomic.AddInt64(&errors, 1)
					} else {
						atomic.AddInt64(&completed, 1)
						atomic.AddInt64(&totalToks, int64(result.CompletionTokens))
					}

					resultsMu.Lock()
					results = append(results, result)
					resultsMu.Unlock()

					printMu.Lock()
					if time.Since(lastPrintAt) >= 2*time.Second {
						printStressProgress(opts, startTime,
							atomic.LoadInt64(&completed),
							atomic.LoadInt64(&errors),
							atomic.LoadInt64(&totalToks))
						lastPrintAt = time.Now()
					}
					printMu.Unlock()
				}
			}
		}()
	}

	if opts.duration > 0 {
		time.Sleep(opts.duration)
		close(stopChan)
	}
	wg.Wait()
	fmt.Printf("\n\n")

	summary := calculateStressSummary(opts, endpoint, results, startTime, concurrency)
	return &summary, nil
}

func loadPrompts(opts *benchmarkOptions) ([]string, error) {
	if opts.promptFile != "" {
		data, err := os.ReadFile(opts.promptFile)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(data), "\n")
		prompts := make([]string, 0, len(lines))
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				prompts = append(prompts, line)
			}
		}
		if len(prompts) == 0 {
			return nil, fmt.Errorf("no prompts found in file %s", opts.promptFile)
		}
		return prompts, nil
	}

	// Use prompt flag if specified (single prompt mode)
	if opts.prompt != defaultBenchmarkPrompt {
		return []string{opts.prompt}, nil
	}

	// For stress tests, use built-in varied prompts by default
	if opts.concurrent > 1 || opts.duration > 0 {
		return stressTestPrompts, nil
	}

	// Default single prompt for regular benchmarks
	return []string{opts.prompt}, nil
}

func sendBenchmarkRequest(
	ctx context.Context, endpoint string, opts *benchmarkOptions, iteration int,
) (BenchmarkResult, error) {
	return sendBenchmarkRequestWithPrompt(ctx, endpoint, opts, iteration, opts.prompt)
}

func sendBenchmarkRequestWithPrompt(
	ctx context.Context, endpoint string, opts *benchmarkOptions, iteration int, prompt string,
) (BenchmarkResult, error) {
	result := BenchmarkResult{
		Iteration: iteration,
	}

	reqBody := ChatCompletionRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: prompt},
		},
		MaxTokens:   opts.maxTokens,
		Temperature: 0.7,
		Stream:      false,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return result, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/v1/chat/completions", bytes.NewReader(jsonBody))
	if err != nil {
		return result, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := &http.Client{Timeout: opts.timeout}
	reqStartTime := time.Now()

	resp, err := httpClient.Do(req)
	if err != nil {
		return result, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	totalTime := time.Since(reqStartTime)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return result, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var chatResp ChatCompletionResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return result, fmt.Errorf("failed to parse response: %w", err)
	}

	result.PromptTokens = chatResp.Usage.PromptTokens
	result.CompletionTokens = chatResp.Usage.CompletionTokens
	result.TotalTokens = chatResp.Usage.TotalTokens
	result.TotalTimeMs = float64(totalTime.Milliseconds())

	if chatResp.Timings.PromptMs > 0 {
		result.PromptTimeMs = chatResp.Timings.PromptMs
		result.GenerationTimeMs = chatResp.Timings.PredictedMs
		result.PromptToksPerSec = chatResp.Timings.PromptPerSecond
		result.GenerationToksPerSec = chatResp.Timings.PredictedPerSecond
	} else {
		result.GenerationTimeMs = result.TotalTimeMs
		if result.CompletionTokens > 0 && result.TotalTimeMs > 0 {
			result.GenerationToksPerSec = float64(result.CompletionTokens) / (result.TotalTimeMs / 1000.0)
		}
	}

	return result, nil
}
