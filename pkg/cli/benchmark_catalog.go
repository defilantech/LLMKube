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
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

func resolveAcceleratorDisplay(opts *benchmarkOptions) string {
	if !opts.gpu {
		return acceleratorCPU
	}
	if opts.accelerator != "" {
		return opts.accelerator
	}
	return acceleratorCUDA
}

func validateCatalogModels(modelIDs []string) ([]*Model, error) {
	fmt.Printf("\n🔍 Validating catalog models...\n")
	catalogModels := make([]*Model, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		model, err := GetModel(modelID)
		if err != nil {
			return nil, fmt.Errorf("model '%s' not found in catalog: %w", modelID, err)
		}
		catalogModels = append(catalogModels, model)
		fmt.Printf("   ✅ %s (%s)\n", modelID, model.Size)
	}
	return catalogModels, nil
}

func printCatalogBenchmarkHeader(opts *benchmarkOptions, modelIDs []string, acceleratorDisplay string) {
	fmt.Printf("\n🏁 LLMKube Catalog Benchmark\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Models:      %d (%s)\n", len(modelIDs), strings.Join(modelIDs, ", "))
	fmt.Printf("Namespace:   %s\n", opts.namespace)
	fmt.Printf("Accelerator: %s\n", acceleratorDisplay)
	if opts.gpu {
		fmt.Printf("GPU Count:   %d\n", opts.gpuCount)
		if opts.gpuLayers >= 0 {
			fmt.Printf("GPU Layers:  %d\n", opts.gpuLayers)
		} else {
			fmt.Printf("GPU Layers:  (catalog default)\n")
		}
	}
	if opts.concurrent > 1 || opts.duration > 0 {
		fmt.Printf("Mode:        Stress Test\n")
		fmt.Printf("Concurrency: %d\n", opts.concurrent)
		if opts.duration > 0 {
			fmt.Printf("Duration:    %s per model\n", opts.duration)
		} else {
			fmt.Printf("Iterations:  %d per model\n", opts.iterations)
		}
	} else {
		fmt.Printf("Iterations:  %d per model (+ %d warmup)\n", opts.iterations, opts.warmup)
	}
	fmt.Printf("Cleanup:     %v\n", opts.cleanup)
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")
}

func outputFormattedReport(report ComparisonReport, opts *benchmarkOptions, reportWriter *ReportWriter) error {
	fmt.Printf("\n")
	switch opts.output {
	case outputFormatJSON:
		if err := outputComparisonJSON(report); err != nil {
			return err
		}
	case outputFormatMarkdown:
		if err := outputComparisonMarkdown(report); err != nil {
			return err
		}
	default:
		if err := outputComparisonTable(report); err != nil {
			return err
		}
	}

	if reportWriter != nil {
		if err := reportWriter.writeComparisonReport(report); err != nil {
			return fmt.Errorf("failed to write report: %w", err)
		}
		if err := reportWriter.close(); err != nil {
			return fmt.Errorf("failed to close report: %w", err)
		}
	}
	return nil
}

func runCatalogBenchmark(opts *benchmarkOptions) error {
	if opts.contextSweep != "" {
		return runContextSweep(opts)
	}

	ctx := context.Background()
	startTime := time.Now()

	modelIDs := parseCatalogModelIDs(opts.catalog)

	catalogModels, err := validateCatalogModels(modelIDs)
	if err != nil {
		return err
	}

	acceleratorDisplay := resolveAcceleratorDisplay(opts)
	printCatalogBenchmarkHeader(opts, modelIDs, acceleratorDisplay)

	if opts.preload {
		preloadCatalogModels(modelIDs, opts.namespace)
	}

	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return fmt.Errorf("failed to create report writer: %w", err)
	}

	k8sClient, err := initK8sClient()
	if err != nil {
		return err
	}

	isStressTest := opts.concurrent > 1 || opts.duration > 0
	report := ComparisonReport{
		Models:         make([]ModelBenchmark, 0, len(modelIDs)),
		Timestamp:      startTime,
		Iterations:     opts.iterations,
		MaxTokens:      opts.maxTokens,
		GPUEnabled:     opts.gpu,
		GPUCount:       opts.gpuCount,
		Accelerator:    acceleratorDisplay,
		IsStressTest:   isStressTest,
		Concurrency:    opts.concurrent,
		TargetDuration: opts.duration,
	}

	for idx, modelID := range modelIDs {
		catalogModel := catalogModels[idx]

		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("📦 [%d/%d] Benchmarking: %s (%s)\n", idx+1, len(modelIDs), catalogModel.Name, catalogModel.Size)
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

		modelBenchmark := benchmarkSingleCatalogModel(ctx, k8sClient, modelID, catalogModel, opts, isStressTest)
		report.Models = append(report.Models, modelBenchmark)
		fmt.Println()
	}

	report.Duration = time.Since(startTime)
	return outputFormattedReport(report, opts, reportWriter)
}

