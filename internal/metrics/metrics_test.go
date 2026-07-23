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
	"github.com/prometheus/client_golang/prometheus/testutil"
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
	if len(AllCollectors) == 0 {
		t.Fatal("AllCollectors is empty")
	}
	for _, c := range AllCollectors {
		err := ctrlmetrics.Registry.Register(c)
		if err == nil {
			t.Errorf("collector %T was not registered by init()", c)
			continue
		}
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			t.Errorf("collector %T: unexpected error: %v", c, err)
		}
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
	labels := inferenceServiceLabels("phase-test", "default")
	InferenceServicePhase.DeletePartialMatch(labels)

	PublishInferenceServicePhase("phase-test", "default", "Creating")
	if got := testutil.ToFloat64(
		InferenceServicePhase.WithLabelValues("phase-test", "default", "Creating")); got != 1 {
		t.Errorf("expected gauge value 1, got %f", got)
	}

	// A phase change drops the old series rather than zeroing it, so the vec
	// holds one series per service no matter how much the phase churns.
	PublishInferenceServicePhase("phase-test", "default", "Ready")
	if got := testutil.ToFloat64(
		InferenceServicePhase.WithLabelValues("phase-test", "default", "Ready")); got != 1 {
		t.Errorf("expected Ready phase to be 1 after transition, got %f", got)
	}
	// WithLabelValues above resurrected Ready at its real value; count after.
	if got := InferenceServicePhase.DeletePartialMatch(labels); got != 1 {
		t.Errorf("expected exactly 1 series after transition, got %d", got)
	}
}

func TestInferenceServiceInfo(t *testing.T) {
	labels := inferenceServiceLabels("info-test", "default")
	InferenceServiceInfo.DeletePartialMatch(labels)

	PublishInferenceServiceInfo("info-test", "default", "cuda", "llamacpp")
	if got := testutil.ToFloat64(
		InferenceServiceInfo.WithLabelValues("info-test", "default", "cuda", "llamacpp")); got != 1 {
		t.Errorf("expected gauge value 1, got %f", got)
	}

	// An in-place accelerator change must not leave both series behind.
	PublishInferenceServiceInfo("info-test", "default", "rocm", "llamacpp")
	if got := testutil.ToFloat64(
		InferenceServiceInfo.WithLabelValues("info-test", "default", "rocm", "llamacpp")); got != 1 {
		t.Errorf("expected gauge value 1 for rocm, got %f", got)
	}
	if got := InferenceServiceInfo.DeletePartialMatch(labels); got != 1 {
		t.Errorf("expected exactly 1 series after an accelerator change, got %d", got)
	}
}

