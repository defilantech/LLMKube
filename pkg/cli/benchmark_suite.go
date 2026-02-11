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
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func AvailableSuites() map[string]BenchmarkSuite {
	return map[string]BenchmarkSuite{
		suiteQuick: {
			Name:        "quick",
			Description: "Fast validation (~10 min) - concurrent load + quick stress test",
			Phases: []SuitePhase{
				{
					Name:        "concurrent",
					Description: "Concurrent load test",
					Concurrency: []int{1, 2, 4},
					Duration:    2 * time.Minute,
				},
				{
					Name:        "stress",
					Description: "Quick stress test",
					Concurrency: []int{4},
					Duration:    5 * time.Minute,
				},
			},
		},
		suiteStress: {
			Name:        "stress",
			Description: "Stress focused (~1 hr) - preload + concurrent sweep + stability test",
			Phases: []SuitePhase{
				{
					Name:            "preload",
					Description:     "Preload model cache",
					PreloadRequired: true,
				},
				{
					Name:        "concurrency-sweep",
					Description: "Concurrency scaling test",
					Concurrency: []int{1, 2, 4, 8},
					Duration:    5 * time.Minute,
				},
				{
					Name:          "stability",
					Description:   "Long-running stability test",
					Concurrency:   []int{4},
					Duration:      30 * time.Minute,
					StabilityTest: true,
				},
			},
		},
		suiteFull: {
			Name:        "full",
			Description: "Comprehensive (~4 hr) - all tests including context and token sweeps",
			Phases: []SuitePhase{
				{
					Name:            "preload",
					Description:     "Preload model cache",
					PreloadRequired: true,
				},
				{
					Name:        "concurrency-sweep",
					Description: "Concurrency scaling test",
					Concurrency: []int{1, 2, 4, 8},
					Duration:    5 * time.Minute,
				},
				{
					Name:        "tokens-sweep",
					Description: "Generation length test",
					Concurrency: []int{4},
					MaxTokens:   []int{64, 256, 512, 1024, 2048},
					Duration:    3 * time.Minute,
				},
				{
					Name:         "context-sweep",
					Description:  "Context size test (redeploys)",
					Concurrency:  []int{4},
					ContextSizes: []int{4096, 8192, 16384, 32768},
					Duration:     5 * time.Minute,
				},
				{
					Name:          "stability",
					Description:   "Long-running stability test",
					Concurrency:   []int{4},
					Duration:      60 * time.Minute,
					StabilityTest: true,
				},
			},
		},
		suiteContext: {
			Name:        "context",
			Description: "Context length testing - sweep from 4K to 64K context sizes",
			Phases: []SuitePhase{
				{
					Name:         "context-sweep",
					Description:  "Context size sweep (redeploys for each)",
					Concurrency:  []int{4},
					ContextSizes: []int{4096, 8192, 16384, 32768, 65536},
					Duration:     5 * time.Minute,
				},
			},
		},
		suiteScaling: {
			Name:        "scaling",
			Description: "Multi-GPU efficiency - compare 1 GPU vs 2 GPU performance",
			Phases: []SuitePhase{
				{
					Name:        "single-gpu",
					Description: "Single GPU baseline",
					Concurrency: []int{1, 2, 4},
					GPUCounts:   []int32{1},
					Duration:    5 * time.Minute,
				},
				{
					Name:        "multi-gpu",
					Description: "Multi-GPU comparison",
					Concurrency: []int{1, 2, 4},
					GPUCounts:   []int32{2},
					Duration:    5 * time.Minute,
				},
			},
		},
	}
}

func SuiteHelp() string {
	suites := AvailableSuites()
	order := []string{suiteQuick, suiteStress, suiteFull, suiteContext, suiteScaling}

	var sb strings.Builder
	sb.WriteString("Available test suites:\n")
	for _, name := range order {
		suite := suites[name]
		sb.WriteString(fmt.Sprintf("  %-10s %s\n", suite.Name, suite.Description))
		for _, phase := range suite.Phases {
			sb.WriteString(fmt.Sprintf("             • %s\n", phase.Description))
		}
	}
	return sb.String()
}