func benchmarkSingleCatalogModel(
	ctx context.Context,
	k8sClient client.Client,
	modelID string,
	catalogModel *Model,
	opts *benchmarkOptions,
	isStressTest bool,
) ModelBenchmark {
	modelBenchmark := ModelBenchmark{
		ModelID:      modelID,
		ModelName:    catalogModel.Name,
		ModelSize:    catalogModel.Size,
		VRAMEstimate: catalogModel.VRAMEstimate,
	}

	fmt.Printf("🚀 Deploying %s...\n", modelID)
	if err := deployModel(ctx, k8sClient, modelID, catalogModel, opts); err != nil {
		fmt.Printf("   ❌ Deployment failed: %v\n\n", err)
		modelBenchmark.Status = statusFailed
		modelBenchmark.Error = fmt.Sprintf("deployment failed: %v", err)
		return modelBenchmark
	}

	fmt.Printf("⏳ Waiting for deployment to be ready...\n")
	if err := waitForDeployment(ctx, k8sClient, modelID, opts); err != nil {
		fmt.Printf("   ❌ Deployment not ready: %v\n", err)
		modelBenchmark.Status = statusFailed
		modelBenchmark.Error = fmt.Sprintf("deployment timeout: %v", err)
		if opts.cleanup {
			_ = cleanupModel(ctx, k8sClient, modelID, opts)
		}
		return modelBenchmark
	}
	fmt.Printf("   ✅ Deployment ready\n\n")

	opts.name = modelID
	endpoint, endpointCleanup, err := getEndpoint(ctx, opts)
	if err != nil {
		fmt.Printf("   ❌ Failed to get endpoint: %v\n\n", err)
		modelBenchmark.Status = statusFailed
		modelBenchmark.Error = fmt.Sprintf("endpoint error: %v", err)
		if opts.cleanup {
			_ = cleanupModel(ctx, k8sClient, modelID, opts)
		}
		return modelBenchmark
	}

	benchmarkStartTime := time.Now()
	if isStressTest {
		stressSummary, stressErr := runStressTestInternal(ctx, endpoint, opts, benchmarkStartTime)
		if stressErr != nil {
			fmt.Printf("   ❌ Benchmark failed: %v\n\n", stressErr)
			modelBenchmark.Status = statusFailed
			modelBenchmark.Error = fmt.Sprintf("benchmark failed: %v", stressErr)
		} else {
			modelBenchmark.Status = statusSuccess
			modelBenchmark.GenerationToksPerSec = stressSummary.GenerationToksPerSecMean
			modelBenchmark.PromptToksPerSec = stressSummary.PromptToksPerSecMean
			modelBenchmark.LatencyP50Ms = stressSummary.LatencyP50
			modelBenchmark.LatencyP99Ms = stressSummary.LatencyP99
			modelBenchmark.TotalRequests = stressSummary.TotalRequests
			modelBenchmark.RequestsPerSec = stressSummary.RequestsPerSec
			modelBenchmark.ErrorRate = stressSummary.ErrorRate
		}
	} else {
		summary, benchErr := runBenchmarkInternalWithEndpoint(ctx, endpoint, opts, benchmarkStartTime)
		if benchErr != nil {
			fmt.Printf("   ❌ Benchmark failed: %v\n\n", benchErr)
			modelBenchmark.Status = statusFailed
			modelBenchmark.Error = fmt.Sprintf("benchmark failed: %v", benchErr)
		} else {
			modelBenchmark.Status = statusSuccess
			modelBenchmark.GenerationToksPerSec = summary.GenerationToksPerSecMean
			modelBenchmark.PromptToksPerSec = summary.PromptToksPerSecMean
			modelBenchmark.LatencyP50Ms = summary.LatencyP50
			modelBenchmark.LatencyP99Ms = summary.LatencyP99
		}
	}

	if endpointCleanup != nil {
		endpointCleanup()
	}

	if opts.cleanup {
		fmt.Printf("🧹 Cleaning up %s...\n", modelID)
		if err := cleanupModel(ctx, k8sClient, modelID, opts); err != nil {
			fmt.Printf("   ⚠️  Cleanup warning: %v\n", err)
		} else {
			fmt.Printf("   ✅ Cleaned up\n")
		}
	}

	return modelBenchmark
}

