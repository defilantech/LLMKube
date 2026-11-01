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

package metrics

import (
	"slices"
	"testing"
	"time"

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
	if len(RouterCollectors) == 0 {
		t.Fatal("RouterCollectors is empty")
	}
	for _, c := range RouterCollectors {
		err := RouterRegistry.Register(c)
		if err == nil {
			t.Errorf("collector %T was not registered by init()", c)
			continue
		}
		if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
			t.Errorf("collector %T: unexpected error: %v", c, err)
		}
	}
}

// Every binary that imports this package and serves controller-runtime's
// registry would export what an import registered there (#2294). Each binary
// registers its own list; cmd and cmd/foreman-operator test theirs.
func TestImportRegistersNothingInControllerRuntime(t *testing.T) {
	for _, c := range slices.Concat(ControllerCollectors, ForemanCollectors, RouterCollectors) {
		if ctrlmetrics.Registry.Unregister(c) {
			t.Errorf("collector %T was registered in controller-runtime's registry on import", c)
		}
	}
}

// RouterRegistry is what the router-proxy serves, and the proxy sets none of
// the controller's collectors, so one there is exported at zero (#2294).
// Compared by descriptor: a labelled collector with no series is absent from
// a scrape but not from Describe.
func TestRouterRegistryHoldsNoControllerCollector(t *testing.T) {
	served := describedNames(t, RouterRegistry)
	shared := describedNames(t, ModelPoolResident)
	for name := range describedNames(t, slices.Concat(ControllerCollectors, ForemanCollectors)...) {
		if served[name] && !shared[name] {
			t.Errorf("RouterRegistry holds %s, which only an operator binary sets", name)
		}
	}
}

