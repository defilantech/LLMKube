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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
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

// AvailableSuites returns all predefined benchmark suites
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

// SuiteHelp returns a formatted help string for available suites
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
  llmkube benchmark --catalog llama-3.2-3b,phi-3-mini --gpu --preload --report comparison.md
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

	// Setup report writer
	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return fmt.Errorf("failed to create report writer: %w", err)
	}

	// Setup GPU monitoring if enabled
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

	// Use stress test mode if concurrent > 1 or duration is specified
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

	summary := calculateSummary(opts, endpoint, results, startTime)

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

	// Write report if enabled
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

	// Write report if enabled
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

	var stopCondition func() bool
	if opts.duration > 0 {
		deadline := time.Now().Add(opts.duration)
		stopCondition = func() bool {
			return time.Now().After(deadline)
		}
		fmt.Printf("📊 Running stress test for %s with %d concurrent workers...\n\n", opts.duration, concurrency)
	} else {
		totalIterations := int64(opts.iterations)
		stopCondition = func() bool {
			return atomic.LoadInt64(&iteration) >= totalIterations
		}
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
						c := atomic.LoadInt64(&completed)
						e := atomic.LoadInt64(&errors)
						t := atomic.LoadInt64(&totalToks)
						elapsed := time.Since(startTime).Seconds()

						rps := float64(c+e) / elapsed
						tps := float64(t) / elapsed
						errRate := float64(0)
						if c+e > 0 {
							errRate = float64(e) / float64(c+e) * 100
						}

						if opts.duration > 0 {
							remaining := opts.duration - time.Since(startTime)
							if remaining < 0 {
								remaining = 0
							}
							fmt.Printf("\r⏱  %s remaining | %d req (%.1f/s) | %.1f tok/s | %.1f%% errors     ",
								remaining.Round(time.Second), c+e, rps, tps, errRate)
						} else {
							fmt.Printf("\r📊 %d/%d (%.1f%%) | %.1f req/s | %.1f tok/s | %.1f%% errors     ",
								c+e, opts.iterations, float64(c+e)/float64(opts.iterations)*100, rps, tps, errRate)
						}
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

func calculateStressSummary(
	opts *benchmarkOptions, endpoint string, results []BenchmarkResult, startTime time.Time, concurrency int,
) StressTestSummary {
	baseSummary := calculateSummary(opts, endpoint, results, startTime)

	summary := StressTestSummary{
		BenchmarkSummary: baseSummary,
		Concurrency:      concurrency,
		TargetDuration:   opts.duration,
		TotalRequests:    int64(len(results)),
	}

	elapsed := time.Since(startTime).Seconds()
	if elapsed > 0 {
		summary.RequestsPerSec = float64(len(results)) / elapsed
	}

	if len(results) > 0 {
		summary.ErrorRate = float64(baseSummary.FailedRuns) / float64(len(results)) * 100
	}

	// Calculate peak and std dev for tok/s
	genToks := make([]float64, 0, len(results))
	for _, r := range results {
		if r.Error == "" && r.GenerationToksPerSec > 0 {
			genToks = append(genToks, r.GenerationToksPerSec)
		}
	}

	if len(genToks) > 0 {
		sort.Float64s(genToks)
		summary.PeakToksPerSec = genToks[len(genToks)-1]

		// Calculate std dev
		mean := summary.GenerationToksPerSecMean
		var sumSquares float64
		for _, v := range genToks {
			diff := v - mean
			sumSquares += diff * diff
		}
		summary.ToksPerSecStdDev = sumSquares / float64(len(genToks))
		if summary.ToksPerSecStdDev > 0 {
			summary.ToksPerSecStdDev = float64(int(summary.ToksPerSecStdDev*100)) / 100 // sqrt approximation via variance
		}
	}

	return summary
}

func outputStressTable(summary StressTestSummary) {
	fmt.Printf("📈 Stress Test Results\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

	// Overview
	fmt.Printf("OVERVIEW\n")
	fmt.Printf("────────\n")
	fmt.Printf("Total Requests:  %d\n", summary.TotalRequests)
	fmt.Printf("Success Rate:    %.1f%% (%d/%d)\n",
		100-summary.ErrorRate, summary.SuccessfulRuns, summary.TotalRequests)
	fmt.Printf("Duration:        %s\n", summary.Duration.Round(time.Second))
	fmt.Printf("Concurrency:     %d\n", summary.Concurrency)
	fmt.Printf("Requests/sec:    %.2f\n\n", summary.RequestsPerSec)

	if summary.SuccessfulRuns == 0 {
		fmt.Printf("❌ No successful runs to report.\n")
		return
	}

	// Throughput
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "THROUGHPUT\t\n")
	_, _ = fmt.Fprintf(w, "──────────\t\n")
	_, _ = fmt.Fprintf(w, "Generation:\t%.1f tok/s (mean)\t%.1f - %.1f (range)\n",
		summary.GenerationToksPerSecMean,
		summary.GenerationToksPerSecMin,
		summary.GenerationToksPerSecMax)
	_, _ = fmt.Fprintf(w, "Peak:\t%.1f tok/s\t\n", summary.PeakToksPerSec)
	if summary.PromptToksPerSecMean > 0 {
		_, _ = fmt.Fprintf(w, "Prompt:\t%.1f tok/s (mean)\t\n", summary.PromptToksPerSecMean)
	}
	_ = w.Flush()

	fmt.Println()

	// Latency
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "LATENCY\t\n")
	_, _ = fmt.Fprintf(w, "───────\t\n")
	_, _ = fmt.Fprintf(w, "P50:\t%.0f ms\t\n", summary.LatencyP50)
	_, _ = fmt.Fprintf(w, "P95:\t%.0f ms\t\n", summary.LatencyP95)
	_, _ = fmt.Fprintf(w, "P99:\t%.0f ms\t\n", summary.LatencyP99)
	_, _ = fmt.Fprintf(w, "Min:\t%.0f ms\t\n", summary.LatencyMin)
	_, _ = fmt.Fprintf(w, "Max:\t%.0f ms\t\n", summary.LatencyMax)
	_, _ = fmt.Fprintf(w, "Mean:\t%.0f ms\t\n", summary.LatencyMean)
	_ = w.Flush()

	fmt.Printf("\n═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Max tokens per request: %d\n", summary.MaxTokens)
}

