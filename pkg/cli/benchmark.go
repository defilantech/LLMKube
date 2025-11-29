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
	"sort"
	"strings"
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

	catalog     string
	gpu         bool
	gpuCount    int32
	gpuLayers   int32
	accelerator string
	cleanup     bool
	deployWait  time.Duration
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
	Models      []ModelBenchmark `json:"models"`
	Timestamp   time.Time        `json:"timestamp"`
	Duration    time.Duration    `json:"duration"`
	Iterations  int              `json:"iterations"`
	MaxTokens   int              `json:"max_tokens"`
	GPUEnabled  bool             `json:"gpu_enabled"`
	GPUCount    int32            `json:"gpu_count,omitempty"`
	Accelerator string           `json:"accelerator,omitempty"`
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
	imageLlamaCppServer     = "ghcr.io/ggerganov/llama.cpp:server"
	imageLlamaCppServerCUDA = "ghcr.io/ggerganov/llama.cpp:server-cuda"
	imageLlamaCppServerROCm = "ghcr.io/ggerganov/llama.cpp:server-rocm"
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

CATALOG MODE (--catalog):
  Automatically deploy, benchmark, and compare multiple models from the catalog.
  Models are deployed sequentially, benchmarked, and optionally cleaned up.

Examples:
  # Benchmark a running service (requires port-forward or external access)
  llmkube benchmark my-llm -n default

  # Benchmark with custom settings
  llmkube benchmark my-llm --iterations 20 --max-tokens 100

  # Benchmark with specific endpoint (if externally exposed)
  llmkube benchmark my-llm --endpoint http://my-llm.example.com:8080

  # Output results as JSON
  llmkube benchmark my-llm --output json

  # CATALOG MODE: Benchmark multiple catalog models
  llmkube benchmark --catalog llama-3.2-3b,mistral-7b,llama-3.1-8b --gpu

  # Catalog mode with custom iterations and no cleanup
  llmkube benchmark --catalog llama-3.2-3b,phi-3-mini --gpu --iterations 5 --no-cleanup

  # Catalog mode with JSON output for CI/CD
  llmkube benchmark --catalog llama-3.2-3b,mistral-7b --gpu --output json