func describedNames(t *testing.T, collectors ...prometheus.Collector) map[string]bool {
	t.Helper()

	descs := make(chan *prometheus.Desc)
	go func() {
		defer close(descs)
		for _, c := range collectors {
			c.Describe(descs)
		}
	}()

	names := map[string]bool{}
	for desc := range descs {
		m := descFQName.FindStringSubmatch(desc.String())
		if m == nil {
			t.Fatalf("no fqName in Desc %s", desc)
		}
		names[m[1]] = true
	}
	return names
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
	PublishGPUQueueDepth(map[string]int32{"default": 3, "team-a": 1})

	if got := testutil.ToFloat64(GPUQueueDepth.WithLabelValues("default")); got != 3 {
		t.Errorf("expected default depth 3, got %f", got)
	}
	if got := testutil.ToFloat64(GPUQueueDepth.WithLabelValues("team-a")); got != 1 {
		t.Errorf("expected team-a depth 1, got %f", got)
	}

	// Read values before counting series: WithLabelValues resurrects a dropped
	// series at 0 rather than reporting it missing.
	PublishGPUQueueDepth(map[string]int32{"default": 0})
	if got := testutil.ToFloat64(GPUQueueDepth.WithLabelValues("default")); got != 0 {
		t.Errorf("expected default depth 0 once the queue drained, got %f", got)
	}
	if got := GPUQueueDepth.DeletePartialMatch(prometheus.Labels{"namespace": "team-a"}); got != 0 {
		t.Errorf("expected team-a to stop being reported, got %d series", got)
	}

	GPUQueueDepth.Reset()
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

func TestRecordTaskOutcomeCounter(t *testing.T) {
	// The completed counter is unconditional: every terminal task emits it,
	// keyed by the bounded label set (agent, kind, verdict, outcome).
	RecordTaskOutcome("coder", "issue-fix", "GO", "MODEL-DECIDED", 42.0, 17)

	var m dto.Metric
	obs := ForemanTaskCompletedTotal.WithLabelValues("coder", "issue-fix", "GO", "MODEL-DECIDED")
	if err := obs.Write(&m); err != nil {
		t.Fatalf("failed to write metric: %v", err)
	}
	if got := m.GetCounter().GetValue(); got < 1 {
		t.Errorf("expected completed counter >= 1, got %f", got)
	}
}

func TestRecordTaskOutcomeDuration(t *testing.T) {
	// A positive elapsedSec must land in the duration histogram.
	RecordTaskOutcome("coder", "issue-fix", "NO-GO", "ALREADY-RESOLVED", 88.0, 0)
	m := getHistogramMetric(t, ForemanTaskDurationSeconds, []string{"coder", "issue-fix", "NO-GO"}, 1.0)
	if m.GetHistogram().GetSampleCount() == 0 {
		t.Error("expected duration sample count > 0 after a positive observation")
	}
}

func TestRecordTaskOutcomeSkipsZeroDurationAndTurns(t *testing.T) {
	// A cascade-skip / pre-report terminal task has no elapsedSec or turns:
	// the histograms must NOT observe zero, or the distribution skews low.
	RecordTaskOutcome("verifier", "verify", "GATE-PASS", "", 0, 0)

	// Read the series directly without observing (getHistogramMetric would
	// observe its own value and inflate the count).
	durObs, err := ForemanTaskDurationSeconds.GetMetricWithLabelValues("verifier", "verify", "GATE-PASS")
	if err != nil {
		t.Fatalf("failed to get duration metric: %v", err)
	}
	var dm dto.Metric
	if err := durObs.(prometheus.Metric).Write(&dm); err != nil {
		t.Fatalf("failed to write duration metric: %v", err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("zero elapsedSec must not observe duration, got %d samples", got)
	}

	turnsObs, err := ForemanTaskTurns.GetMetricWithLabelValues("verifier", "verify")
	if err != nil {
		t.Fatalf("failed to get turns metric: %v", err)
	}
	var tm dto.Metric
	if err := turnsObs.(prometheus.Metric).Write(&tm); err != nil {
		t.Fatalf("failed to write turns metric: %v", err)
	}
	if got := tm.GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("zero turns must not observe the turns histogram, got %d samples", got)
	}
}

func TestRecordTaskOutcomeTurns(t *testing.T) {
	// A positive turn count must land in the turns histogram.
	RecordTaskOutcome("coder", "issue-fix", "GO", "", 12.0, 9)
	m := getHistogramMetric(t, ForemanTaskTurns, []string{"coder", "issue-fix"}, 3.0)
	if m.GetHistogram().GetSampleCount() == 0 {
		t.Error("expected turns sample count > 0 after a positive observation")
	}
}

func TestRecordWorkloadOutcome(t *testing.T) {
	RecordWorkloadOutcome("completed")
	RecordWorkloadOutcome("failed")

	var m dto.Metric
	if err := ForemanWorkloadCompletedTotal.WithLabelValues("completed").Write(&m); err != nil {
		t.Fatalf("failed to write completed metric: %v", err)
	}
	if got := m.GetCounter().GetValue(); got < 1 {
		t.Errorf("expected completed workload counter >= 1, got %f", got)
	}
	if err := ForemanWorkloadCompletedTotal.WithLabelValues("failed").Write(&m); err != nil {
		t.Fatalf("failed to write failed metric: %v", err)
	}
	if got := m.GetCounter().GetValue(); got < 1 {
		t.Errorf("expected failed workload counter >= 1, got %f", got)
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

func TestWatchCanaryMetrics(t *testing.T) {
	at := time.Unix(1750000000, 0)
	RecordWatchCanaryObservation(at)
	if got := testutil.ToFloat64(WatchCanaryLastObservedTimestamp); got != float64(at.Unix()) {
		t.Errorf("expected last-observed timestamp %d, got %f", at.Unix(), got)
	}

	SetWatchCanaryUp()
	if got := testutil.ToFloat64(WatchCanaryUp); got != 1 {
		t.Errorf("expected watch-canary up = 1, got %f", got)
	}

	RecordWatchCanaryWrite("ok")
	RecordWatchCanaryWrite("error")
	if got := testutil.ToFloat64(WatchCanaryWritesTotal.WithLabelValues("ok")); got < 1 {
		t.Errorf("expected ok write counter >= 1, got %f", got)
	}
	if got := testutil.ToFloat64(WatchCanaryWritesTotal.WithLabelValues("error")); got < 1 {
		t.Errorf("expected error write counter >= 1, got %f", got)
	}
}

func TestSetUnreconciledGeneration(t *testing.T) {
	SetUnreconciledGeneration("Model", 2, 25*time.Minute)
	if got := testutil.ToFloat64(UnreconciledGenerationObjects.WithLabelValues("Model")); got != 2 {
		t.Errorf("expected 2 lagging Models, got %f", got)
	}
	if got := testutil.ToFloat64(UnreconciledGenerationOldestSeconds.WithLabelValues("Model")); got != 1500 {
		t.Errorf("expected the oldest lag in seconds (1500), got %f", got)
	}

	// A caught-up kind publishes 0, not its last lagging value.
	SetUnreconciledGeneration("Model", 0, 0)
	if got := testutil.ToFloat64(UnreconciledGenerationObjects.WithLabelValues("Model")); got != 0 {
		t.Errorf("expected 0 after catch-up, got %f", got)
	}
	if got := testutil.ToFloat64(UnreconciledGenerationOldestSeconds.WithLabelValues("Model")); got != 0 {
		t.Errorf("expected oldest 0 after catch-up, got %f", got)
	}
}
