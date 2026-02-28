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
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// getHistogramMetric is a test helper that observes a value and reads the metric.
func getHistogramMetric(t *testing.T, h *prometheus.HistogramVec, labels []string, value float64) *dto.Metric {
	t.Helper()
	h.WithLabelValues(labels...).Observe(value)
	observer, err := h.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("failed to get metric: %v", err)
	}
	var m dto.Metric
	if err := observer.(prometheus.Metric).Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	return &m
}

func TestMetricsRegistered(t *testing.T) {
	// Verify all metrics are registered by checking that re-registration
	// fails with AlreadyRegisteredError. This confirms init() ran correctly.
	collectors := []struct {
		name      string
		collector prometheus.Collector
	}{
		{"llmkube_model_download_duration_seconds", ModelDownloadDuration},
		{"llmkube_model_status", ModelStatus},
		{"llmkube_inferenceservice_ready_duration_seconds", InferenceServiceReadyDuration},
		{"llmkube_inferenceservice_phase", InferenceServicePhase},
		{"llmkube_gpu_queue_depth", GPUQueueDepth},
		{"llmkube_gpu_queue_wait_duration_seconds", GPUQueueWaitDuration},
		{"llmkube_reconcile_total", ReconcileTotal},
		{"llmkube_reconcile_duration_seconds", ReconcileDuration},
		{"llmkube_active_models_total", ActiveModelsTotal},
		{"llmkube_active_inferenceservices_total", ActiveInferenceServicesTotal},
	}

	for _, c := range collectors {
		t.Run(c.name, func(t *testing.T) {
			err := ctrlmetrics.Registry.Register(c.collector)
			if err == nil {
				t.Errorf("metric %q was not already registered — init() did not register it", c.name)
				// Unregister if we accidentally succeeded
				ctrlmetrics.Registry.Unregister(c.collector)
			} else if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				t.Errorf("unexpected error registering %q: %v", c.name, err)
			}
		})
	}
}

func TestModelDownloadDuration(t *testing.T) {
	m := getHistogramMetric(t, ModelDownloadDuration, []string{"dl-test", "default", "http"}, 5.5)

	if m.GetHistogram().GetSampleCount() == 0 {
		t.Error("expected sample count > 0 after observation")
	}
	if m.GetHistogram().GetSampleSum() < 5.5 {
		t.Errorf("expected sample sum >= 5.5, got %f", m.GetHistogram().GetSampleSum())
	}
}

func TestModelStatus(t *testing.T) {
	ModelStatus.WithLabelValues("status-test", "default", "Ready").Set(1)

	var m dto.Metric
	if err := ModelStatus.WithLabelValues("status-test", "default", "Ready").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 1 {
		t.Errorf("expected gauge value 1, got %f", m.GetGauge().GetValue())
	}
}

func TestInferenceServicePhase(t *testing.T) {
	InferenceServicePhase.WithLabelValues("phase-test", "default", "Creating").Set(1)

	var m dto.Metric
	if err := InferenceServicePhase.WithLabelValues("phase-test", "default", "Creating").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 1 {
		t.Errorf("expected gauge value 1, got %f", m.GetGauge().GetValue())
	}

	// Verify phase transition clears old phase
	InferenceServicePhase.WithLabelValues("phase-test", "default", "Creating").Set(0)
	InferenceServicePhase.WithLabelValues("phase-test", "default", "Ready").Set(1)

	if err := InferenceServicePhase.WithLabelValues("phase-test", "default", "Creating").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 0 {
		t.Errorf("expected Creating phase to be 0 after transition, got %f", m.GetGauge().GetValue())
	}

	if err := InferenceServicePhase.WithLabelValues("phase-test", "default", "Ready").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 1 {
		t.Errorf("expected Ready phase to be 1 after transition, got %f", m.GetGauge().GetValue())
	}
}

func TestGPUQueueDepth(t *testing.T) {
	GPUQueueDepth.Set(3)

	var m dto.Metric
	if err := GPUQueueDepth.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 3 {
		t.Errorf("expected gauge value 3, got %f", m.GetGauge().GetValue())
	}

	GPUQueueDepth.Set(0)
	if err := GPUQueueDepth.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 0 {
		t.Errorf("expected gauge value 0, got %f", m.GetGauge().GetValue())
	}
}

func TestReconcileTotal(t *testing.T) {
	ReconcileTotal.WithLabelValues("test-ctrl", "success").Inc()
	ReconcileTotal.WithLabelValues("test-ctrl", "success").Inc()
	ReconcileTotal.WithLabelValues("test-ctrl", "error").Inc()

	var m dto.Metric
	if err := ReconcileTotal.WithLabelValues("test-ctrl", "success").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetCounter().GetValue() < 2 {
		t.Errorf("expected counter >= 2, got %f", m.GetCounter().GetValue())
	}

	if err := ReconcileTotal.WithLabelValues("test-ctrl", "error").Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetCounter().GetValue() < 1 {
		t.Errorf("expected counter >= 1, got %f", m.GetCounter().GetValue())
	}
}

func TestReconcileDuration(t *testing.T) {
	m := getHistogramMetric(t, ReconcileDuration, []string{"dur-model"}, 0.5)
	if m.GetHistogram().GetSampleCount() == 0 {
		t.Error("expected model reconcile duration sample count > 0")
	}

	m = getHistogramMetric(t, ReconcileDuration, []string{"dur-inferenceservice"}, 1.2)
	if m.GetHistogram().GetSampleCount() == 0 {
		t.Error("expected inferenceservice reconcile duration sample count > 0")
	}
}

func TestActiveGauges(t *testing.T) {
	ActiveModelsTotal.Set(5)
	ActiveInferenceServicesTotal.Set(3)

	var m dto.Metric
	if err := ActiveModelsTotal.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 5 {
		t.Errorf("expected 5 active models, got %f", m.GetGauge().GetValue())
	}

	if err := ActiveInferenceServicesTotal.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if m.GetGauge().GetValue() != 3 {
		t.Errorf("expected 3 active inference services, got %f", m.GetGauge().GetValue())
	}
}

func TestHistogramBuckets(t *testing.T) {
	tests := []struct {
		name    string
		metric  *prometheus.HistogramVec
		labels  []string
		minBkts int
	}{
		{"ModelDownloadDuration", ModelDownloadDuration, []string{"bkt-test", "default", "http"}, 12},
		{"InferenceServiceReadyDuration", InferenceServiceReadyDuration, []string{"bkt-test", "default"}, 10},
		{"GPUQueueWaitDuration", GPUQueueWaitDuration, []string{"bkt-test", "default"}, 10},
		{"ReconcileDuration", ReconcileDuration, []string{"bkt-test-ctrl"}, 11},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := getHistogramMetric(t, tt.metric, tt.labels, 1.0)
			bucketCount := len(m.GetHistogram().GetBucket())
			if bucketCount < tt.minBkts {
				t.Errorf("expected at least %d buckets, got %d", tt.minBkts, bucketCount)
			}
		})
	}
}
