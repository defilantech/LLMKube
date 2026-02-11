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
	"os"
	"time"

	"github.com/spf13/cobra"
)

type benchmarkOptions struct {
	name        string
	namespace   string
	iterations  int
	warmup      int
	prompt      string
	maxTokens   int
	concurrent  int
	output      string
	endpoint    string
	timeout     time.Duration
	portForward bool
	duration    time.Duration
	promptFile  string

	catalog     string
	gpu         bool
	gpuCount    int32
	gpuLayers   int32
	accelerator string
	cleanup     bool
	deployWait  time.Duration
	contextSize int32

	// Report generation
	report    string
	reportDir string

	// Cache preloading
	preload bool

	// Sweep modes
	concurrencySweep string
	contextSweep     string
	tokensSweep      string

	// GPU monitoring
	monitorGPU bool

	// Test suites
	suite string
}

type BenchmarkResult struct {
	Iteration            int     `json:"iteration"`
	PromptTokens         int     `json:"prompt_tokens"`
	CompletionTokens     int     `json:"completion_tokens"`
	TotalTokens          int     `json:"total_tokens"`
	PromptTimeMs         float64 `json:"prompt_time_ms"`
	GenerationTimeMs     float64 `json:"generation_time_ms"`
	TotalTimeMs          float64 `json:"total_time_ms"`
	PromptToksPerSec     float64 `json:"prompt_tokens_per_sec"`
	GenerationToksPerSec float64 `json:"generation_tokens_per_sec"`
	Error                string  `json:"error,omitempty"`
}

type BenchmarkSummary struct {
	ServiceName    string `json:"service_name"`
	Namespace      string `json:"namespace"`
	Endpoint       string `json:"endpoint"`
	Iterations     int    `json:"iterations"`
	SuccessfulRuns int    `json:"successful_runs"`
	FailedRuns     int    `json:"failed_runs"`
	PromptTokens   int    `json:"prompt_tokens"`
	MaxTokens      int    `json:"max_tokens"`

	// Latency stats (in ms)
	LatencyMin  float64 `json:"latency_min_ms"`
	LatencyMax  float64 `json:"latency_max_ms"`
	LatencyMean float64 `json:"latency_mean_ms"`
	LatencyP50  float64 `json:"latency_p50_ms"`
	LatencyP95  float64 `json:"latency_p95_ms"`
	LatencyP99  float64 `json:"latency_p99_ms"`

	// Throughput stats
	PromptToksPerSecMean     float64 `json:"prompt_toks_per_sec_mean"`
	GenerationToksPerSecMean float64 `json:"generation_toks_per_sec_mean"`
	GenerationToksPerSecMin  float64 `json:"generation_toks_per_sec_min"`
	GenerationToksPerSecMax  float64 `json:"generation_toks_per_sec_max"`

	Results   []BenchmarkResult `json:"results"`
	Timestamp time.Time         `json:"timestamp"`
	Duration  time.Duration     `json:"duration"`
}

type ComparisonReport struct {
	Models         []ModelBenchmark `json:"models"`
	Timestamp      time.Time        `json:"timestamp"`
	Duration       time.Duration    `json:"duration"`
	Iterations     int              `json:"iterations"`
	MaxTokens      int              `json:"max_tokens"`
	GPUEnabled     bool             `json:"gpu_enabled"`
	GPUCount       int32            `json:"gpu_count,omitempty"`
	Accelerator    string           `json:"accelerator,omitempty"`
	IsStressTest   bool             `json:"is_stress_test,omitempty"`
	Concurrency    int              `json:"concurrency,omitempty"`
	TargetDuration time.Duration    `json:"target_duration,omitempty"`
}

type StressTestSummary struct {
	BenchmarkSummary
	Concurrency      int           `json:"concurrency"`
	TargetDuration   time.Duration `json:"target_duration,omitempty"`
	TotalRequests    int64         `json:"total_requests"`
	RequestsPerSec   float64       `json:"requests_per_sec"`
	ErrorRate        float64       `json:"error_rate"`
	PeakToksPerSec   float64       `json:"peak_toks_per_sec"`
	ToksPerSecStdDev float64       `json:"toks_per_sec_std_dev"`
}

