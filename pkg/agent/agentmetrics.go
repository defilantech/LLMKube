/*
Copyright 2026.

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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/defilantech/llmkube/pkg/agent/ingress"
)

// AgentRegistry is a standalone Prometheus registry for the Metal agent.
// It is separate from controller-runtime's registry because the agent
// runs as its own binary without the controller-manager.
var AgentRegistry = prometheus.NewRegistry()

var (
	managedProcesses = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_managed_processes",
			Help: "Number of llama-server processes currently managed by the agent.",
		},
	)

	processHealthy = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_process_healthy",
			Help: "Whether a managed process is healthy (1) or unhealthy (0).",
		},
		[]string{"name", "namespace"},
	)

	processRestarts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_process_restarts_total",
			Help: "Total number of engine restart attempts by the agent: health restarts " +
				"and restarts after a memory-pressure eviction.",
		},
		[]string{"name", "namespace"},
	)

	healthCheckDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmkube_metal_agent_health_check_duration_seconds",
			Help:    "Duration of individual health check probes.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"name", "namespace"},
	)

	memoryBudgetBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_memory_budget_bytes",
			Help: "Host-wide memory budget available for model serving in bytes (total memory times " +
				"--memory-fraction). Under a cgroup memory limit on Linux, engines the agent starts are " +
				"admitted against the cgroup's budget instead, which this gauge does not report.",
		},
	)

	memoryCommittedBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_memory_committed_bytes",
			Help: "Memory admitted to running and starting processes in bytes; " +
				"new models are admitted against the budget minus this.",
		},
	)

	memoryEstimatedBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_memory_estimated_bytes",
			Help: "Estimated memory usage per managed process in bytes.",
		},
		[]string{"name", "namespace"},
	)

	systemMemoryAvailableBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_system_memory_available_bytes",
			Help: "Available system memory in bytes.",
		},
	)

	systemMemoryWiredBytes = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_system_memory_wired_bytes",
			Help: "Wired (non-pageable) system memory in bytes.",
		},
	)

	processRSSBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_process_rss_bytes",
			Help: "Actual resident set size per managed process in bytes.",
		},
		[]string{"name"},
	)

	memoryPressureLevelGauge = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_memory_pressure_level",
			Help: "Current memory pressure level: 0=normal, 1=warning, 2=critical.",
		},
	)

	evictionsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_evictions_total",
			Help: "Total number of process eviction events triggered by memory pressure.",
		},
	)

	engineRecordErrorsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_engine_record_errors_total",
			Help: "Engine record writes that failed and failed the start.",
		},
	)

	engineAdoptionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_engines_adopted_total",
			Help: "Engine records evaluated at startup, by outcome (adopted, stopped, discarded).",
		},
		[]string{"result"},
	)

	evictionsSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_evictions_skipped_total",
			Help: "Total number of times the watchdog wanted to evict but did not. " +
				"Reasons: 'disabled' (eviction CLI flag off), 'below_guard' (LLMKube " +
				"not responsible for >50% of system RSS), 'floor' (would have left " +
				"zero managed processes), 'all_protected' (every candidate has " +
				"EvictionProtection=true), 'empty' (no managed processes), " +
				"'runtime_ineligible' (every managed process is on a shared daemon, " +
				"such as oMLX or Ollama, that the agent does not own).",
		},
		[]string{"reason"},
	)

	watchConsecutiveFailures = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_watch_consecutive_failures",
			Help: "Current count of consecutive Kubernetes list failures from the " +
				"InferenceService watcher. Resets to 0 on a successful poll. " +
				"At the configured threshold the API counts as unreachable " +
				"(llmkube_metal_agent_api_unreachable); the agent keeps running.",
		},
	)

	watchAPIUnreachable = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_api_unreachable",
			Help: "1 while the agent counts the Kubernetes API as unreachable: the " +
				"InferenceService watcher's last --max-watch-failures lists or more " +
				"all failed. Running engines keep serving with their last-known state " +
				"and relay tokens, and the watcher retries with backoff. 0 otherwise.",
		},
	)

	watchLocalNetworkBlocked = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_local_network_blocked",
			Help: "1 while, on macOS, the Kubernetes API counts as unreachable and the " +
				"latest failed InferenceService list failed with no route to host or " +
				"network unreachable to a private or link-local address: how macOS refuses " +
				"an agent without Local Network access, which it loses on every agent " +
				"upgrade. 0 otherwise, and always 0 on other systems.",
		},
	)

	watchModelLookupFailingServices = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_watch_model_lookup_failing_services",
			Help: "Number of InferenceServices whose referenced Model lookup is " +
				"currently failing with an error other than NotFound. The agent " +
				"keeps each such service in its previous state until a lookup succeeds.",
		},
	)

	fatalExitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_fatal_exits_total",
			Help: "Total number of fatal-exit signals raised by agent subsystems " +
				"(health-server). The agent process terminates after raising one; " +
				"this counter is therefore typically 0 or 1 over a process lifetime, but " +
				"useful when scraped by a metric pipeline that survives the restart.",
		},
		[]string{"subsystem"},
	)

	// Apple Silicon power gauges. Populated by the powermetrics sampler when
	// the agent is run with --apple-power-enabled. Zero otherwise. Designed
	// to be scraped by InferCost's Metal collector for $/MTok cost
	// attribution on macOS, where DCGM (NVIDIA-only) doesn't exist.
	// See infercost issue #46 for the cross-repo design.
	applePowerCombinedWatts = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_apple_power_combined_watts",
			Help: "Combined CPU + GPU + ANE package power in watts from macOS powermetrics. " +
				"Zero unless the agent is run with --apple-power-enabled and a " +
				"NOPASSWD sudoers entry for /usr/bin/powermetrics is installed.",
		},
	)
	applePowerGPUWatts = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_apple_power_gpu_watts",
			Help: "GPU subsystem power in watts from macOS powermetrics. " +
				"Zero unless --apple-power-enabled.",
		},
	)
	applePowerCPUWatts = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_apple_power_cpu_watts",
			Help: "CPU subsystem power in watts from macOS powermetrics. " +
				"Zero unless --apple-power-enabled.",
		},
	)
	applePowerANEWatts = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_apple_power_ane_watts",
			Help: "Apple Neural Engine power in watts from macOS powermetrics. " +
				"Zero unless --apple-power-enabled. ANE is typically idle for " +
				"llama.cpp / Metal LLM inference (which uses GPU compute, not ANE).",
		},
	)
)

// clientProxyRequests counts host-side client-proxy requests by outcome
// (no_backend, 2xx, 3xx, 4xx, 5xx, other). See ClientProxy / #406.
var clientProxyRequests = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "llmkube_metal_agent_client_proxy_requests_total",
		Help: "Total host-side client-proxy requests, labeled by outcome.",
	},
	[]string{"outcome"},
)

// AgentCollectors are the llmkube_* collectors the agent records; init
// registers them in AgentRegistry. They are exported, as internal/metrics'
// lists are, so cmd/gen-reference-docs can document every family, including a
// vector that has no series yet and so is invisible to AgentRegistry.Gather.
var AgentCollectors = []prometheus.Collector{
	clientProxyRequests,
	ingress.RequestsTotal,
	managedProcesses,
	processHealthy,
	processRestarts,
	healthCheckDuration,
	memoryBudgetBytes,
	memoryCommittedBytes,
	memoryEstimatedBytes,
	systemMemoryAvailableBytes,
	systemMemoryWiredBytes,
	processRSSBytes,
	memoryPressureLevelGauge,
	evictionsTotal,
	engineRecordErrorsTotal,
	engineAdoptionsTotal,
	evictionsSkippedTotal,
	watchConsecutiveFailures,
	watchAPIUnreachable,
	watchLocalNetworkBlocked,
	watchModelLookupFailingServices,
	fatalExitsTotal,
	applePowerCombinedWatts,
	applePowerGPUWatts,
	applePowerCPUWatts,
	applePowerANEWatts,
}

func init() {
	AgentRegistry.MustRegister(AgentCollectors...)
	AgentRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
}
