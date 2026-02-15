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
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func parseSweepValues(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ",")
	values := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("invalid value '%s': %w", p, err)
		}
		values = append(values, v)
	}
	return values, nil
}

func runSweepIteration(ctx context.Context, endpoint string, opts *benchmarkOptions, startTime time.Time) SweepResult {
	var result SweepResult
	if opts.concurrent > 1 || opts.duration > 0 {
		summary, err := runStressTestInternal(ctx, endpoint, opts, startTime)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Stress = summary
		}
	} else {
		summary, err := runBenchmarkInternalWithEndpoint(ctx, endpoint, opts, startTime)
		if err != nil {
			result.Error = err.Error()
		} else {
			result.Summary = summary
		}
	}
	return result
}

func runConcurrencySweep(opts *benchmarkOptions) error {
	ctx := context.Background()
	startTime := time.Now()

	values, err := parseSweepValues(opts.concurrencySweep)
	if err != nil {
		return fmt.Errorf("invalid concurrency-sweep values: %w", err)
	}

	endpoint, cleanup, err := getEndpoint(ctx, opts)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return err
	}

	var gpuMon *gpuMonitor
	if opts.monitorGPU {
		gpuMon = newGPUMonitor()
		gpuMon.start(10 * time.Second)
	}

	fmt.Printf("\n🔄 Concurrency Sweep\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Service:     %s\n", opts.name)
	fmt.Printf("Values:      %v\n", values)
	if opts.duration > 0 {
		fmt.Printf("Duration:    %s per level\n", opts.duration)
	} else {
		fmt.Printf("Iterations:  %d per level\n", opts.iterations)
	}
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

	sweepReport := SweepReport{
		SweepType:  "Concurrency",
		Values:     make([]string, len(values)),
		Results:    make([]SweepResult, 0, len(values)),
		Timestamp:  startTime,
		GPUEnabled: opts.gpu,
	}
	for i, v := range values {
		sweepReport.Values[i] = strconv.Itoa(v)
	}

	for _, concurrency := range values {
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("📊 Testing concurrency: %d\n", concurrency)
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

		testOpts := *opts
		testOpts.concurrent = concurrency

		result := runSweepIteration(ctx, endpoint, &testOpts, time.Now())
		result.Parameter = "concurrency"
		result.Value = strconv.Itoa(concurrency)

		sweepReport.Results = append(sweepReport.Results, result)
		fmt.Println()
	}

	if gpuMon != nil {
		sweepReport.GPUMetrics = gpuMon.stop()
	}

	sweepReport.Duration = time.Since(startTime)
	outputSweepTable(sweepReport)

	if reportWriter != nil {
		if err := reportWriter.writeSweepResults(&sweepReport); err != nil {
			return fmt.Errorf("failed to write sweep results: %w", err)
		}
		if len(sweepReport.GPUMetrics) > 0 {
			if err := reportWriter.writeGPUMetrics(sweepReport.GPUMetrics); err != nil {
				return fmt.Errorf("failed to write GPU metrics: %w", err)
			}
		}
		if err := reportWriter.close(); err != nil {
			return fmt.Errorf("failed to close report: %w", err)
		}
	}

	return nil
}

func runTokensSweep(opts *benchmarkOptions) error {
	ctx := context.Background()
	startTime := time.Now()

	values, err := parseSweepValues(opts.tokensSweep)
	if err != nil {
		return fmt.Errorf("invalid tokens-sweep values: %w", err)
	}

	endpoint, cleanup, err := getEndpoint(ctx, opts)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return err
	}

	var gpuMon *gpuMonitor
	if opts.monitorGPU {
		gpuMon = newGPUMonitor()
		gpuMon.start(10 * time.Second)
	}

	fmt.Printf("\n🔄 Token Generation Sweep\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Service:     %s\n", opts.name)
	fmt.Printf("Values:      %v\n", values)
	fmt.Printf("Concurrency: %d\n", opts.concurrent)
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

	sweepReport := SweepReport{
		SweepType:  "Max Tokens",
		Values:     make([]string, len(values)),
		Results:    make([]SweepResult, 0, len(values)),
		Timestamp:  startTime,
		GPUEnabled: opts.gpu,
	}
	for i, v := range values {
		sweepReport.Values[i] = strconv.Itoa(v)
	}

	for _, maxTokens := range values {
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("📊 Testing max-tokens: %d\n", maxTokens)
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

		testOpts := *opts
		testOpts.maxTokens = maxTokens

		result := runSweepIteration(ctx, endpoint, &testOpts, time.Now())
		result.Parameter = "max_tokens"
		result.Value = strconv.Itoa(maxTokens)

		sweepReport.Results = append(sweepReport.Results, result)
		fmt.Println()
	}

	if gpuMon != nil {
		sweepReport.GPUMetrics = gpuMon.stop()
	}

	sweepReport.Duration = time.Since(startTime)
	outputSweepTable(sweepReport)

	if reportWriter != nil {
		if err := reportWriter.writeSweepResults(&sweepReport); err != nil {
			return err
		}
		if len(sweepReport.GPUMetrics) > 0 {
			_ = reportWriter.writeGPUMetrics(sweepReport.GPUMetrics)
		}
		if err := reportWriter.close(); err != nil {
			return err
		}
	}

	return nil
}