func printSuiteHeader(suite BenchmarkSuite, modelIDs []string, opts *benchmarkOptions) {
	fmt.Printf("\n🧪 LLMKube Test Suite: %s\n", suite.Name)
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Description: %s\n", suite.Description)
	fmt.Printf("Models:      %s\n", strings.Join(modelIDs, ", "))
	fmt.Printf("Phases:      %d\n", len(suite.Phases))
	if opts.gpu {
		accelerator := opts.accelerator
		if accelerator == "" {
			accelerator = acceleratorCUDA
		}
		fmt.Printf("Accelerator: %s (GPU count: %d)\n", accelerator, opts.gpuCount)
	}
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")
}

func writeSuiteConfig(reportWriter *ReportWriter, suite BenchmarkSuite, modelIDs []string) {
	if reportWriter == nil {
		return
	}
	var header strings.Builder
	header.WriteString(fmt.Sprintf("**Suite:** %s  \n", suite.Name))
	header.WriteString(fmt.Sprintf("**Description:** %s  \n", suite.Description))
	header.WriteString(fmt.Sprintf("**Models:** %s  \n", strings.Join(modelIDs, ", ")))
	header.WriteString(fmt.Sprintf("**Phases:** %d  \n", len(suite.Phases)))
	_ = reportWriter.writeSection("Test Suite Configuration", header.String())
}

func runSuite(opts *benchmarkOptions) error {
	suites := AvailableSuites()
	suite, ok := suites[opts.suite]
	if !ok {
		validSuites := make([]string, 0, len(suites))
		for name := range suites {
			validSuites = append(validSuites, name)
		}
		sort.Strings(validSuites)
		return fmt.Errorf("unknown suite '%s'. Available: %s", opts.suite, strings.Join(validSuites, ", "))
	}

	ctx := context.Background()
	startTime := time.Now()

	modelIDs := strings.Split(opts.catalog, ",")
	for i := range modelIDs {
		modelIDs[i] = strings.TrimSpace(modelIDs[i])
	}

	catalogModels, err := validateCatalogModels(modelIDs)
	if err != nil {
		return err
	}

	printSuiteHeader(suite, modelIDs, opts)

	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return fmt.Errorf("failed to create report writer: %w", err)
	}
	writeSuiteConfig(reportWriter, suite, modelIDs)

	k8sClient, err := initK8sClient()
	if err != nil {
		return err
	}

	for phaseIdx, phase := range suite.Phases {
		fmt.Printf("\n")
		fmt.Printf("╔═══════════════════════════════════════════════════════════════╗\n")
		fmt.Printf("║ Phase %d/%d: %s\n", phaseIdx+1, len(suite.Phases), phase.Description)
		fmt.Printf("╚═══════════════════════════════════════════════════════════════╝\n\n")

		if phase.PreloadRequired {
			preloadCatalogModels(modelIDs, opts.namespace)
			continue
		}

		if err := runSuitePhase(ctx, k8sClient, &phase, modelIDs, catalogModels, opts, reportWriter); err != nil {
			fmt.Printf("   ⚠️  Phase failed: %v\n", err)
			if reportWriter != nil {
				_ = reportWriter.writeSection(
					fmt.Sprintf("Phase %d: %s", phaseIdx+1, phase.Name),
					fmt.Sprintf("**Status:** Failed  \n**Error:** %v\n", err),
				)
			}
		}
	}

	totalDuration := time.Since(startTime)

	fmt.Printf("\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("✅ Suite '%s' completed\n", suite.Name)
	fmt.Printf("   Total Duration: %s\n", totalDuration.Round(time.Second))
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")

	if reportWriter != nil {
		if err := reportWriter.close(); err != nil {
			return fmt.Errorf("failed to close report: %w", err)
		}
	}

	return nil
}

func runSuitePhase(
	ctx context.Context,
	k8sClient client.Client,
	phase *SuitePhase,
	modelIDs []string,
	catalogModels []*Model,
	opts *benchmarkOptions,
	reportWriter *ReportWriter,
) error {
	if len(phase.ContextSizes) > 0 {
		return runSuiteContextSweep(ctx, k8sClient, phase, modelIDs, catalogModels, opts, reportWriter)
	}

	if len(phase.MaxTokens) > 0 {
		return runSuiteTokensSweep(ctx, k8sClient, phase, modelIDs, catalogModels, opts, reportWriter)
	}

	if len(phase.GPUCounts) > 0 {
		return runSuiteGPUScaling(ctx, k8sClient, phase, modelIDs, catalogModels, opts, reportWriter)
	}

	return runSuiteConcurrencyPhase(ctx, k8sClient, phase, modelIDs, catalogModels, opts, reportWriter)
}