type ModelBenchmark struct {
	ModelID              string  `json:"model_id"`
	ModelName            string  `json:"model_name"`
	ModelSize            string  `json:"model_size"`
	Status               string  `json:"status"` // "success", "failed", "skipped"
	Error                string  `json:"error,omitempty"`
	GenerationToksPerSec float64 `json:"generation_toks_per_sec"`
	PromptToksPerSec     float64 `json:"prompt_toks_per_sec"`
	LatencyP50Ms         float64 `json:"latency_p50_ms"`
	LatencyP99Ms         float64 `json:"latency_p99_ms"`
	VRAMEstimate         string  `json:"vram_estimate"`
	TotalRequests        int64   `json:"total_requests,omitempty"`
	RequestsPerSec       float64 `json:"requests_per_sec,omitempty"`
	ErrorRate            float64 `json:"error_rate,omitempty"`
}

type ChatCompletionRequest struct {
	Model       string        `json:"model,omitempty"`
	Messages    []ChatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Timings struct {
		PromptN             int     `json:"prompt_n"`
		PromptMs            float64 `json:"prompt_ms"`
		PromptPerTokenMs    float64 `json:"prompt_per_token_ms"`
		PromptPerSecond     float64 `json:"prompt_per_second"`
		PredictedN          int     `json:"predicted_n"`
		PredictedMs         float64 `json:"predicted_ms"`
		PredictedPerTokenMs float64 `json:"predicted_per_token_ms"`
		PredictedPerSecond  float64 `json:"predicted_per_second"`
	} `json:"timings"`
}

// ReportWriter handles generation of benchmark reports
type ReportWriter struct {
	file      *os.File
	startTime time.Time
	opts      *benchmarkOptions
}

// SweepResult holds results from a single sweep iteration
type SweepResult struct {
	Parameter string             `json:"parameter"`
	Value     string             `json:"value"`
	Summary   *BenchmarkSummary  `json:"summary,omitempty"`
	Stress    *StressTestSummary `json:"stress,omitempty"`
	Error     string             `json:"error,omitempty"`
}

// SweepReport holds results from a complete sweep test
type SweepReport struct {
	SweepType  string        `json:"sweep_type"`
	Values     []string      `json:"values"`
	Results    []SweepResult `json:"results"`
	Timestamp  time.Time     `json:"timestamp"`
	Duration   time.Duration `json:"duration"`
	GPUEnabled bool          `json:"gpu_enabled"`
	GPUMetrics []GPUMetric   `json:"gpu_metrics,omitempty"`
}

// GPUMetric holds a single GPU monitoring sample
type GPUMetric struct {
	Timestamp     time.Time `json:"timestamp"`
	MemoryUsedMB  int       `json:"memory_used_mb"`
	MemoryTotalMB int       `json:"memory_total_mb"`
	UtilPercent   int       `json:"util_percent"`
	TempCelsius   int       `json:"temp_celsius,omitempty"`
	PowerWatts    int       `json:"power_watts,omitempty"`
}

// BenchmarkSuite defines a predefined test suite
type BenchmarkSuite struct {
	Name        string
	Description string
	Phases      []SuitePhase
}

// SuitePhase defines a single phase within a test suite
type SuitePhase struct {
	Name            string
	Description     string
	Concurrency     []int
	Duration        time.Duration
	Iterations      int
	MaxTokens       []int
	ContextSizes    []int
	GPUCounts       []int32
	StabilityTest   bool
	PreloadRequired bool
}

const defaultBenchmarkPrompt = "Explain what machine learning is in exactly three sentences."

const (
	statusSuccess = "success"
	statusFailed  = "failed"
)

const (
	statusIconSuccess = "✅"
	statusIconFailed  = "❌"
)

const (
	outputFormatTable    = "table"
	outputFormatJSON     = "json"
	outputFormatMarkdown = "markdown"
)

const (
	phaseReady  = "Ready"
	phaseFailed = "Failed"
)

const (
	acceleratorCUDA  = "cuda"
	acceleratorMetal = "metal"
	acceleratorROCm  = "rocm"
	acceleratorCPU   = "cpu"
)