func outputStressJSON(summary StressTestSummary) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func outputStressMarkdown(summary StressTestSummary) {
	fmt.Printf("# LLMKube Stress Test Results\n\n")
	fmt.Printf("**Service:** %s  \n", summary.ServiceName)
	fmt.Printf("**Namespace:** %s  \n", summary.Namespace)
	fmt.Printf("**Date:** %s  \n\n", summary.Timestamp.Format("2006-01-02 15:04:05"))

	fmt.Printf("## Overview\n\n")
	fmt.Printf("| Metric | Value |\n")
	fmt.Printf("|--------|-------|\n")
	fmt.Printf("| Total Requests | %d |\n", summary.TotalRequests)
	fmt.Printf("| Success Rate | %.1f%% |\n", 100-summary.ErrorRate)
	fmt.Printf("| Duration | %s |\n", summary.Duration.Round(time.Second))
	fmt.Printf("| Concurrency | %d |\n", summary.Concurrency)
	fmt.Printf("| Requests/sec | %.2f |\n\n", summary.RequestsPerSec)

	if summary.SuccessfulRuns == 0 {
		fmt.Printf("No successful runs to report.\n")
		return
	}

	fmt.Printf("## Throughput\n\n")
	fmt.Printf("| Metric | Mean | Min | Max | Peak |\n")
	fmt.Printf("|--------|------|-----|-----|------|\n")
	fmt.Printf("| Generation (tok/s) | %.1f | %.1f | %.1f | %.1f |\n",
		summary.GenerationToksPerSecMean,
		summary.GenerationToksPerSecMin,
		summary.GenerationToksPerSecMax,
		summary.PeakToksPerSec)
	if summary.PromptToksPerSecMean > 0 {
		fmt.Printf("| Prompt (tok/s) | %.1f | - | - | - |\n", summary.PromptToksPerSecMean)
	}

	fmt.Printf("\n## Latency\n\n")
	fmt.Printf("| Percentile | Value (ms) |\n")
	fmt.Printf("|------------|------------|\n")
	fmt.Printf("| P50 | %.0f |\n", summary.LatencyP50)
	fmt.Printf("| P95 | %.0f |\n", summary.LatencyP95)
	fmt.Printf("| P99 | %.0f |\n", summary.LatencyP99)
	fmt.Printf("| Min | %.0f |\n", summary.LatencyMin)
	fmt.Printf("| Max | %.0f |\n", summary.LatencyMax)
	fmt.Printf("| Mean | %.0f |\n", summary.LatencyMean)

	fmt.Printf("\n---\n")
	fmt.Printf("*Generated by LLMKube v%s*\n", Version)
}

func getEndpoint(ctx context.Context, opts *benchmarkOptions) (string, func(), error) {
	if opts.endpoint != "" {
		return opts.endpoint, nil, nil
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return "", nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return "", nil, fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return "", nil, fmt.Errorf("failed to create client: %w", err)
	}

	isvc := &inferencev1alpha1.InferenceService{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: opts.name, Namespace: opts.namespace}, isvc); err != nil {
		return "", nil, fmt.Errorf("failed to get InferenceService '%s': %w", opts.name, err)
	}

	if isvc.Status.Phase != phaseReady {
		return "", nil, fmt.Errorf("InferenceService '%s' is not ready (phase: %s)", opts.name, isvc.Status.Phase)
	}

	if opts.portForward {
		return setupPortForward(opts)
	}

	if isvc.Status.Endpoint != "" {
		return isvc.Status.Endpoint, nil, nil
	}

	return "", nil, fmt.Errorf(
		"no endpoint found for service '%s'. Use --endpoint to specify manually or --port-forward",
		opts.name)
}

func setupPortForward(opts *benchmarkOptions) (string, func(), error) {
	klog.SetOutput(io.Discard)
	klog.LogToStderr(false)

	serviceName := strings.ReplaceAll(opts.name, ".", "-")

	fmt.Printf("⚡ Port forwarding to service/%s...\n", serviceName)

	restConfig, err := config.GetConfig()
	if err != nil {
		return "", nil, fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	podName, err := findReadyPodForService(clientset, opts.namespace, serviceName)
	if err != nil {
		return "", nil, fmt.Errorf("failed to find pod for service %s: %w", serviceName, err)
	}

	localPort, err := findAvailablePort()
	if err != nil {
		return "", nil, fmt.Errorf("failed to find available port: %w", err)
	}

	stopChan := make(chan struct{}, 1)
	readyChan := make(chan struct{})
	errChan := make(chan error, 1)

	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", opts.namespace, podName)
	hostIP := strings.TrimPrefix(restConfig.Host, "https://")
	hostIP = strings.TrimPrefix(hostIP, "http://")

	serverURL := url.URL{Scheme: "https", Host: hostIP, Path: path}
	if strings.HasPrefix(restConfig.Host, "http://") {
		serverURL.Scheme = "http"
	}

	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create SPDY transport: %w", err)
	}

	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, &serverURL)

	ports := []string{fmt.Sprintf("%d:8080", localPort)}

	pf, err := portforward.New(dialer, ports, stopChan, readyChan, io.Discard, io.Discard)
	if err != nil {
		return "", nil, fmt.Errorf("failed to create port forwarder: %w", err)
	}

	go func() {
		if err := pf.ForwardPorts(); err != nil {
			errChan <- err
		}
	}()

	select {
	case <-readyChan:
	case err := <-errChan:
		return "", nil, fmt.Errorf("port forward failed: %w", err)
	case <-time.After(10 * time.Second):
		close(stopChan)
		return "", nil, fmt.Errorf("timeout waiting for port forward to be ready")
	}

	endpoint := fmt.Sprintf("http://localhost:%d", localPort)
	cleanup := func() {
		close(stopChan)
	}

	httpClient := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for i := 0; i < 5; i++ {
		resp, err := httpClient.Get(endpoint + "/health")
		if err == nil {
			_ = resp.Body.Close()
			fmt.Printf("   ✅ Connected on port %d\n", localPort)
			break
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
		if i == 4 {
			cleanup()
			return "", nil, fmt.Errorf("cannot connect to %s after port forward: %w", endpoint, lastErr)
		}
	}

	fmt.Printf("   ⏳ Waiting for model to load...\n")
	modelLoadTimeout := 10 * time.Minute
	startTime := time.Now()
	lastStatus := 0
	for {
		if time.Since(startTime) > modelLoadTimeout {
			cleanup()
			return "", nil, fmt.Errorf("timeout waiting for model to load (last status: %d)", lastStatus)
		}

		resp, err := httpClient.Get(endpoint + "/health")
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}

		lastStatus = resp.StatusCode
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			fmt.Printf("   ✅ Model loaded (took %s)\n\n", time.Since(startTime).Round(time.Second))
			return endpoint, cleanup, nil
		}

		time.Sleep(2 * time.Second)
	}
}

func findReadyPodForService(clientset *kubernetes.Clientset, namespace, serviceName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := clientset.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get service: %w", err)
	}

	selectors := make([]string, 0, len(svc.Spec.Selector))
	for k, v := range svc.Spec.Selector {
		selectors = append(selectors, fmt.Sprintf("%s=%s", k, v))
	}
	labelSelector := strings.Join(selectors, ",")

	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	for _, pod := range pods.Items {
		if isPodReady(&pod) {
			return pod.Name, nil
		}
	}

	return "", fmt.Errorf("no ready pods found for service %s", serviceName)
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func findAvailablePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port, nil
}

func sendBenchmarkRequest(
	ctx context.Context, endpoint string, opts *benchmarkOptions, iteration int,
) (BenchmarkResult, error) {
	result := BenchmarkResult{
		Iteration: iteration,
	}

	reqBody := ChatCompletionRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: opts.prompt},
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
	startTime := time.Now()

	resp, err := httpClient.Do(req)
	if err != nil {
		return result, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	totalTime := time.Since(startTime)

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
		// Fallback calculation
		result.GenerationTimeMs = result.TotalTimeMs
		if result.CompletionTokens > 0 && result.TotalTimeMs > 0 {
			result.GenerationToksPerSec = float64(result.CompletionTokens) / (result.TotalTimeMs / 1000.0)
		}
	}

	return result, nil
}