type phaseEndpoint struct {
	endpoint        string
	endpointCleanup func()
}

func deployCatalogForPhase(
	ctx context.Context, k8sClient client.Client, modelID string,
	catalogModel *Model, opts *benchmarkOptions,
) (*phaseEndpoint, error) {
	fmt.Printf("🚀 Deploying %s...\n", modelID)
	if err := deployModel(ctx, k8sClient, modelID, catalogModel, opts); err != nil {
		return nil, fmt.Errorf("deploy failed: %w", err)
	}

	fmt.Printf("⏳ Waiting for deployment...\n")
	if err := waitForDeployment(ctx, k8sClient, modelID, opts); err != nil {
		if opts.cleanup {
			_ = cleanupModel(ctx, k8sClient, modelID, opts)
		}
		return nil, fmt.Errorf("deployment timeout: %w", err)
	}
	fmt.Printf("   ✅ Ready\n\n")

	testOpts := *opts
	testOpts.name = modelID
	endpoint, cleanup, err := getEndpoint(ctx, &testOpts)
	if err != nil {
		if opts.cleanup {
			_ = cleanupModel(ctx, k8sClient, modelID, opts)
		}
		return nil, fmt.Errorf("endpoint error: %w", err)
	}

	return &phaseEndpoint{endpoint: endpoint, endpointCleanup: cleanup}, nil
}

func runSuiteConcurrencySweep(
	ctx context.Context, endpoint string, phase *SuitePhase,
	testOpts *benchmarkOptions, reportWriter *ReportWriter,
) {
	sweepReport := SweepReport{
		SweepType:  "Concurrency",
		Values:     make([]string, len(phase.Concurrency)),
		Results:    make([]SweepResult, 0, len(phase.Concurrency)),
		Timestamp:  time.Now(),
		GPUEnabled: testOpts.gpu,
	}
	for i, c := range phase.Concurrency {
		sweepReport.Values[i] = strconv.Itoa(c)
	}

	for _, concurrency := range phase.Concurrency {
		fmt.Printf("📊 Testing concurrency: %d (duration: %s)\n", concurrency, phase.Duration)

		runOpts := *testOpts
		runOpts.concurrent = concurrency
		runOpts.duration = phase.Duration

		result := runSweepIteration(ctx, endpoint, &runOpts, time.Now())
		result.Parameter = "concurrency"
		result.Value = strconv.Itoa(concurrency)

		sweepReport.Results = append(sweepReport.Results, result)
	}

	sweepReport.Duration = time.Since(sweepReport.Timestamp)
	outputSweepTable(sweepReport)

	if reportWriter != nil {
		_ = reportWriter.writeSweepResults(&sweepReport)
	}
}

func runSuiteConcurrencyPhase(
	ctx context.Context,
	k8sClient client.Client,
	phase *SuitePhase,
	modelIDs []string,
	catalogModels []*Model,
	opts *benchmarkOptions,
	reportWriter *ReportWriter,
) error {
	for idx, modelID := range modelIDs {
		pe, err := deployCatalogForPhase(ctx, k8sClient, modelID, catalogModels[idx], opts)
		if err != nil {
			return err
		}

		testOpts := *opts
		testOpts.name = modelID

		if len(phase.Concurrency) > 1 {
			runSuiteConcurrencySweep(ctx, pe.endpoint, phase, &testOpts, reportWriter)
		} else {
			concurrency := 4
			if len(phase.Concurrency) == 1 {
				concurrency = phase.Concurrency[0]
			}

			fmt.Printf("📊 Running stability test: %d concurrent, %s duration\n", concurrency, phase.Duration)

			runOpts := testOpts
			runOpts.concurrent = concurrency
			runOpts.duration = phase.Duration

			summary, err := runStressTestInternal(ctx, pe.endpoint, &runOpts, time.Now())
			if err != nil {
				return fmt.Errorf("stability test failed: %w", err)
			}

			outputStressTable(*summary)

			if reportWriter != nil {
				_ = reportWriter.writeStressResult(summary)
			}
		}

		if pe.endpointCleanup != nil {
			pe.endpointCleanup()
		}

		if opts.cleanup {
			fmt.Printf("🧹 Cleaning up %s...\n", modelID)
			_ = cleanupModel(ctx, k8sClient, modelID, opts)
		}
	}

	return nil
}

