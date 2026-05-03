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

package agent

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
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
			Help: "Total number of process restarts triggered by health monitoring.",
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
			Help: "Total memory budget available for model serving in bytes.",
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

	evictionsSkippedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_evictions_skipped_total",
			Help: "Total number of times the watchdog wanted to evict but did not. " +
				"Reasons: 'disabled' (eviction CLI flag off), 'below_guard' (LLMKube " +
				"not responsible for >50% of system RSS), 'floor' (would have left " +
				"zero managed processes), 'all_protected' (every candidate has " +
				"EvictionProtection=true), 'empty' (no managed processes).",
		},
		[]string{"reason"},
	)

	watchConsecutiveFailures = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_metal_agent_watch_consecutive_failures",
			Help: "Current count of consecutive Kubernetes list failures from the " +
				"InferenceService watcher. Resets to 0 on a successful poll. " +
				"Triggers a fatal exit when the configured threshold is reached.",
		},
	)

	fatalExitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_metal_agent_fatal_exits_total",
			Help: "Total number of fatal-exit signals raised by agent subsystems " +
				"(watcher, health-server). The agent process terminates after raising one; " +
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

func init() {
	AgentRegistry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		managedProcesses,
		processHealthy,
		processRestarts,
		healthCheckDuration,
		memoryBudgetBytes,
		memoryEstimatedBytes,
		systemMemoryAvailableBytes,
		systemMemoryWiredBytes,
		processRSSBytes,
		memoryPressureLevelGauge,
		evictionsTotal,
		evictionsSkippedTotal,
		watchConsecutiveFailures,
		fatalExitsTotal,
		applePowerCombinedWatts,
		applePowerGPUWatts,
		applePowerCPUWatts,
		applePowerANEWatts,
	)
}