const (
	imageLlamaCppServer     = "ghcr.io/ggml-org/llama.cpp:server"
	imageLlamaCppServerCUDA = "ghcr.io/ggml-org/llama.cpp:server-cuda"
	imageLlamaCppServerROCm = "ghcr.io/ggml-org/llama.cpp:server-rocm"
)

// Suite names
const (
	suiteQuick   = "quick"
	suiteStress  = "stress"
	suiteFull    = "full"
	suiteContext = "context"
	suiteScaling = "scaling"
)

func NewBenchmarkCommand() *cobra.Command {
	opts := &benchmarkOptions{}

	cmd := &cobra.Command{
		Use:   "benchmark [SERVICE_NAME]",
		Short: "Benchmark an LLM inference service",
		Long: `Run performance benchmarks against a deployed LLM inference service.

This command sends test requests to the inference endpoint and measures:
- Prompt processing speed (tokens/sec)
- Generation speed (tokens/sec)
- Latency percentiles (P50, P95, P99)
- Request success rate

SINGLE SERVICE MODE:
  Benchmark an already-deployed inference service.

STRESS TEST MODE (--concurrent or --duration):
  Run concurrent requests to stress test the service. Automatically uses varied
  prompts (short, medium, long) to stress both prompt processing and generation.

CATALOG MODE (--catalog):
  Automatically deploy, benchmark, and compare multiple models from the catalog.
  Models are deployed sequentially, benchmarked, and optionally cleaned up.

TEST SUITES (--suite):
  Run predefined comprehensive test suites. Requires --catalog for model deployment.

  Available suites:
    quick     Fast validation (~10 min)
              • Concurrent load test (1,2,4 workers, 2min each)
              • Quick stress test (4 workers, 5min)

    stress    Stress focused (~1 hr)
              • Preload model cache
              • Concurrency sweep (1,2,4,8 workers, 5min each)
              • Stability test (4 workers, 30min)

    full      Comprehensive (~4 hr)
              • Preload model cache
              • Concurrency sweep (1,2,4,8 workers)
              • Token generation sweep (64-2048 tokens)
              • Context size sweep (4K-32K, redeploys)
              • Stability test (4 workers, 1hr)

    context   Context length testing
              • Context sweep (4K, 8K, 16K, 32K, 64K)

    scaling   Multi-GPU efficiency
              • Single GPU baseline (1,2,4 workers)
              • Multi-GPU comparison (1,2,4 workers)

SWEEP MODES:
  Test across multiple configurations automatically:
  --concurrency-sweep: Test multiple concurrency levels (e.g., 1,2,4,8)
  --context-sweep:     Test multiple context sizes (e.g., 4096,16384,32768)
  --tokens-sweep:      Test multiple generation lengths (e.g., 64,256,512)

REPORTING:
  Generate markdown reports with --report or --report-dir for analysis and sharing.

Examples:
  # Basic benchmark (sequential requests)
  llmkube benchmark my-llm -n default

  # TEST SUITE: Quick validation
  llmkube benchmark --suite quick --catalog llama-3.2-3b --gpu

  # TEST SUITE: Full comprehensive test with report
  llmkube benchmark --suite full --catalog qwen-2.5-32b --gpu --gpu-count 2 --report-dir ./reports

  # TEST SUITE: Stress test with preloading
  llmkube benchmark --suite stress --catalog mistral-7b --gpu --report stress-report.md

  # STRESS TEST: 8 concurrent requests for 30 minutes
  llmkube benchmark my-llm --concurrent 8 --duration 30m

  # STRESS TEST with report
  llmkube benchmark my-llm --concurrent 4 --duration 1h --report stress-test.md

  # Concurrency sweep - test scaling with report
  llmkube benchmark my-llm --concurrency-sweep 1,2,4,8 --duration 5m --report-dir ./reports

  # Context sweep - test different KV cache sizes
  llmkube benchmark --catalog qwen-2.5-32b --context-sweep 4096,16384,32768 --gpu

  # CATALOG MODE: Full report with preloading
  llmkube benchmark --catalog llama-3.2-3b,phi-4-mini --gpu --preload --report comparison.md
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Suite mode (requires catalog)
			if opts.suite != "" {
				if opts.catalog == "" {
					return fmt.Errorf("--suite requires --catalog to specify model(s) to test")
				}
				return runSuite(opts)
			}

			// Catalog mode
			if opts.catalog != "" {
				return runCatalogBenchmark(opts)
			}

			// Service name required for all other modes
			if len(args) == 0 {
				return fmt.Errorf("SERVICE_NAME is required (or use --catalog for multi-model comparison)")
			}
			opts.name = args[0]

			// Sweep modes (mutually exclusive)
			if opts.concurrencySweep != "" {
				return runConcurrencySweep(opts)
			}
			if opts.tokensSweep != "" {
				return runTokensSweep(opts)
			}
			if opts.contextSweep != "" {
				return runContextSweep(opts)
			}

			return runBenchmark(opts)
		},
	}

	// Flags
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().IntVarP(&opts.iterations, "iterations", "i", 10, "Number of benchmark iterations")
	cmd.Flags().IntVar(&opts.warmup, "warmup", 2, "Number of warmup requests (not counted)")
	cmd.Flags().StringVarP(&opts.prompt, "prompt", "p", defaultBenchmarkPrompt, "Prompt to use for benchmarking")
	cmd.Flags().IntVar(&opts.maxTokens, "max-tokens", 50, "Maximum tokens to generate per request")
	cmd.Flags().IntVarP(&opts.concurrent, "concurrent", "c", 1, "Number of concurrent requests for stress testing")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "table", "Output format: table, json, markdown")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "Override endpoint URL (default: auto-detect from service)")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 60*time.Second, "Request timeout")
	cmd.Flags().BoolVar(&opts.portForward, "port-forward", true, "Automatically set up port forwarding")
	cmd.Flags().DurationVar(&opts.duration, "duration", 0, "Run stress test for specified duration (e.g., 30m, 2h)")
	cmd.Flags().StringVar(&opts.promptFile, "prompt-file", "", "Load prompts from file (one per line) for varied workload")

	// Catalog mode flags
	cmd.Flags().StringVar(&opts.catalog, "catalog", "", "Comma-separated list of catalog model IDs to benchmark")
	cmd.Flags().BoolVar(&opts.gpu, "gpu", false, "Enable GPU acceleration for catalog deployments")
	cmd.Flags().Int32Var(&opts.gpuCount, "gpu-count", 1,
		"Number of GPUs per pod (for multi-GPU benchmarks)")
	cmd.Flags().Int32Var(&opts.gpuLayers, "gpu-layers", -1,
		"Number of model layers to offload to GPU (-1 = use catalog default)")
	cmd.Flags().StringVar(&opts.accelerator, "accelerator", "",
		"Hardware accelerator: cuda, metal, rocm (auto-detected if --gpu is set)")
	cmd.Flags().BoolVar(&opts.cleanup, "cleanup", true,
		"Cleanup deployments after benchmarking (use --no-cleanup to keep)")
	cmd.Flags().DurationVar(&opts.deployWait, "deploy-wait", 10*time.Minute, "Timeout waiting for deployment to be ready")
	cmd.Flags().Int32Var(&opts.contextSize, "context", 0,
		"Context size (KV cache) for model deployment (0 = use catalog default)")

	// Report generation flags
	cmd.Flags().StringVar(&opts.report, "report", "",
		"Generate markdown report to specified file path")
	cmd.Flags().StringVar(&opts.reportDir, "report-dir", "",
		"Directory for auto-timestamped reports (creates benchmark-YYYYMMDD-HHMMSS.md)")

	// Cache preloading flag
	cmd.Flags().BoolVar(&opts.preload, "preload", false,
		"Preload model cache before benchmarking (catalog mode only)")

	// Sweep mode flags
	cmd.Flags().StringVar(&opts.concurrencySweep, "concurrency-sweep", "",
		"Test multiple concurrency levels (comma-separated, e.g., 1,2,4,8)")
	cmd.Flags().StringVar(&opts.contextSweep, "context-sweep", "",
		"Test multiple context sizes (comma-separated, e.g., 4096,8192,16384)")
	cmd.Flags().StringVar(&opts.tokensSweep, "tokens-sweep", "",
		"Test multiple max-token values (comma-separated, e.g., 64,256,512,1024)")

	// GPU monitoring flag
	cmd.Flags().BoolVar(&opts.monitorGPU, "monitor-gpu", false,
		"Monitor GPU memory usage during benchmark (requires nvidia-smi)")

	// Test suite flag
	cmd.Flags().StringVar(&opts.suite, "suite", "",
		"Run predefined test suite: quick, stress, full, context, scaling (requires --catalog)")

	return cmd
}

func runWarmupRequests(ctx context.Context, endpoint string, opts *benchmarkOptions) {
	fmt.Printf("🔥 Running %d warmup requests...\n", opts.warmup)
	for i := 0; i < opts.warmup; i++ {
		_, err := sendBenchmarkRequest(ctx, endpoint, opts, i+1)
		if err != nil {
			fmt.Printf("   Warmup %d: failed (%v)\n", i+1, err)
		} else {
			fmt.Printf("   Warmup %d: ok\n", i+1)
		}
	}
	fmt.Println()
}

func runBenchmarkIterations(ctx context.Context, endpoint string, opts *benchmarkOptions) []BenchmarkResult {
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
	fmt.Println()
	return results
}

func runBenchmark(opts *benchmarkOptions) error {
	ctx := context.Background()
	startTime := time.Now()

	endpoint, cleanup, err := getEndpoint(ctx, opts)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return fmt.Errorf("failed to create report writer: %w", err)
	}

	var gpuMon *gpuMonitor
	if opts.monitorGPU {
		gpuMon = newGPUMonitor()
		gpuMon.start(10 * time.Second)
		defer func() {
			metrics := gpuMon.stop()
			if reportWriter != nil && len(metrics) > 0 {
				_ = reportWriter.writeGPUMetrics(metrics)
			}
		}()
	}

	if opts.concurrent > 1 || opts.duration > 0 {
		return runStressTestWithReport(ctx, endpoint, opts, startTime, reportWriter)
	}

	fmt.Printf("\n🏁 LLMKube Benchmark\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Service:     %s\n", opts.name)
	fmt.Printf("Namespace:   %s\n", opts.namespace)
	fmt.Printf("Endpoint:    %s\n", endpoint)
	fmt.Printf("Iterations:  %d (+ %d warmup)\n", opts.iterations, opts.warmup)
	fmt.Printf("Max Tokens:  %d\n", opts.maxTokens)
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

	if opts.warmup > 0 {
		runWarmupRequests(ctx, endpoint, opts)
	}

	results := runBenchmarkIterations(ctx, endpoint, opts)
	summary := calculateSummary(opts, endpoint, results, startTime)

	return outputBenchmarkResults(summary, opts, reportWriter)
}

func outputBenchmarkResults(summary BenchmarkSummary, opts *benchmarkOptions, reportWriter *ReportWriter) error {
	switch opts.output {
	case outputFormatJSON:
		if err := outputJSON(summary); err != nil {
			return err
		}
	case outputFormatMarkdown:
		outputMarkdown(summary)
	default:
		outputTable(summary)
	}

	if reportWriter != nil {
		if err := reportWriter.writeBenchmarkResult(&summary); err != nil {
			return fmt.Errorf("failed to write report: %w", err)
		}
		if err := reportWriter.close(); err != nil {
			return fmt.Errorf("failed to close report: %w", err)
		}
	}

	return nil
}

func runStressTestWithReport(
	ctx context.Context, endpoint string, opts *benchmarkOptions, startTime time.Time, reportWriter *ReportWriter,
) error {
	summary, err := runStressTestInternal(ctx, endpoint, opts, startTime)
	if err != nil {
		return err
	}

	switch opts.output {
	case outputFormatJSON:
		if err := outputStressJSON(*summary); err != nil {
			return err
		}
	case outputFormatMarkdown:
		outputStressMarkdown(*summary)
	default:
		outputStressTable(*summary)
	}

	if reportWriter != nil {
		if err := reportWriter.writeStressResult(summary); err != nil {
			return fmt.Errorf("failed to write report: %w", err)
		}
		if err := reportWriter.close(); err != nil {
			return fmt.Errorf("failed to close report: %w", err)
		}
	}

	return nil
}