func parseCatalogModelIDs(catalog string) []string {
	modelIDs := strings.Split(catalog, ",")
	for i := range modelIDs {
		modelIDs[i] = strings.TrimSpace(modelIDs[i])
	}
	return modelIDs
}

func resolveAccelerator(opts *benchmarkOptions) string {
	if opts.accelerator != "" {
		return opts.accelerator
	}
	if opts.gpu {
		return acceleratorCUDA
	}
	return ""
}

func resolveImage(accelerator string, gpuEnabled bool) string {
	if !gpuEnabled {
		return imageLlamaCppServer
	}
	switch accelerator {
	case acceleratorCUDA:
		return imageLlamaCppServerCUDA
	case acceleratorROCm:
		return imageLlamaCppServerROCm
	case acceleratorMetal:
		return "" // Metal uses native binary, not container
	default:
		return imageLlamaCppServerCUDA
	}
}

func gpuVendor(accelerator string) string {
	switch accelerator {
	case acceleratorROCm:
		return "amd"
	case acceleratorMetal:
		return "apple"
	default:
		return "nvidia"
	}
}

func buildModelResource(
	modelID string, catalogModel *Model, opts *benchmarkOptions, accelerator string,
) *inferencev1alpha1.Model {
	gpuLayers := catalogModel.GPULayers
	if opts.gpuLayers >= 0 {
		gpuLayers = opts.gpuLayers
	}

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelID,
			Namespace: opts.namespace,
			Labels: map[string]string{
				"llmkube.dev/benchmark": "true",
			},
		},
		Spec: inferencev1alpha1.ModelSpec{
			Source:       catalogModel.Source,
			Format:       "gguf",
			Quantization: catalogModel.Quantization,
			Resources: &inferencev1alpha1.ResourceRequirements{
				CPU:    catalogModel.Resources.CPU,
				Memory: catalogModel.Resources.Memory,
			},
		},
	}

	if opts.gpu {
		model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{
			Accelerator: accelerator,
			GPU: &inferencev1alpha1.GPUSpec{
				Enabled: true,
				Count:   opts.gpuCount,
				Vendor:  gpuVendor(accelerator),
				Layers:  gpuLayers,
				Memory:  catalogModel.Resources.GPUMemory,
			},
		}
	}

	return model
}

func deployModel(
	ctx context.Context,
	k8sClient client.Client,
	modelID string,
	catalogModel *Model,
	opts *benchmarkOptions,
) error {
	_ = cleanupModel(ctx, k8sClient, modelID, opts)
	time.Sleep(2 * time.Second)

	accelerator := resolveAccelerator(opts)
	model := buildModelResource(modelID, catalogModel, opts, accelerator)

	if err := k8sClient.Create(ctx, model); err != nil {
		return fmt.Errorf("failed to create Model: %w", err)
	}

	image := resolveImage(accelerator, opts.gpu)
	replicas := int32(1)
	inferenceService := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelID,
			Namespace: opts.namespace,
			Labels: map[string]string{
				"llmkube.dev/benchmark": "true",
			},
		},
		Spec: inferencev1alpha1.InferenceServiceSpec{
			ModelRef: modelID,
			Replicas: &replicas,
			Image:    image,
			Endpoint: &inferencev1alpha1.EndpointSpec{
				Port: 8080,
				Path: "/v1/chat/completions",
				Type: "ClusterIP",
			},
			Resources: &inferencev1alpha1.InferenceResourceRequirements{
				CPU:    catalogModel.Resources.CPU,
				Memory: catalogModel.Resources.Memory,
			},
		},
	}

	if opts.gpu {
		inferenceService.Spec.Resources.GPU = opts.gpuCount
		inferenceService.Spec.Resources.GPUMemory = catalogModel.Resources.GPUMemory
	}

	if opts.contextSize > 0 {
		inferenceService.Spec.ContextSize = &opts.contextSize
	} else if catalogModel.ContextSize > 0 {
		contextSize := int32(catalogModel.ContextSize)
		inferenceService.Spec.ContextSize = &contextSize
	}

	if err := k8sClient.Create(ctx, inferenceService); err != nil {
		_ = k8sClient.Delete(ctx, model)
		return fmt.Errorf("failed to create InferenceService: %w", err)
	}

	return nil
}

