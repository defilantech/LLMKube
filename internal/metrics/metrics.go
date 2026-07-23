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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// Model metrics

	ModelDownloadDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmkube_model_download_duration_seconds",
			Help:    "Duration of model download/copy operations.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1s to ~4096s
		},
		[]string{"model", "namespace", "source_type"},
	)

	ModelStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_model_status",
			Help: "Current phase of models. Value is always 1; use the phase label for filtering.",
		},
		[]string{"model", "namespace", "phase"},
	)

	// InferenceService metrics

	InferenceServiceReadyDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmkube_inferenceservice_ready_duration_seconds",
			Help:    "Time from InferenceService creation to Ready phase.",
			Buckets: prometheus.ExponentialBuckets(5, 2, 10), // 5s to ~2560s
		},
		[]string{"inferenceservice", "namespace"},
	)

	InferenceServicePhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_inferenceservice_phase",
			Help: "Current phase of inference services. Value is always 1; use the phase label for filtering.",
		},
		[]string{"inferenceservice", "namespace", "phase"},
	)

	InferenceServiceInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_inferenceservice_info",
			Help: "Information about inference services. Value is always 1; use accelerator and runtime labels for grouping.",
		},
		[]string{"inferenceservice", "namespace", "accelerator", "runtime"},
	)

	InferenceServiceReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_inferenceservice_replicas",
			Help: "InferenceService replica counts. Use the state label: ready, desired.",
		},
		[]string{"inferenceservice", "namespace", "state"},
	)

	GPUQueueDepth = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_gpu_queue_depth",
			Help: "Number of InferenceServices waiting for GPU resources.",
		},
	)

	// Reconcile metrics

	ReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_reconcile_total",
			Help: "Total number of reconciliation cycles.",
		},
		[]string{"controller", "result"},
	)

	ReconcileDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmkube_reconcile_duration_seconds",
			Help:    "Duration of reconciliation cycles.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller"},
	)

	// Router-proxy metrics

	RouterRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_router_requests_total",
			Help: "Total number of router-proxy requests.",
		},
		[]string{"router", "rule", "backend", "classification", "outcome"},
	)

	RouterRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmkube_router_request_duration_seconds",
			Help:    "Duration of router-proxy requests.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"router", "rule", "backend"},
	)

	RouterFailClosedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_router_fail_closed_total",
			Help: "Total number of requests rejected by the fail-closed gate.",
		},
		[]string{"router", "rule", "classification"},
	)

	RouterActiveBackends = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_router_active_backends",
			Help: "Number of active backends per tier.",
		},
		[]string{"router", "tier"},
	)

	RouterBackendHealth = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_router_backend_health",
			Help: "Health status of a backend (1=healthy, 0=unhealthy).",
		},
		[]string{"router", "backend"},
	)

	// RouterFirstTokenSeconds captures time-to-first-byte (TTFT) for
	// streaming responses. Operators use it to size user-facing
	// timeouts and to compare local vs. cloud TTFT for the same model.
	RouterFirstTokenSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmkube_router_first_token_seconds",
			Help:    "Time from inbound request to first upstream response byte (streaming TTFT).",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 12), // 10ms to ~20s
		},
		[]string{"router", "backend"},
	)

	// RouterBudgetUtilization reports how much of a rule's dispatch
	// timeout a request consumed (0.0..1.0). Values near 1.0 flag
	// requests that are budget-bound and likely to fail if the
	// upstream slows further; values near 0.0 flag under-provisioned
	// budgets that could be tightened.
	RouterBudgetUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_router_budget_utilization",
			Help: "Fraction of the resolved dispatch timeout consumed by a request (0.0 to 1.0).",
		},
		[]string{"router", "scope"},
	)

	// GPUQuota metrics (#416): per-quota GPU usage vs. cap, and admission
	// denials. These enable the multi-tenancy dashboard and satisfy the
	// "record the denial" requirement (a validating webhook, being
	// sideEffects=None, cannot mutate the GPUQuota status counter).
	GPUQuotaUsedGPUCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_gpuquota_used_gpu_count",
			Help: "GPUs currently used by InferenceServices in a GPUQuota's scope.",
		},
		[]string{"gpuquota", "namespace"},
	)

	GPUQuotaGPUCountLimit = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_gpuquota_gpu_count_limit",
			Help: "The GPU cap (spec.gpuCount) declared by a GPUQuota. 0 means no cap.",
		},
		[]string{"gpuquota", "namespace"},
	)

	GPUQuotaAdmissionDenialsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llmkube_gpuquota_admission_denials_total",
			Help: "InferenceService admissions denied by a GPUQuota, by quota.",
		},
		[]string{"gpuquota", "namespace"},
	)

	GPUQuotaUsedVRAMBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_gpuquota_used_vram_bytes",
			Help: "Device memory (bytes) currently accounted to InferenceServices in a GPUQuota's scope. Workloads whose footprint cannot be derived contribute zero.",
		},
		[]string{"gpuquota", "namespace"},
	)

	GPUQuotaVRAMBytesLimit = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llmkube_gpuquota_vram_bytes_limit",
			Help: "The device-memory cap (spec.vramBytes) declared by a GPUQuota. 0 means no cap.",
		},
		[]string{"gpuquota", "namespace"},
	)
)

