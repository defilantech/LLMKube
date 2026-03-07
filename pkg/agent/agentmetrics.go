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
	)
}