func calculateSummary(
	opts *benchmarkOptions, endpoint string, results []BenchmarkResult, startTime time.Time,
) BenchmarkSummary {
	summary := BenchmarkSummary{
		ServiceName:  opts.name,
		Namespace:    opts.namespace,
		Endpoint:     endpoint,
		Iterations:   opts.iterations,
		PromptTokens: 0,
		MaxTokens:    opts.maxTokens,
		Results:      results,
		Timestamp:    startTime,
		Duration:     time.Since(startTime),
	}

	latencies := make([]float64, 0, len(results))
	genToks := make([]float64, 0, len(results))
	promptToks := make([]float64, 0, len(results))

	for _, r := range results {
		if r.Error != "" {
			summary.FailedRuns++
			continue
		}
		summary.SuccessfulRuns++
		summary.PromptTokens = r.PromptTokens // They should all be the same

		latencies = append(latencies, r.TotalTimeMs)
		if r.GenerationToksPerSec > 0 {
			genToks = append(genToks, r.GenerationToksPerSec)
		}
		if r.PromptToksPerSec > 0 {
			promptToks = append(promptToks, r.PromptToksPerSec)
		}
	}

	if len(latencies) == 0 {
		return summary
	}

	sort.Float64s(latencies)
	sort.Float64s(genToks)

	summary.LatencyMin = latencies[0]
	summary.LatencyMax = latencies[len(latencies)-1]
	summary.LatencyMean = mean(latencies)
	summary.LatencyP50 = percentile(latencies, 50)
	summary.LatencyP95 = percentile(latencies, 95)
	summary.LatencyP99 = percentile(latencies, 99)

	if len(genToks) > 0 {
		summary.GenerationToksPerSecMean = mean(genToks)
		summary.GenerationToksPerSecMin = genToks[0]
		summary.GenerationToksPerSecMax = genToks[len(genToks)-1]
	}
	if len(promptToks) > 0 {
		summary.PromptToksPerSecMean = mean(promptToks)
	}

	return summary
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func percentile(sortedValues []float64, p float64) float64 {
	if len(sortedValues) == 0 {
		return 0
	}
	if len(sortedValues) == 1 {
		return sortedValues[0]
	}

	index := (p / 100.0) * float64(len(sortedValues)-1)
	lower := int(index)
	upper := lower + 1
	if upper >= len(sortedValues) {
		return sortedValues[len(sortedValues)-1]
	}

	weight := index - float64(lower)
	return sortedValues[lower]*(1-weight) + sortedValues[upper]*weight
}

func outputTable(summary BenchmarkSummary) {
	fmt.Printf("📈 Benchmark Results\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

	// Success rate
	successRate := float64(summary.SuccessfulRuns) / float64(summary.Iterations) * 100
	fmt.Printf("Runs: %d/%d successful (%.1f%%)\n\n",
		summary.SuccessfulRuns, summary.Iterations, successRate)

	if summary.SuccessfulRuns == 0 {
		fmt.Printf("❌ No successful runs to report.\n")
		return
	}

	// Throughput table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintf(w, "THROUGHPUT\t\n")
	_, _ = fmt.Fprintf(w, "──────────\t\n")
	_, _ = fmt.Fprintf(w, "Generation:\t%.1f tok/s (mean)\t%.1f - %.1f tok/s (range)\n",
		summary.GenerationToksPerSecMean,
		summary.GenerationToksPerSecMin,
		summary.GenerationToksPerSecMax)
	if summary.PromptToksPerSecMean > 0 {
		_, _ = fmt.Fprintf(w, "Prompt:\t%.1f tok/s (mean)\t\n", summary.PromptToksPerSecMean)
	}
	_ = w.Flush()

	fmt.Println()

	// Latency table
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "LATENCY\t\n")
	_, _ = fmt.Fprintf(w, "───────\t\n")
	_, _ = fmt.Fprintf(w, "P50:\t%.0f ms\t\n", summary.LatencyP50)
	_, _ = fmt.Fprintf(w, "P95:\t%.0f ms\t\n", summary.LatencyP95)
	_, _ = fmt.Fprintf(w, "P99:\t%.0f ms\t\n", summary.LatencyP99)
	_, _ = fmt.Fprintf(w, "Min:\t%.0f ms\t\n", summary.LatencyMin)
	_, _ = fmt.Fprintf(w, "Max:\t%.0f ms\t\n", summary.LatencyMax)
	_, _ = fmt.Fprintf(w, "Mean:\t%.0f ms\t\n", summary.LatencyMean)
	_ = w.Flush()

	fmt.Printf("\n═══════════════════════════════════════════════════════════════\n")
	fmt.Printf("Duration: %s\n", summary.Duration.Round(time.Second))
	fmt.Printf("Prompt: %d tokens | Max generation: %d tokens\n",
		summary.PromptTokens, summary.MaxTokens)
}

func outputJSON(summary BenchmarkSummary) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}

func outputMarkdown(summary BenchmarkSummary) {
	fmt.Printf("# LLMKube Benchmark Results\n\n")
	fmt.Printf("**Service:** %s  \n", summary.ServiceName)
	fmt.Printf("**Namespace:** %s  \n", summary.Namespace)
	fmt.Printf("**Date:** %s  \n\n", summary.Timestamp.Format("2006-01-02 15:04:05"))

	successRate := float64(summary.SuccessfulRuns) / float64(summary.Iterations) * 100
	fmt.Printf("## Summary\n\n")
	fmt.Printf("| Metric | Value |\n")
	fmt.Printf("|--------|-------|\n")
	fmt.Printf("| Iterations | %d |\n", summary.Iterations)
	fmt.Printf("| Success Rate | %.1f%% |\n", successRate)
	fmt.Printf("| Duration | %s |\n\n", summary.Duration.Round(time.Second))

	if summary.SuccessfulRuns == 0 {
		fmt.Printf("No successful runs to report.\n")
		return
	}

	fmt.Printf("## Throughput\n\n")
	fmt.Printf("| Metric | Mean | Min | Max |\n")
	fmt.Printf("|--------|------|-----|-----|\n")
	fmt.Printf("| Generation (tok/s) | %.1f | %.1f | %.1f |\n",
		summary.GenerationToksPerSecMean,
		summary.GenerationToksPerSecMin,
		summary.GenerationToksPerSecMax)
	if summary.PromptToksPerSecMean > 0 {
		fmt.Printf("| Prompt (tok/s) | %.1f | - | - |\n", summary.PromptToksPerSecMean)
	}

	fmt.Printf("\n## Latency\n\n")
	fmt.Printf("| Percentile | Value (ms) |\n")
	fmt.Printf("|------------|------------|\n")
	fmt.Printf("| P50 | %.0f |\n", summary.LatencyP50)
	fmt.Printf("| P95 | %.0f |\n", summary.LatencyP95)
	fmt.Printf("| P99 | %.0f |\n", summary.LatencyP99)
	fmt.Printf("| Min | %.0f |\n", summary.LatencyMin)
	fmt.Printf("| Max | %.0f |\n", summary.LatencyMax)
	fmt.Printf("| Mean | %.0f |\n", summary.LatencyMean)

	fmt.Printf("\n---\n")
	fmt.Printf("*Generated by LLMKube v%s*\n", Version)
}