func waitForDeployment(ctx context.Context, k8sClient client.Client, modelID string, opts *benchmarkOptions) error {
	ctx, cancel := context.WithTimeout(ctx, opts.deployWait)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for deployment")
		case <-ticker.C:
			isvc := &inferencev1alpha1.InferenceService{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: modelID, Namespace: opts.namespace}, isvc); err != nil {
				continue
			}

			if isvc.Status.Phase == phaseReady {
				return nil
			}

			if isvc.Status.Phase == phaseFailed {
				return fmt.Errorf("deployment failed")
			}

			fmt.Printf("   Status: %s (%d/%d replicas)\n",
				isvc.Status.Phase, isvc.Status.ReadyReplicas, isvc.Status.DesiredReplicas)
		}
	}
}

func cleanupModel(ctx context.Context, k8sClient client.Client, modelID string, opts *benchmarkOptions) error {
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelID,
			Namespace: opts.namespace,
		},
	}
	if err := k8sClient.Delete(ctx, isvc); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("failed to delete InferenceService: %w", err)
		}
	}

	model := &inferencev1alpha1.Model{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelID,
			Namespace: opts.namespace,
		},
	}
	if err := k8sClient.Delete(ctx, model); err != nil {
		if !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("failed to delete Model: %w", err)
		}
	}

	return nil
}

func runBenchmarkInternalWithEndpoint(
	ctx context.Context, endpoint string, opts *benchmarkOptions, startTime time.Time,
) (*BenchmarkSummary, error) {
	if opts.warmup > 0 {
		fmt.Printf("🔥 Running %d warmup requests...\n", opts.warmup)
		for i := 0; i < opts.warmup; i++ {
			_, err := sendBenchmarkRequest(ctx, endpoint, opts, i+1)
			if err != nil {
				fmt.Printf("   Warmup %d: failed (%v)\n", i+1, err)
			} else {
				fmt.Printf("   Warmup %d: ok\n", i+1)
			}
		}
	}

	fmt.Printf("📊 Running %d benchmark iterations...\n", opts.iterations)
	results := make([]BenchmarkResult, 0, opts.iterations)

	for i := 0; i < opts.iterations; i++ {
		result, err := sendBenchmarkRequest(ctx, endpoint, opts, i+1)
		if err != nil {
			result = BenchmarkResult{
				Iteration: i + 1,
				Error:     err.Error(),
			}
			fmt.Printf("   [%d/%d] ❌ Error: %v\n", i+1, opts.iterations, err)
		} else {
			fmt.Printf("   [%d/%d] ✅ %.1f tok/s (%.0fms)\n",
				i+1, opts.iterations,
				result.GenerationToksPerSec,
				result.TotalTimeMs)
		}
		results = append(results, result)
	}

	summary := calculateSummary(opts, endpoint, results, startTime)
	if summary.SuccessfulRuns == 0 {
		return nil, fmt.Errorf("all %d benchmark iterations failed", opts.iterations)
	}
	return &summary, nil
}

func preloadCatalogModels(modelIDs []string, namespace string) {
	fmt.Printf("📦 Preloading %d model(s) to cache...\n", len(modelIDs))

	for _, modelID := range modelIDs {
		fmt.Printf("   Preloading %s...\n", modelID)
		if err := runCachePreload(modelID, namespace); err != nil {
			fmt.Printf("   ⚠️  Failed to preload %s: %v\n", modelID, err)
		} else {
			fmt.Printf("   ✅ %s cached\n", modelID)
		}
	}
	fmt.Println()
}