func runSuiteTokensSweep(
	ctx context.Context,
	k8sClient client.Client,
	phase *SuitePhase,
	modelIDs []string,
	catalogModels []*Model,
	opts *benchmarkOptions,
	reportWriter *ReportWriter,
) error {
	for idx, modelID := range modelIDs {
		catalogModel := catalogModels[idx]

		fmt.Printf("🚀 Deploying %s...\n", modelID)
		if err := deployModel(ctx, k8sClient, modelID, catalogModel, opts); err != nil {
			return fmt.Errorf("deploy failed: %w", err)
		}

		if err := waitForDeployment(ctx, k8sClient, modelID, opts); err != nil {
			if opts.cleanup {
				_ = cleanupModel(ctx, k8sClient, modelID, opts)
			}
			return err
		}
		fmt.Printf("   ✅ Ready\n\n")

		testOpts := *opts
		testOpts.name = modelID

		endpoint, endpointCleanup, err := getEndpoint(ctx, &testOpts)
		if err != nil {
			if opts.cleanup {
				_ = cleanupModel(ctx, k8sClient, modelID, opts)
			}
			return err
		}

		sweepReport := SweepReport{
			SweepType:  "Max Tokens",
			Values:     make([]string, len(phase.MaxTokens)),
			Results:    make([]SweepResult, 0, len(phase.MaxTokens)),
			Timestamp:  time.Now(),
			GPUEnabled: opts.gpu,
		}
		for i, t := range phase.MaxTokens {
			sweepReport.Values[i] = strconv.Itoa(t)
		}

		concurrency := 4
		if len(phase.Concurrency) > 0 {
			concurrency = phase.Concurrency[0]
		}

		for _, maxTokens := range phase.MaxTokens {
			fmt.Printf("📊 Testing max-tokens: %d\n", maxTokens)

			runOpts := testOpts
			runOpts.maxTokens = maxTokens
			runOpts.concurrent = concurrency
			runOpts.duration = phase.Duration

			result := SweepResult{
				Parameter: "max_tokens",
				Value:     strconv.Itoa(maxTokens),
			}

			summary, err := runStressTestInternal(ctx, endpoint, &runOpts, time.Now())
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Stress = summary
			}
			sweepReport.Results = append(sweepReport.Results, result)
		}

		sweepReport.Duration = time.Since(sweepReport.Timestamp)
		outputSweepTable(sweepReport)

		if reportWriter != nil {
			_ = reportWriter.writeSweepResults(&sweepReport)
		}

		if endpointCleanup != nil {
			endpointCleanup()
		}

		if opts.cleanup {
			fmt.Printf("🧹 Cleaning up %s...\n", modelID)
			_ = cleanupModel(ctx, k8sClient, modelID, opts)
		}
	}

	return nil
}