func runCatalogBenchmark(opts *benchmarkOptions) error {
	// Handle context sweep mode separately
	if opts.contextSweep != "" {
		return runContextSweep(opts)
	}

	ctx := context.Background()
	startTime := time.Now()

	modelIDs := strings.Split(opts.catalog, ",")
	for i := range modelIDs {
		modelIDs[i] = strings.TrimSpace(modelIDs[i])
	}

	fmt.Printf("\n🔍 Validating catalog models...\n")
	catalogModels := make([]*Model, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		model, err := GetModel(modelID)
		if err != nil {
			return fmt.Errorf("model '%s' not found in catalog: %w", modelID, err)
		}
		catalogModels = append(catalogModels, model)
		fmt.Printf("   ✅ %s (%s)\n", modelID, model.Size)
	}

	acceleratorDisplay := acceleratorCPU
	if opts.gpu {
		acceleratorDisplay = opts.accelerator
		if acceleratorDisplay == "" {
			acceleratorDisplay = acceleratorCUDA
		}
	}

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

	// Cache preloading if requested
	if opts.preload {
		preloadCatalogModels(modelIDs, opts.namespace)
	}

	// Setup report writer
	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return fmt.Errorf("failed to create report writer: %w", err)
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
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

	// Output comparison results
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

	// Write report if enabled
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

func deployModel(
	ctx context.Context,
	k8sClient client.Client,
	modelID string,
	catalogModel *Model,
	opts *benchmarkOptions,
) error {
	// Clean up any existing resources first to avoid "already exists" errors
	// This is safe because catalog benchmark mode always wants a fresh deployment
	_ = cleanupModel(ctx, k8sClient, modelID, opts)
	// Give the cluster a moment to process the deletion
	time.Sleep(2 * time.Second)

	// Determine accelerator type
	accelerator := opts.accelerator
	if accelerator == "" && opts.gpu {
		accelerator = acceleratorCUDA // default to CUDA if GPU enabled but no accelerator specified
	}

	// Determine GPU layers (use catalog default or override)
	gpuLayers := catalogModel.GPULayers
	if opts.gpuLayers >= 0 {
		gpuLayers = opts.gpuLayers
	}

	// Create Model resource
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

	// Add GPU config if enabled
	if opts.gpu {
		// Determine vendor based on accelerator
		var vendor string
		switch accelerator {
		case acceleratorROCm:
			vendor = "amd"
		case acceleratorMetal:
			vendor = "apple"
		default:
			vendor = "nvidia"
		}

		model.Spec.Hardware = &inferencev1alpha1.HardwareSpec{
			Accelerator: accelerator,
			GPU: &inferencev1alpha1.GPUSpec{
				Enabled: true,
				Count:   opts.gpuCount,
				Vendor:  vendor,
				Layers:  gpuLayers,
				Memory:  catalogModel.Resources.GPUMemory,
			},
		}
	}

	if err := k8sClient.Create(ctx, model); err != nil {
		return fmt.Errorf("failed to create Model: %w", err)
	}

	// Determine image based on accelerator
	image := imageLlamaCppServer
	if opts.gpu {
		switch accelerator {
		case acceleratorCUDA:
			image = imageLlamaCppServerCUDA
		case acceleratorROCm:
			image = imageLlamaCppServerROCm
		case acceleratorMetal:
			image = "" // Metal uses native binary, not container
		default:
			image = imageLlamaCppServerCUDA
		}
	}

	// Create InferenceService resource
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
		// Cleanup model if inference service creation fails
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
			// Check InferenceService status
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
	// Delete InferenceService
	isvc := &inferencev1alpha1.InferenceService{
		ObjectMeta: metav1.ObjectMeta{
			Name:      modelID,
			Namespace: opts.namespace,
		},
	}
	if err := k8sClient.Delete(ctx, isvc); err != nil {
		// Ignore not found errors
		if !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("failed to delete InferenceService: %w", err)
		}
	}

	// Delete Model
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

func outputComparisonTable(report ComparisonReport) error {
	if report.IsStressTest {
		fmt.Printf("📊 Stress Test Comparison Results\n")
	} else {
		fmt.Printf("📊 Benchmark Comparison Results\n")
	}
	fmt.Printf("═══════════════════════════════════════════════════════════════════════════════\n\n")

	// Count successes and failures
	successes := 0
	for _, m := range report.Models {
		if m.Status == statusSuccess {
			successes++
		}
	}
	fmt.Printf("Models: %d/%d benchmarked successfully\n", successes, len(report.Models))

	if report.IsStressTest {
		if report.GPUEnabled {
			if report.TargetDuration > 0 {
				fmt.Printf("Accelerator: %s | GPU Count: %d | Concurrency: %d | Duration: %s | Max Tokens: %d\n\n",
					report.Accelerator, report.GPUCount, report.Concurrency, report.TargetDuration, report.MaxTokens)
			} else {
				fmt.Printf("Accelerator: %s | GPU Count: %d | Concurrency: %d | Iterations: %d | Max Tokens: %d\n\n",
					report.Accelerator, report.GPUCount, report.Concurrency, report.Iterations, report.MaxTokens)
			}
		} else {
			if report.TargetDuration > 0 {
				fmt.Printf("Accelerator: cpu | Concurrency: %d | Duration: %s | Max Tokens: %d\n\n",
					report.Concurrency, report.TargetDuration, report.MaxTokens)
			} else {
				fmt.Printf("Accelerator: cpu | Concurrency: %d | Iterations: %d | Max Tokens: %d\n\n",
					report.Concurrency, report.Iterations, report.MaxTokens)
			}
		}
	} else {
		if report.GPUEnabled {
			fmt.Printf("Accelerator: %s | GPU Count: %d | Iterations: %d | Max Tokens: %d\n\n",
				report.Accelerator, report.GPUCount, report.Iterations, report.MaxTokens)
		} else {
			fmt.Printf("Accelerator: cpu | Iterations: %d | Max Tokens: %d\n\n",
				report.Iterations, report.MaxTokens)
		}
	}

	// Create comparison table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if report.IsStressTest {
		_, _ = fmt.Fprintf(w, "MODEL\tSIZE\tREQUESTS\tREQ/S\tTOK/S\tP50 (ms)\tP99 (ms)\tERROR%%\tSTATUS\n")
		_, _ = fmt.Fprintf(w, "─────\t────\t────────\t─────\t─────\t────────\t────────\t──────\t──────\n")
	} else {
		_, _ = fmt.Fprintf(w, "MODEL\tSIZE\tGEN TOK/S\tP50 (ms)\tP99 (ms)\tVRAM\tSTATUS\n")
		_, _ = fmt.Fprintf(w, "─────\t────\t─────────\t────────\t────────\t────\t──────\n")
	}

	for _, m := range report.Models {
		status := statusIconSuccess
		if m.Status != statusSuccess {
			status = statusIconFailed
		}

		if report.IsStressTest {
			requests := "-"
			rps := "-"
			tps := "-"
			p50 := "-"
			p99 := "-"
			errRate := "-"
			if m.Status == statusSuccess {
				requests = fmt.Sprintf("%d", m.TotalRequests)
				rps = fmt.Sprintf("%.1f", m.RequestsPerSec)
				tps = fmt.Sprintf("%.1f", m.GenerationToksPerSec)
				p50 = fmt.Sprintf("%.0f", m.LatencyP50Ms)
				p99 = fmt.Sprintf("%.0f", m.LatencyP99Ms)
				errRate = fmt.Sprintf("%.1f", m.ErrorRate)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				m.ModelID,
				m.ModelSize,
				requests,
				rps,
				tps,
				p50,
				p99,
				errRate,
				status,
			)
		} else {
			genToks := "-"
			p50 := "-"
			p99 := "-"
			if m.Status == statusSuccess {
				genToks = fmt.Sprintf("%.1f", m.GenerationToksPerSec)
				p50 = fmt.Sprintf("%.0f", m.LatencyP50Ms)
				p99 = fmt.Sprintf("%.0f", m.LatencyP99Ms)
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				m.ModelID,
				m.ModelSize,
				genToks,
				p50,
				p99,
				m.VRAMEstimate,
				status,
			)
		}
	}
	_ = w.Flush()

	// Print any errors
	hasErrors := false
	for _, m := range report.Models {
		if m.Error != "" {
			if !hasErrors {
				fmt.Printf("\n⚠️  Errors:\n")
				hasErrors = true
			}
			fmt.Printf("   %s: %s\n", m.ModelID, m.Error)
		}
	}

	fmt.Printf("\n═══════════════════════════════════════════════════════════════════════════════\n")
	fmt.Printf("Total Duration: %s\n", report.Duration.Round(time.Second))

	return nil
}