func TestPublishInferenceServiceReplicas(t *testing.T) {
	labels := inferenceServiceLabels("replicas-test", "default")
	InferenceServiceReplicas.DeletePartialMatch(labels)

	PublishInferenceServiceReplicas("replicas-test", "default", 2, 3)
	if got := testutil.ToFloat64(
		InferenceServiceReplicas.WithLabelValues("replicas-test", "default", "ready")); got != 2 {
		t.Errorf("expected ready 2, got %f", got)
	}
	if got := testutil.ToFloat64(
		InferenceServiceReplicas.WithLabelValues("replicas-test", "default", "desired")); got != 3 {
		t.Errorf("expected desired 3, got %f", got)
	}
	if got := InferenceServiceReplicas.DeletePartialMatch(labels); got != 2 {
		t.Errorf("expected exactly 2 series (ready, desired), got %d", got)
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

func TestDeleteModelSeries(t *testing.T) {
	// Counts are taken as deltas: these vecs are package-level and other tests
	// in this file leave series behind.
	before := testutil.CollectAndCount(ModelStatus)

	ModelStatus.WithLabelValues("del-model", "default", "Ready").Set(1)
	ModelStatus.WithLabelValues("del-model", "default", "Failed").Set(1)
	ModelStatus.WithLabelValues("keep-model", "default", "Ready").Set(1)
	if got, want := testutil.CollectAndCount(ModelStatus), before+3; got != want {
		t.Fatalf("setup: got %d series, want %d", got, want)
	}

	DeleteModelSeries("del-model", "default")

	// Both phases of the deleted Model go, the unrelated Model stays.
	if got, want := testutil.CollectAndCount(ModelStatus), before+1; got != want {
		t.Errorf("after delete: got %d series, want %d", got, want)
	}
	if testutil.ToFloat64(ModelStatus.WithLabelValues("keep-model", "default", "Ready")) != 1 {
		t.Error("unrelated Model series was removed")
	}
}

func TestDeleteInferenceServiceSeries(t *testing.T) {
	beforePhase := testutil.CollectAndCount(InferenceServicePhase)
	beforeInfo := testutil.CollectAndCount(InferenceServiceInfo)
	beforeReplicas := testutil.CollectAndCount(InferenceServiceReplicas)

	InferenceServicePhase.WithLabelValues("del-isvc", "default", "Ready").Set(1)
	InferenceServiceInfo.WithLabelValues("del-isvc", "default", "cpu", "llamacpp").Set(1)
	InferenceServiceReplicas.WithLabelValues("del-isvc", "default", "ready").Set(1)
	InferenceServiceReplicas.WithLabelValues("del-isvc", "default", "desired").Set(1)

	DeleteInferenceServiceSeries("del-isvc", "default")

	// Every per-service vec must be covered, or a deleted service keeps
	// reporting through whichever one was missed.
	if got := testutil.CollectAndCount(InferenceServicePhase); got != beforePhase {
		t.Errorf("phase: got %d series, want %d", got, beforePhase)
	}
	if got := testutil.CollectAndCount(InferenceServiceInfo); got != beforeInfo {
		t.Errorf("info: got %d series, want %d", got, beforeInfo)
	}
	if got := testutil.CollectAndCount(InferenceServiceReplicas); got != beforeReplicas {
		t.Errorf("replicas: got %d series, want %d", got, beforeReplicas)
	}
}

func TestDeleteGPUQuotaSeries(t *testing.T) {
	beforeUsedGPU := testutil.CollectAndCount(GPUQuotaUsedGPUCount)
	beforeLimitGPU := testutil.CollectAndCount(GPUQuotaGPUCountLimit)
	beforeUsedVRAM := testutil.CollectAndCount(GPUQuotaUsedVRAMBytes)
	beforeLimitVRAM := testutil.CollectAndCount(GPUQuotaVRAMBytesLimit)

	GPUQuotaUsedGPUCount.WithLabelValues("del-gq", "default").Set(3)
	GPUQuotaGPUCountLimit.WithLabelValues("del-gq", "default").Set(10)
	GPUQuotaUsedVRAMBytes.WithLabelValues("del-gq", "default").Set(16000000000)
	GPUQuotaVRAMBytesLimit.WithLabelValues("del-gq", "default").Set(32000000000)

	DeleteGPUQuotaSeries("del-gq", "default")

	// Every per-quota gauge must be covered, or a deleted quota keeps
	// reporting through whichever one was missed.
	if got := testutil.CollectAndCount(GPUQuotaUsedGPUCount); got != beforeUsedGPU {
		t.Errorf("used gpu: got %d series, want %d", got, beforeUsedGPU)
	}
	if got := testutil.CollectAndCount(GPUQuotaGPUCountLimit); got != beforeLimitGPU {
		t.Errorf("gpu limit: got %d series, want %d", got, beforeLimitGPU)
	}
	if got := testutil.CollectAndCount(GPUQuotaUsedVRAMBytes); got != beforeUsedVRAM {
		t.Errorf("used vram: got %d series, want %d", got, beforeUsedVRAM)
	}
	if got := testutil.CollectAndCount(GPUQuotaVRAMBytesLimit); got != beforeLimitVRAM {
		t.Errorf("vram limit: got %d series, want %d", got, beforeLimitVRAM)
	}
}

func TestGPUQuotaLabels(t *testing.T) {
	labels := gpuQuotaLabels("my-quota", "my-ns")
	if labels["gpuquota"] != "my-quota" {
		t.Errorf("gpuquota label = %q, want %q", labels["gpuquota"], "my-quota")
	}
	if labels["namespace"] != "my-ns" {
		t.Errorf("namespace label = %q, want %q", labels["namespace"], "my-ns")
	}
}

func TestPublishModelPhase(t *testing.T) {
	labels := modelLabels("publish-test", "default")
	ModelStatus.DeletePartialMatch(labels)

	// No phase yet: callers defer this unconditionally, so it must no-op
	// rather than publish an empty label.
	PublishModelPhase("publish-test", "default", "")
	if got := ModelStatus.DeletePartialMatch(labels); got != 0 {
		t.Errorf("empty phase should publish nothing, got %d series", got)
	}

	PublishModelPhase("publish-test", "default", "Downloading")
	PublishModelPhase("publish-test", "default", "Ready")
	if got := testutil.ToFloat64(
		ModelStatus.WithLabelValues("publish-test", "default", "Ready")); got != 1 {
		t.Errorf("expected Ready to be 1, got %f", got)
	}
	if got := ModelStatus.DeletePartialMatch(labels); got != 1 {
		t.Errorf("expected exactly 1 series after a phase change, got %d", got)
	}
}