`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.catalog != "" {
				return runCatalogBenchmark(opts)
			}

			if len(args) == 0 {
				return fmt.Errorf("SERVICE_NAME is required (or use --catalog for multi-model comparison)")
			}
			opts.name = args[0]
			return runBenchmark(opts)
		},
	}

	// Flags
	cmd.Flags().StringVarP(&opts.namespace, "namespace", "n", "default", "Kubernetes namespace")
	cmd.Flags().IntVarP(&opts.iterations, "iterations", "i", 10, "Number of benchmark iterations")
	cmd.Flags().IntVar(&opts.warmup, "warmup", 2, "Number of warmup requests (not counted)")
	cmd.Flags().StringVarP(&opts.prompt, "prompt", "p", defaultBenchmarkPrompt, "Prompt to use for benchmarking")
	cmd.Flags().IntVar(&opts.maxTokens, "max-tokens", 50, "Maximum tokens to generate per request")
	cmd.Flags().IntVarP(&opts.concurrent, "concurrent", "c", 1, "Number of concurrent requests (not yet implemented)")
	cmd.Flags().StringVarP(&opts.output, "output", "o", "table", "Output format: table, json, markdown")
	cmd.Flags().StringVar(&opts.endpoint, "endpoint", "", "Override endpoint URL (default: auto-detect from service)")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 60*time.Second, "Request timeout")
	cmd.Flags().BoolVar(&opts.portForward, "port-forward", true, "Automatically set up port forwarding")

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
	case "json":
		return outputJSON(summary)
	case "markdown":
		outputMarkdown(summary)
		return nil
	default:
		outputTable(summary)
		return nil
	}
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
	fmt.Printf("Iterations:  %d per model (+ %d warmup)\n", opts.iterations, opts.warmup)
	fmt.Printf("Cleanup:     %v\n", opts.cleanup)
	fmt.Printf("═══════════════════════════════════════════════════════════════\n\n")

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

	report := ComparisonReport{
		Models:      make([]ModelBenchmark, 0, len(modelIDs)),
		Timestamp:   startTime,
		Iterations:  opts.iterations,
		MaxTokens:   opts.maxTokens,
		GPUEnabled:  opts.gpu,
		GPUCount:    opts.gpuCount,
		Accelerator: acceleratorDisplay,
	}

	for idx, modelID := range modelIDs {
		catalogModel := catalogModels[idx]

		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n")
		fmt.Printf("📦 [%d/%d] Benchmarking: %s (%s)\n", idx+1, len(modelIDs), catalogModel.Name, catalogModel.Size)
		fmt.Printf("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n")

		modelBenchmark := ModelBenchmark{
			ModelID:      modelID,
			ModelName:    catalogModel.Name,
			ModelSize:    catalogModel.Size,
			VRAMEstimate: catalogModel.VRAMEstimate,
		}

		// Deploy the model
		fmt.Printf("🚀 Deploying %s...\n", modelID)
		err := deployModel(ctx, k8sClient, modelID, catalogModel, opts)
		if err != nil {
			fmt.Printf("   ❌ Deployment failed: %v\n\n", err)
			modelBenchmark.Status = statusFailed
			modelBenchmark.Error = fmt.Sprintf("deployment failed: %v", err)
			report.Models = append(report.Models, modelBenchmark)
			continue
		}

		// Wait for deployment to be ready
		fmt.Printf("⏳ Waiting for deployment to be ready...\n")
		err = waitForDeployment(ctx, k8sClient, modelID, opts)
		if err != nil {
			fmt.Printf("   ❌ Deployment not ready: %v\n", err)
			modelBenchmark.Status = statusFailed
			modelBenchmark.Error = fmt.Sprintf("deployment timeout: %v", err)
			if opts.cleanup {
				_ = cleanupModel(ctx, k8sClient, modelID, opts)
			}
			report.Models = append(report.Models, modelBenchmark)
			continue
		}
		fmt.Printf("   ✅ Deployment ready\n\n")

		// Run benchmark
		opts.name = modelID
		summary, err := runBenchmarkInternal(ctx, opts)
		if err != nil {
			fmt.Printf("   ❌ Benchmark failed: %v\n\n", err)
			modelBenchmark.Status = statusFailed
			modelBenchmark.Error = fmt.Sprintf("benchmark failed: %v", err)
		} else {
			modelBenchmark.Status = statusSuccess
			modelBenchmark.GenerationToksPerSec = summary.GenerationToksPerSecMean
			modelBenchmark.PromptToksPerSec = summary.PromptToksPerSecMean
			modelBenchmark.LatencyP50Ms = summary.LatencyP50
			modelBenchmark.LatencyP99Ms = summary.LatencyP99
		}

		report.Models = append(report.Models, modelBenchmark)

		// Cleanup if requested
		if opts.cleanup {
			fmt.Printf("🧹 Cleaning up %s...\n", modelID)
			if err := cleanupModel(ctx, k8sClient, modelID, opts); err != nil {
				fmt.Printf("   ⚠️  Cleanup warning: %v\n", err)
			} else {
				fmt.Printf("   ✅ Cleaned up\n")
			}
		}
		fmt.Println()
	}

	report.Duration = time.Since(startTime)

	// Output comparison results
	fmt.Printf("\n")
	switch opts.output {
	case "json":
		return outputComparisonJSON(report)
	case "markdown":
		return outputComparisonMarkdown(report)
	default:
		return outputComparisonTable(report)
	}
}

func deployModel(
	ctx context.Context,
	k8sClient client.Client,
	modelID string,
	catalogModel *Model,
	opts *benchmarkOptions,
) error {
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

func runBenchmarkInternal(ctx context.Context, opts *benchmarkOptions) (*BenchmarkSummary, error) {
	// Get endpoint URL
	endpoint, cleanup, err := getEndpoint(ctx, opts)
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	startTime := time.Now()

	// Run warmup requests
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

	// Run benchmark iterations
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

	// Calculate summary statistics
	summary := calculateSummary(opts, endpoint, results, startTime)
	return &summary, nil
}

func outputComparisonTable(report ComparisonReport) error {
	fmt.Printf("📊 Benchmark Comparison Results\n")
	fmt.Printf("═══════════════════════════════════════════════════════════════════════════════\n\n")

	// Count successes and failures
	successes := 0
	for _, m := range report.Models {
		if m.Status == statusSuccess {
			successes++
		}
	}
	fmt.Printf("Models: %d/%d benchmarked successfully\n", successes, len(report.Models))
	if report.GPUEnabled {
		fmt.Printf("Accelerator: %s | GPU Count: %d | Iterations: %d | Max Tokens: %d\n\n",
			report.Accelerator, report.GPUCount, report.Iterations, report.MaxTokens)
	} else {
		fmt.Printf("Accelerator: cpu | Iterations: %d | Max Tokens: %d\n\n",
			report.Iterations, report.MaxTokens)
	}

	// Create comparison table
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "MODEL\tSIZE\tGEN TOK/S\tP50 (ms)\tP99 (ms)\tVRAM\tSTATUS\n")
	_, _ = fmt.Fprintf(w, "─────\t────\t─────────\t────────\t────────\t────\t──────\n")

	for _, m := range report.Models {
		status := "✅"
		if m.Status != statusSuccess {
			status = "❌"
		}

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