func outputComparisonJSON(report ComparisonReport) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func outputComparisonMarkdown(report ComparisonReport) error {
	fmt.Printf("# LLMKube Benchmark Comparison\n\n")
	fmt.Printf("**Date:** %s  \n", report.Timestamp.Format("2006-01-02 15:04:05"))
	fmt.Printf("**GPU Enabled:** %v  \n", report.GPUEnabled)
	fmt.Printf("**Iterations:** %d per model  \n", report.Iterations)
	fmt.Printf("**Max Tokens:** %d  \n\n", report.MaxTokens)

	fmt.Printf("## Results\n\n")
	fmt.Printf("| Model | Size | Gen tok/s | P50 (ms) | P99 (ms) | VRAM | Status |\n")
	fmt.Printf("|-------|------|-----------|----------|----------|------|--------|\n")

	for _, m := range report.Models {
		status := "✅ Success"
		if m.Status != statusSuccess {
			status = "❌ Failed"
		}

		genToks := "-"
		p50 := "-"
		p99 := "-"
		if m.Status == statusSuccess {
			genToks = fmt.Sprintf("%.1f", m.GenerationToksPerSec)
			p50 = fmt.Sprintf("%.0f", m.LatencyP50Ms)
			p99 = fmt.Sprintf("%.0f", m.LatencyP99Ms)
		}

		fmt.Printf("| %s | %s | %s | %s | %s | %s | %s |\n",
			m.ModelID,
			m.ModelSize,
			genToks,
			p50,
			p99,
			m.VRAMEstimate,
			status,
		)
	}

	// Print errors if any
	hasErrors := false
	for _, m := range report.Models {
		if m.Error != "" {
			if !hasErrors {
				fmt.Printf("\n## Errors\n\n")
				hasErrors = true
			}
			fmt.Printf("- **%s**: %s\n", m.ModelID, m.Error)
		}
	}

	fmt.Printf("\n---\n")
	fmt.Printf("*Total Duration: %s*  \n", report.Duration.Round(time.Second))
	fmt.Printf("*Generated by LLMKube v%s*\n", Version)

	return nil
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

func getReportPath(opts *benchmarkOptions) (string, error) {
	if opts.report != "" {
		return opts.report, nil
	}
	if opts.reportDir != "" {
		if err := os.MkdirAll(opts.reportDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create report directory: %w", err)
		}
		filename := fmt.Sprintf("benchmark-%s.md", time.Now().Format("20060102-150405"))
		return filepath.Join(opts.reportDir, filename), nil
	}
	return "", nil
}

func newReportWriter(opts *benchmarkOptions) (*ReportWriter, error) {
	path, err := getReportPath(opts)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}

	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create report file: %w", err)
	}

	rw := &ReportWriter{
		file:      file,
		startTime: time.Now(),
		opts:      opts,
	}

	if err := rw.writeHeader(); err != nil {
		_ = file.Close()
		return nil, err
	}

	fmt.Printf("📄 Report: %s\n", path)
	return rw, nil
}

func (rw *ReportWriter) writeHeader() error {
	_, err := fmt.Fprintf(rw.file, "# LLMKube Benchmark Report\n\n")
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(rw.file, "**Generated:** %s  \n", rw.startTime.Format("2006-01-02 15:04:05"))
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(rw.file, "**Host:** %s (%s/%s)  \n", getHostname(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	if rw.opts.gpu {
		accelerator := rw.opts.accelerator
		if accelerator == "" {
			accelerator = acceleratorCUDA
		}
		_, err = fmt.Fprintf(rw.file, "**Accelerator:** %s (GPU Count: %d)  \n", accelerator, rw.opts.gpuCount)
	} else {
		_, err = fmt.Fprintf(rw.file, "**Accelerator:** CPU  \n")
	}
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(rw.file, "\n---\n\n")
	return err
}

func (rw *ReportWriter) writeSection(title string, content string) error {
	_, err := fmt.Fprintf(rw.file, "## %s\n\n%s\n\n", title, content)
	return err
}

func (rw *ReportWriter) writeBenchmarkResult(summary *BenchmarkSummary) error {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("**Service:** %s  \n", summary.ServiceName))
	buf.WriteString(fmt.Sprintf("**Namespace:** %s  \n", summary.Namespace))
	buf.WriteString(fmt.Sprintf("**Duration:** %s  \n\n", summary.Duration.Round(time.Second)))

	buf.WriteString("| Metric | Value |\n")
	buf.WriteString("|--------|-------|\n")
	buf.WriteString(fmt.Sprintf("| Iterations | %d |\n", summary.Iterations))
	buf.WriteString(fmt.Sprintf("| Success Rate | %.1f%% |\n",
		float64(summary.SuccessfulRuns)/float64(summary.Iterations)*100))
	buf.WriteString(fmt.Sprintf("| Generation (tok/s) | %.1f |\n", summary.GenerationToksPerSecMean))
	buf.WriteString(fmt.Sprintf("| Latency P50 | %.0f ms |\n", summary.LatencyP50))
	buf.WriteString(fmt.Sprintf("| Latency P99 | %.0f ms |\n", summary.LatencyP99))

	return rw.writeSection("Benchmark Results", buf.String())
}

func (rw *ReportWriter) writeStressResult(summary *StressTestSummary) error {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("**Service:** %s  \n", summary.ServiceName))
	buf.WriteString(fmt.Sprintf("**Concurrency:** %d  \n", summary.Concurrency))
	if summary.TargetDuration > 0 {
		buf.WriteString(fmt.Sprintf("**Target Duration:** %s  \n", summary.TargetDuration))
	}
	buf.WriteString(fmt.Sprintf("**Actual Duration:** %s  \n\n", summary.Duration.Round(time.Second)))

	buf.WriteString("| Metric | Value |\n")
	buf.WriteString("|--------|-------|\n")
	buf.WriteString(fmt.Sprintf("| Total Requests | %d |\n", summary.TotalRequests))
	buf.WriteString(fmt.Sprintf("| Requests/sec | %.2f |\n", summary.RequestsPerSec))
	buf.WriteString(fmt.Sprintf("| Error Rate | %.1f%% |\n", summary.ErrorRate))
	buf.WriteString(fmt.Sprintf("| Generation (tok/s) | %.1f |\n", summary.GenerationToksPerSecMean))
	buf.WriteString(fmt.Sprintf("| Peak (tok/s) | %.1f |\n", summary.PeakToksPerSec))
	buf.WriteString(fmt.Sprintf("| Latency P50 | %.0f ms |\n", summary.LatencyP50))
	buf.WriteString(fmt.Sprintf("| Latency P99 | %.0f ms |\n", summary.LatencyP99))

	return rw.writeSection("Stress Test Results", buf.String())
}