func runSuiteContextSweep(
	ctx context.Context,
	k8sClient client.Client,
	phase *SuitePhase,
	modelIDs []string,
	catalogModels []*Model,
	opts *benchmarkOptions,
	reportWriter *ReportWriter,
) error {
	for idx, modelID := range modelIDs {
		catalogModel := catalogModels[idx]

		sweepReport := SweepReport{
			SweepType:  "Context Size",
			Values:     make([]string, len(phase.ContextSizes)),
			Results:    make([]SweepResult, 0, len(phase.ContextSizes)),
			Timestamp:  time.Now(),
			GPUEnabled: opts.gpu,
		}
		for i, c := range phase.ContextSizes {
			sweepReport.Values[i] = strconv.Itoa(c)
		}

		for _, contextSize := range phase.ContextSizes {
			fmt.Printf("📊 Testing context size: %d\n", contextSize)

			testOpts := *opts
			testOpts.contextSize = int32(contextSize)
			testOpts.name = modelID

			result := SweepResult{
				Parameter: "context_size",
				Value:     strconv.Itoa(contextSize),
			}

			fmt.Printf("🚀 Deploying with context %d...\n", contextSize)
			if err := deployModel(ctx, k8sClient, modelID, catalogModel, &testOpts); err != nil {
				result.Error = fmt.Sprintf("deploy failed: %v", err)
				sweepReport.Results = append(sweepReport.Results, result)
				continue
			}

			if err := waitForDeployment(ctx, k8sClient, modelID, &testOpts); err != nil {
				result.Error = fmt.Sprintf("deployment timeout: %v", err)
				if opts.cleanup {
					_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
				}
				sweepReport.Results = append(sweepReport.Results, result)
				continue
			}
			fmt.Printf("   ✅ Ready\n")

			endpoint, endpointCleanup, err := getEndpoint(ctx, &testOpts)
			if err != nil {
				result.Error = fmt.Sprintf("endpoint error: %v", err)
				if opts.cleanup {
					_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
				}
				sweepReport.Results = append(sweepReport.Results, result)
				continue
			}

			concurrency := 4
			if len(phase.Concurrency) > 0 {
				concurrency = phase.Concurrency[0]
			}

			runOpts := testOpts
			runOpts.concurrent = concurrency
			runOpts.duration = phase.Duration

			summary, err := runStressTestInternal(ctx, endpoint, &runOpts, time.Now())
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Stress = summary
			}
			sweepReport.Results = append(sweepReport.Results, result)

			if endpointCleanup != nil {
				endpointCleanup()
			}

			if opts.cleanup {
				fmt.Printf("🧹 Cleaning up...\n")
				_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
			}
		}

		sweepReport.Duration = time.Since(sweepReport.Timestamp)
		outputSweepTable(sweepReport)

		if reportWriter != nil {
			_ = reportWriter.writeSweepResults(&sweepReport)
		}
	}

	return nil
}

func runSuiteGPUScaling(
	ctx context.Context,
	k8sClient client.Client,
	phase *SuitePhase,
	modelIDs []string,
	catalogModels []*Model,
	opts *benchmarkOptions,
	reportWriter *ReportWriter,
) error {
	for idx, modelID := range modelIDs {
		catalogModel := catalogModels[idx]

		sweepReport := SweepReport{
			SweepType:  fmt.Sprintf("GPU Scaling (%s)", phase.Name),
			Values:     make([]string, 0),
			Results:    make([]SweepResult, 0),
			Timestamp:  time.Now(),
			GPUEnabled: true,
		}

		for _, gpuCount := range phase.GPUCounts {
			testOpts := *opts
			testOpts.gpuCount = gpuCount
			testOpts.name = modelID

			fmt.Printf("🚀 Deploying with %d GPU(s)...\n", gpuCount)
			if err := deployModel(ctx, k8sClient, modelID, catalogModel, &testOpts); err != nil {
				return fmt.Errorf("deploy failed: %w", err)
			}

			if err := waitForDeployment(ctx, k8sClient, modelID, &testOpts); err != nil {
				if opts.cleanup {
					_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
				}
				return err
			}
			fmt.Printf("   ✅ Ready\n\n")

			endpoint, endpointCleanup, err := getEndpoint(ctx, &testOpts)
			if err != nil {
				if opts.cleanup {
					_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
				}
				return err
			}

			for _, concurrency := range phase.Concurrency {
				label := fmt.Sprintf("%dGPU-C%d", gpuCount, concurrency)
				sweepReport.Values = append(sweepReport.Values, label)

				fmt.Printf("📊 Testing %d GPU(s), concurrency %d\n", gpuCount, concurrency)

				runOpts := testOpts
				runOpts.concurrent = concurrency
				runOpts.duration = phase.Duration

				result := SweepResult{
					Parameter: "gpu_scaling",
					Value:     label,
				}

				summary, err := runStressTestInternal(ctx, endpoint, &runOpts, time.Now())
				if err != nil {
					result.Error = err.Error()
				} else {
					result.Stress = summary
				}
				sweepReport.Results = append(sweepReport.Results, result)
			}

			if endpointCleanup != nil {
				endpointCleanup()
			}

			if opts.cleanup {
				fmt.Printf("🧹 Cleaning up...\n")
				_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
			}
		}

		sweepReport.Duration = time.Since(sweepReport.Timestamp)
		outputSweepTable(sweepReport)

		if reportWriter != nil {
			_ = reportWriter.writeSweepResults(&sweepReport)
		}
	}

	return nil
}