// AllCollectors is every metric this operator registers. init() registers
// these and the tests range over the same slice, so a metric cannot be added
// to one list and silently missed in the other.
var AllCollectors = []prometheus.Collector{
	GPUQuotaUsedGPUCount,
	GPUQuotaGPUCountLimit,
	GPUQuotaAdmissionDenialsTotal,
	GPUQuotaUsedVRAMBytes,
	GPUQuotaVRAMBytesLimit,
	ModelDownloadDuration,
	ModelStatus,
	InferenceServiceReadyDuration,
	InferenceServicePhase,
	InferenceServiceInfo,
	InferenceServiceReplicas,
	GPUQueueDepth,
	ReconcileTotal,
	ReconcileDuration,
	RouterRequestsTotal,
	RouterRequestDuration,
	RouterFailClosedTotal,
	RouterActiveBackends,
	RouterBackendHealth,
	RouterFirstTokenSeconds,
	RouterBudgetUtilization,
}

func init() {
	ctrlmetrics.Registry.MustRegister(AllCollectors...)
}

// PublishModelPhase makes phase the only llmkube_model_status series for a
// Model. The delete is unconditional so a failed status write cannot strand the
// series it already published. No-ops on an empty phase, so callers can defer it.
// Delete-then-set is not atomic: a scrape landing between the two ops sees no
// series for this Model. Accepted — the window is sub-microsecond.
func PublishModelPhase(name, namespace, phase string) {
	if phase == "" {
		return
	}
	DeleteModelSeries(name, namespace)
	ModelStatus.WithLabelValues(name, namespace, phase).Set(1)
}

// DeleteModelSeries drops every series held for one Model. The phase label is
// open-ended, so the exact series cannot be named.
func DeleteModelSeries(name, namespace string) {
	ModelStatus.DeletePartialMatch(modelLabels(name, namespace))
}

// PublishInferenceServicePhase makes phase the only phase series for one
// InferenceService. Same self-healing argument as PublishModelPhase.
func PublishInferenceServicePhase(name, namespace, phase string) {
	InferenceServicePhase.DeletePartialMatch(inferenceServiceLabels(name, namespace))
	InferenceServicePhase.WithLabelValues(name, namespace, phase).Set(1)
}

// PublishInferenceServiceInfo makes accelerator/runtime the only info series for
// one InferenceService, so changing either in place does not leave both.
func PublishInferenceServiceInfo(name, namespace, accelerator, runtime string) {
	InferenceServiceInfo.DeletePartialMatch(inferenceServiceLabels(name, namespace))
	InferenceServiceInfo.WithLabelValues(name, namespace, accelerator, runtime).Set(1)
}

// PublishInferenceServiceReplicas keeps the state label vocabulary next to the
// declaration that documents it.
func PublishInferenceServiceReplicas(name, namespace string, ready, desired int32) {
	InferenceServiceReplicas.WithLabelValues(name, namespace, "ready").Set(float64(ready))
	InferenceServiceReplicas.WithLabelValues(name, namespace, "desired").Set(float64(desired))
}

// DeleteInferenceServiceSeries drops the state gauges held for one
// InferenceService. Cumulative metrics are deliberately kept: resetting
// InferenceServiceReadyDuration would break rate() across a recreate.
func DeleteInferenceServiceSeries(name, namespace string) {
	labels := inferenceServiceLabels(name, namespace)
	InferenceServicePhase.DeletePartialMatch(labels)
	InferenceServiceInfo.DeletePartialMatch(labels)
	InferenceServiceReplicas.DeletePartialMatch(labels)
}

func modelLabels(name, namespace string) prometheus.Labels {
	return prometheus.Labels{"model": name, "namespace": namespace}
}

func inferenceServiceLabels(name, namespace string) prometheus.Labels {
	return prometheus.Labels{"inferenceservice": name, "namespace": namespace}
}