func (rw *ReportWriter) writeSweepResults(sweepReport *SweepReport) error {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("**Sweep Type:** %s  \n", sweepReport.SweepType))
	buf.WriteString(fmt.Sprintf("**Values Tested:** %s  \n", strings.Join(sweepReport.Values, ", ")))
	buf.WriteString(fmt.Sprintf("**Duration:** %s  \n\n", sweepReport.Duration.Round(time.Second)))

	// Results table
	buf.WriteString("| Value | Gen tok/s | P50 (ms) | P99 (ms) | Requests | RPS | Error% | Status |\n")
	buf.WriteString("|-------|-----------|----------|----------|----------|-----|--------|--------|\n")

	for _, result := range sweepReport.Results {
		status := statusIconSuccess
		genToks := "-"
		p50 := "-"
		p99 := "-"
		requests := "-"
		rps := "-"
		errRate := "-"

		if result.Error != "" {
			status = statusIconFailed
		} else if result.Stress != nil {
			genToks = fmt.Sprintf("%.1f", result.Stress.GenerationToksPerSecMean)
			p50 = fmt.Sprintf("%.0f", result.Stress.LatencyP50)
			p99 = fmt.Sprintf("%.0f", result.Stress.LatencyP99)
			requests = fmt.Sprintf("%d", result.Stress.TotalRequests)
			rps = fmt.Sprintf("%.1f", result.Stress.RequestsPerSec)
			errRate = fmt.Sprintf("%.1f", result.Stress.ErrorRate)
		} else if result.Summary != nil {
			genToks = fmt.Sprintf("%.1f", result.Summary.GenerationToksPerSecMean)
			p50 = fmt.Sprintf("%.0f", result.Summary.LatencyP50)
			p99 = fmt.Sprintf("%.0f", result.Summary.LatencyP99)
			requests = fmt.Sprintf("%d", result.Summary.Iterations)
		}

		buf.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			result.Value, genToks, p50, p99, requests, rps, errRate, status))
	}

	return rw.writeSection(sweepReport.SweepType+" Sweep Results", buf.String())
}

func (rw *ReportWriter) writeGPUMetrics(metrics []GPUMetric) error {
	if len(metrics) == 0 {
		return nil
	}

	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("**Samples:** %d  \n\n", len(metrics)))

	// Find peak values
	var peakMem, peakUtil, peakTemp, peakPower int
	for _, m := range metrics {
		if m.MemoryUsedMB > peakMem {
			peakMem = m.MemoryUsedMB
		}
		if m.UtilPercent > peakUtil {
			peakUtil = m.UtilPercent
		}
		if m.TempCelsius > peakTemp {
			peakTemp = m.TempCelsius
		}
		if m.PowerWatts > peakPower {
			peakPower = m.PowerWatts
		}
	}

	buf.WriteString("| Metric | Peak Value |\n")
	buf.WriteString("|--------|------------|\n")
	if len(metrics) > 0 && metrics[0].MemoryTotalMB > 0 {
		buf.WriteString(fmt.Sprintf("| Memory | %d / %d MB (%.1f%%) |\n",
			peakMem, metrics[0].MemoryTotalMB, float64(peakMem)/float64(metrics[0].MemoryTotalMB)*100))
	}
	buf.WriteString(fmt.Sprintf("| Utilization | %d%% |\n", peakUtil))
	if peakTemp > 0 {
		buf.WriteString(fmt.Sprintf("| Temperature | %d°C |\n", peakTemp))
	}
	if peakPower > 0 {
		buf.WriteString(fmt.Sprintf("| Power | %d W |\n", peakPower))
	}

	return rw.writeSection("GPU Metrics", buf.String())
}

func (rw *ReportWriter) writeComparisonReport(report ComparisonReport) error {
	var buf strings.Builder

	buf.WriteString(fmt.Sprintf("**Models:** %d  \n", len(report.Models)))
	if report.GPUEnabled {
		buf.WriteString(fmt.Sprintf("**Accelerator:** %s  \n", report.Accelerator))
		buf.WriteString(fmt.Sprintf("**GPU Count:** %d  \n", report.GPUCount))
	}
	if report.IsStressTest {
		buf.WriteString(fmt.Sprintf("**Concurrency:** %d  \n", report.Concurrency))
		if report.TargetDuration > 0 {
			buf.WriteString(fmt.Sprintf("**Duration:** %s per model  \n", report.TargetDuration))
		}
	}
	buf.WriteString(fmt.Sprintf("**Iterations:** %d per model  \n", report.Iterations))
	buf.WriteString(fmt.Sprintf("**Max Tokens:** %d  \n\n", report.MaxTokens))

	if report.IsStressTest {
		buf.WriteString("| Model | Size | Requests | RPS | tok/s | P50 | P99 | Error% | Status |\n")
		buf.WriteString("|-------|------|----------|-----|-------|-----|-----|--------|--------|\n")
	} else {
		buf.WriteString("| Model | Size | Gen tok/s | P50 (ms) | P99 (ms) | VRAM | Status |\n")
		buf.WriteString("|-------|------|-----------|----------|----------|------|--------|\n")
	}

	for _, m := range report.Models {
		status := statusIconSuccess
		if m.Status != statusSuccess {
			status = statusIconFailed
		}

		if report.IsStressTest {
			requests := "-"
			rps := "-"
			tps := "-"
			p50 := "-"
			p99 := "-"
			errRate := "-"
			if m.Status == statusSuccess {
				requests = fmt.Sprintf("%d", m.TotalRequests)
				rps = fmt.Sprintf("%.1f", m.RequestsPerSec)
				tps = fmt.Sprintf("%.1f", m.GenerationToksPerSec)
				p50 = fmt.Sprintf("%.0f", m.LatencyP50Ms)
				p99 = fmt.Sprintf("%.0f", m.LatencyP99Ms)
				errRate = fmt.Sprintf("%.1f", m.ErrorRate)
			}
			buf.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
				m.ModelID, m.ModelSize, requests, rps, tps, p50, p99, errRate, status))
		} else {
			genToks := "-"
			p50 := "-"
			p99 := "-"
			if m.Status == statusSuccess {
				genToks = fmt.Sprintf("%.1f", m.GenerationToksPerSec)
				p50 = fmt.Sprintf("%.0f", m.LatencyP50Ms)
				p99 = fmt.Sprintf("%.0f", m.LatencyP99Ms)
			}
			buf.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s |\n",
				m.ModelID, m.ModelSize, genToks, p50, p99, m.VRAMEstimate, status))
		}
	}

	// Add errors section
	for _, m := range report.Models {
		if m.Error != "" {
			buf.WriteString(fmt.Sprintf("\n**Error (%s):** %s\n", m.ModelID, m.Error))
		}
	}

	return rw.writeSection("Model Comparison", buf.String())
}

