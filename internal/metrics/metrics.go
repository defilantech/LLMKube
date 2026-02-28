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
			Help: "Current status of models. Value encodes phase: 0=Unknown, 1=Downloading, 2=Copying, 3=Ready, 4=Cached, 5=Failed.",
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

	GPUQueueDepth = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_gpu_queue_depth",
			Help: "Number of InferenceServices waiting for GPU resources.",
		},
	)

	GPUQueueWaitDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llmkube_gpu_queue_wait_duration_seconds",
			Help:    "Time InferenceServices spend waiting for GPU resources.",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10), // 10s to ~5120s
		},
		[]string{"inferenceservice", "namespace"},
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

	// Aggregate gauges

	ActiveModelsTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_active_models_total",
			Help: "Total number of models in Ready/Cached phase.",
		},
	)

	ActiveInferenceServicesTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "llmkube_active_inferenceservices_total",
			Help: "Total number of inference services in Ready phase.",
		},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		ModelDownloadDuration,
		ModelStatus,
		InferenceServiceReadyDuration,
		InferenceServicePhase,
		GPUQueueDepth,
		GPUQueueWaitDuration,
		ReconcileTotal,
		ReconcileDuration,
		ActiveModelsTotal,
		ActiveInferenceServicesTotal,
	)
}