func runContextSweepIteration(
	ctx context.Context, k8sClient client.Client, modelID string,
	catalogModel *Model, contextSize int, opts *benchmarkOptions,
) SweepResult {
	result := SweepResult{
		Parameter: "context_size",
		Value:     strconv.Itoa(contextSize),
	}

	testOpts := *opts
	testOpts.contextSize = int32(contextSize)
	testOpts.name = modelID

	fmt.Printf("🚀 Deploying with context size %d...\n", contextSize)
	if err := deployModel(ctx, k8sClient, modelID, catalogModel, &testOpts); err != nil {
		result.Error = fmt.Sprintf("deploy failed: %v", err)
		fmt.Printf("   ❌ %s\n\n", result.Error)
		return result
	}

	fmt.Printf("⏳ Waiting for deployment...\n")
	if err := waitForDeployment(ctx, k8sClient, modelID, &testOpts); err != nil {
		result.Error = fmt.Sprintf("deployment timeout: %v", err)
		if opts.cleanup {
			_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
		}
		fmt.Printf("   ❌ %s\n\n", result.Error)
		return result
	}
	fmt.Printf("   ✅ Ready\n\n")

	endpoint, endpointCleanup, err := getEndpoint(ctx, &testOpts)
	if err != nil {
		result.Error = fmt.Sprintf("endpoint error: %v", err)
		if opts.cleanup {
			_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
		}
		return result
	}

	iterResult := runSweepIteration(ctx, endpoint, &testOpts, time.Now())
	result.Stress = iterResult.Stress
	result.Summary = iterResult.Summary
	result.Error = iterResult.Error

	if endpointCleanup != nil {
		endpointCleanup()
	}

	if opts.cleanup {
		fmt.Printf("🧹 Cleaning up...\n")
		if err := cleanupModel(ctx, k8sClient, modelID, &testOpts); err != nil {
			fmt.Printf("   ⚠️  %v\n", err)
		}
	}

	return result
}

func runContextSweep(opts *benchmarkOptions) error {
	if opts.catalog == "" {
		return fmt.Errorf("--context-sweep requires --catalog mode (deploys with different context sizes)")
	}

	ctx := context.Background()
	startTime := time.Now()

	values, err := parseSweepValues(opts.contextSweep)
	if err != nil {
		return fmt.Errorf("invalid context-sweep values: %w", err)
	}

	modelIDs := strings.Split(opts.catalog, ",")
	for i := range modelIDs {
		modelIDs[i] = strings.TrimSpace(modelIDs[i])
	}

	if len(modelIDs) > 1 {
		return fmt.Errorf("--context-sweep works with a single catalog model (got %d)", len(modelIDs))
	}

	modelID := modelIDs[0]
	catalogModel, err := GetModel(modelID)
	if err != nil {
		return fmt.Errorf("model '%s' not found in catalog: %w", modelID, err)
	}

	k8sClient, err := initK8sClient()
	if err != nil {
		return err
	}

	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return err
	}

	fmt.Printf("\n🔄 Context Size Sweep\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Model:       %s (%s)\n", catalogModel.Name, catalogModel.Size)
	fmt.Printf("Values:      %v\n", values)
	if opts.concurrent > 1 || opts.duration > 0 {
		fmt.Printf("Concurrency: %d\n", opts.concurrent)
	} else {
		fmt.Printf("Iterations:  %d\n", opts.iterations)
	}
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

	sweepReport := SweepReport{
		SweepType:  "Context Size",
		Values:     make([]string, len(values)),
		Results:    make([]SweepResult, 0, len(values)),
		Timestamp:  startTime,
		GPUEnabled: opts.gpu,
	}
	for i, v := range values {
		sweepReport.Values[i] = strconv.Itoa(v)
	}

	for _, contextSize := range values {
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("📊 Testing context size: %d\n", contextSize)
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

		result := runContextSweepIteration(ctx, k8sClient, modelID, catalogModel, contextSize, opts)
		sweepReport.Results = append(sweepReport.Results, result)
		fmt.Println()
	}

	sweepReport.Duration = time.Since(startTime)
	outputSweepTable(sweepReport)

	if reportWriter != nil {
		if err := reportWriter.writeSweepResults(&sweepReport); err != nil {
			return err
		}
		if err := reportWriter.close(); err != nil {
			return err
		}
	}

	return nil
}