func (rw *ReportWriter) close() error {
	duration := time.Since(rw.startTime)
	_, _ = fmt.Fprintf(rw.file, "\n---\n\n")
	_, _ = fmt.Fprintf(rw.file, "*Total Duration: %s*  \n", duration.Round(time.Second))
	_, _ = fmt.Fprintf(rw.file, "*Generated by LLMKube v%s*\n", Version)
	return rw.file.Close()
}

func getHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
}

// GPU Monitoring functions

type gpuMonitor struct {
	metrics  []GPUMetric
	mu       sync.Mutex
	stopChan chan struct{}
	wg       sync.WaitGroup
}

func newGPUMonitor() *gpuMonitor {
	return &gpuMonitor{
		metrics:  make([]GPUMetric, 0),
		stopChan: make(chan struct{}),
	}
}

func (gm *gpuMonitor) start(interval time.Duration) {
	gm.wg.Add(1)
	go func() {
		defer gm.wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// Get initial reading
		if metric := gm.sample(); metric != nil {
			gm.mu.Lock()
			gm.metrics = append(gm.metrics, *metric)
			gm.mu.Unlock()
		}

		for {
			select {
			case <-gm.stopChan:
				return
			case <-ticker.C:
				if metric := gm.sample(); metric != nil {
					gm.mu.Lock()
					gm.metrics = append(gm.metrics, *metric)
					gm.mu.Unlock()
				}
			}
		}
	}()
}

func (gm *gpuMonitor) stop() []GPUMetric {
	close(gm.stopChan)
	gm.wg.Wait()
	gm.mu.Lock()
	defer gm.mu.Unlock()
	return gm.metrics
}

func (gm *gpuMonitor) sample() *GPUMetric {
	// Try nvidia-smi first
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=memory.used,memory.total,utilization.gpu,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err == nil {
		return parseNvidiaSMI(string(output))
	}
	return nil
}

func parseNvidiaSMI(output string) *GPUMetric {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return nil
	}

	// Parse first GPU (sum if multiple)
	var totalMemUsed, totalMemTotal, maxUtil, maxTemp, totalPower int
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) < 3 {
			continue
		}

		memUsed, _ := strconv.Atoi(strings.TrimSpace(fields[0]))
		memTotal, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
		util, _ := strconv.Atoi(strings.TrimSpace(fields[2]))

		totalMemUsed += memUsed
		totalMemTotal += memTotal
		if util > maxUtil {
			maxUtil = util
		}

		if len(fields) >= 4 {
			temp, _ := strconv.Atoi(strings.TrimSpace(fields[3]))
			if temp > maxTemp {
				maxTemp = temp
			}
		}
		if len(fields) >= 5 {
			power, _ := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)
			totalPower += int(power)
		}
	}

	return &GPUMetric{
		Timestamp:     time.Now(),
		MemoryUsedMB:  totalMemUsed,
		MemoryTotalMB: totalMemTotal,
		UtilPercent:   maxUtil,
		TempCelsius:   maxTemp,
		PowerWatts:    totalPower,
	}
}

// Sweep functions

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

func runConcurrencySweep(opts *benchmarkOptions) error {
	ctx := context.Background()
	startTime := time.Now()

	values, err := parseSweepValues(opts.concurrencySweep)
	if err != nil {
		return fmt.Errorf("invalid concurrency-sweep values: %w", err)
	}

	// Get endpoint once
	endpoint, cleanup, err := getEndpoint(ctx, opts)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Setup report writer
	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return err
	}

	// Setup GPU monitoring if enabled
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

		result := SweepResult{
			Parameter: "concurrency",
			Value:     strconv.Itoa(concurrency),
		}

		iterStartTime := time.Now()
		if concurrency > 1 || opts.duration > 0 {
			summary, err := runStressTestInternal(ctx, endpoint, &testOpts, iterStartTime)
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Stress = summary
			}
		} else {
			summary, err := runBenchmarkInternalWithEndpoint(ctx, endpoint, &testOpts, iterStartTime)
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Summary = summary
			}
		}

		sweepReport.Results = append(sweepReport.Results, result)
		fmt.Println()
	}

	// Stop GPU monitoring and collect metrics
	if gpuMon != nil {
		sweepReport.GPUMetrics = gpuMon.stop()
	}

	sweepReport.Duration = time.Since(startTime)

	// Output results
	outputSweepTable(sweepReport)

	// Write report if enabled
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

		result := SweepResult{
			Parameter: "max_tokens",
			Value:     strconv.Itoa(maxTokens),
		}

		iterStartTime := time.Now()
		if opts.concurrent > 1 || opts.duration > 0 {
			summary, err := runStressTestInternal(ctx, endpoint, &testOpts, iterStartTime)
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Stress = summary
			}
		} else {
			summary, err := runBenchmarkInternalWithEndpoint(ctx, endpoint, &testOpts, iterStartTime)
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Summary = summary
			}
		}

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

func outputSweepTable(report SweepReport) {
	fmt.Printf("\n📊 %s Sweep Results\n", report.SweepType)
	fmt.Printf("═══════════════════════════════════════════════════════════════════════════════\n\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "VALUE\tGEN TOK/S\tP50 (ms)\tP99 (ms)\tREQUESTS\tRPS\tERROR%%\tSTATUS\n")
	_, _ = fmt.Fprintf(w, "─────\t─────────\t────────\t────────\t────────\t───\t──────\t──────\n")

	for _, r := range report.Results {
		status := statusIconSuccess
		genToks := "-"
		p50 := "-"
		p99 := "-"
		requests := "-"
		rps := "-"
		errRate := "-"

		if r.Error != "" {
			status = statusIconFailed
		} else if r.Stress != nil {
			genToks = fmt.Sprintf("%.1f", r.Stress.GenerationToksPerSecMean)
			p50 = fmt.Sprintf("%.0f", r.Stress.LatencyP50)
			p99 = fmt.Sprintf("%.0f", r.Stress.LatencyP99)
			requests = fmt.Sprintf("%d", r.Stress.TotalRequests)
			rps = fmt.Sprintf("%.1f", r.Stress.RequestsPerSec)
			errRate = fmt.Sprintf("%.1f", r.Stress.ErrorRate)
		} else if r.Summary != nil {
			genToks = fmt.Sprintf("%.1f", r.Summary.GenerationToksPerSecMean)
			p50 = fmt.Sprintf("%.0f", r.Summary.LatencyP50)
			p99 = fmt.Sprintf("%.0f", r.Summary.LatencyP99)
			requests = fmt.Sprintf("%d", r.Summary.Iterations)
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Value, genToks, p50, p99, requests, rps, errRate, status)
	}
	_ = w.Flush()

	fmt.Printf("\n═══════════════════════════════════════════════════════════════════════════════\n")
	fmt.Printf("Total Duration: %s\n", report.Duration.Round(time.Second))
}

// Context sweep requires catalog mode (redeploys with different context sizes)
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

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
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

		testOpts := *opts
		testOpts.contextSize = int32(contextSize)
		testOpts.name = modelID

		result := SweepResult{
			Parameter: "context_size",
			Value:     strconv.Itoa(contextSize),
		}

		// Deploy with this context size
		fmt.Printf("🚀 Deploying with context size %d...\n", contextSize)
		if err := deployModel(ctx, k8sClient, modelID, catalogModel, &testOpts); err != nil {
			result.Error = fmt.Sprintf("deploy failed: %v", err)
			sweepReport.Results = append(sweepReport.Results, result)
			fmt.Printf("   ❌ %s\n\n", result.Error)
			continue
		}

		fmt.Printf("⏳ Waiting for deployment...\n")
		if err := waitForDeployment(ctx, k8sClient, modelID, &testOpts); err != nil {
			result.Error = fmt.Sprintf("deployment timeout: %v", err)
			if opts.cleanup {
				_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
			}
			sweepReport.Results = append(sweepReport.Results, result)
			fmt.Printf("   ❌ %s\n\n", result.Error)
			continue
		}
		fmt.Printf("   ✅ Ready\n\n")

		// Get endpoint
		endpoint, endpointCleanup, err := getEndpoint(ctx, &testOpts)
		if err != nil {
			result.Error = fmt.Sprintf("endpoint error: %v", err)
			if opts.cleanup {
				_ = cleanupModel(ctx, k8sClient, modelID, &testOpts)
			}
			sweepReport.Results = append(sweepReport.Results, result)
			continue
		}

		// Run benchmark
		iterStartTime := time.Now()
		if opts.concurrent > 1 || opts.duration > 0 {
			summary, err := runStressTestInternal(ctx, endpoint, &testOpts, iterStartTime)
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Stress = summary
			}
		} else {
			summary, err := runBenchmarkInternalWithEndpoint(ctx, endpoint, &testOpts, iterStartTime)
			if err != nil {
				result.Error = err.Error()
			} else {
				result.Summary = summary
			}
		}

		if endpointCleanup != nil {
			endpointCleanup()
		}

		// Cleanup
		if opts.cleanup {
			fmt.Printf("🧹 Cleaning up...\n")
			if err := cleanupModel(ctx, k8sClient, modelID, &testOpts); err != nil {
				fmt.Printf("   ⚠️  %v\n", err)
			}
		}

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

// Cache preloading

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

// Suite runner

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

	// Validate catalog models
	catalogModels := make([]*Model, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		model, err := GetModel(modelID)
		if err != nil {
			return fmt.Errorf("model '%s' not found in catalog: %w", modelID, err)
		}
		catalogModels = append(catalogModels, model)
	}

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

	// Setup report writer
	reportWriter, err := newReportWriter(opts)
	if err != nil {
		return fmt.Errorf("failed to create report writer: %w", err)
	}

	// Write suite header to report
	if reportWriter != nil {
		var header strings.Builder
		header.WriteString(fmt.Sprintf("**Suite:** %s  \n", suite.Name))
		header.WriteString(fmt.Sprintf("**Description:** %s  \n", suite.Description))
		header.WriteString(fmt.Sprintf("**Models:** %s  \n", strings.Join(modelIDs, ", ")))
		header.WriteString(fmt.Sprintf("**Phases:** %d  \n", len(suite.Phases)))
		_ = reportWriter.writeSection("Test Suite Configuration", header.String())
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	if err := inferencev1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return fmt.Errorf("failed to add scheme: %w", err)
	}

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return fmt.Errorf("failed to create client: %w", err)
	}

	// Run each phase
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
	// Handle context sweep phase
	if len(phase.ContextSizes) > 0 {
		return runSuiteContextSweep(ctx, k8sClient, phase, modelIDs, catalogModels, opts, reportWriter)
	}

	// Handle tokens sweep phase
	if len(phase.MaxTokens) > 0 {
		return runSuiteTokensSweep(ctx, k8sClient, phase, modelIDs, catalogModels, opts, reportWriter)
	}

	// Handle GPU count variations (scaling suite)
	if len(phase.GPUCounts) > 0 {
		return runSuiteGPUScaling(ctx, k8sClient, phase, modelIDs, catalogModels, opts, reportWriter)
	}

	// Handle concurrency sweep or stability test
	return runSuiteConcurrencyPhase(ctx, k8sClient, phase, modelIDs, catalogModels, opts, reportWriter)
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
		catalogModel := catalogModels[idx]

		fmt.Printf("🚀 Deploying %s...\n", modelID)
		if err := deployModel(ctx, k8sClient, modelID, catalogModel, opts); err != nil {
			return fmt.Errorf("deploy failed: %w", err)
		}

		fmt.Printf("⏳ Waiting for deployment...\n")
		if err := waitForDeployment(ctx, k8sClient, modelID, opts); err != nil {
			if opts.cleanup {
				_ = cleanupModel(ctx, k8sClient, modelID, opts)
			}
			return fmt.Errorf("deployment timeout: %w", err)
		}
		fmt.Printf("   ✅ Ready\n\n")

		testOpts := *opts
		testOpts.name = modelID

		endpoint, endpointCleanup, err := getEndpoint(ctx, &testOpts)
		if err != nil {
			if opts.cleanup {
				_ = cleanupModel(ctx, k8sClient, modelID, opts)
			}
			return fmt.Errorf("endpoint error: %w", err)
		}

		// Run concurrency sweep or single stress test
		if len(phase.Concurrency) > 1 {
			sweepReport := SweepReport{
				SweepType:  "Concurrency",
				Values:     make([]string, len(phase.Concurrency)),
				Results:    make([]SweepResult, 0, len(phase.Concurrency)),
				Timestamp:  time.Now(),
				GPUEnabled: opts.gpu,
			}
			for i, c := range phase.Concurrency {
				sweepReport.Values[i] = strconv.Itoa(c)
			}

			for _, concurrency := range phase.Concurrency {
				fmt.Printf("📊 Testing concurrency: %d (duration: %s)\n", concurrency, phase.Duration)

				runOpts := testOpts
				runOpts.concurrent = concurrency
				runOpts.duration = phase.Duration

				result := SweepResult{
					Parameter: "concurrency",
					Value:     strconv.Itoa(concurrency),
				}

				iterStart := time.Now()
				summary, err := runStressTestInternal(ctx, endpoint, &runOpts, iterStart)
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
		} else {
			// Single concurrency level (stability test)
			concurrency := 4
			if len(phase.Concurrency) == 1 {
				concurrency = phase.Concurrency[0]
			}

			fmt.Printf("📊 Running stability test: %d concurrent, %s duration\n", concurrency, phase.Duration)

			runOpts := testOpts
			runOpts.concurrent = concurrency
			runOpts.duration = phase.Duration

			summary, err := runStressTestInternal(ctx, endpoint, &runOpts, time.Now())
			if err != nil {
				return fmt.Errorf("stability test failed: %w", err)
			}

			outputStressTable(*summary)

			if reportWriter != nil {
				_ = reportWriter.writeStressResult(summary)
			}
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
