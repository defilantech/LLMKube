package grounding

import "testing"

func TestDetectUngroundedReferences_Group(t *testing.T) {
	gt := &GroundTruth{Groups: map[string]bool{"inference.llmkube.dev": true}}
	added := []AddedLine{
		{File: "good.md", Line: 1, Text: "apiVersion: inference.llmkube.dev/v1alpha1"},
		{File: "bad.md", Line: 5, Text: "apiVersion: llmkube.io/v1alpha1"},
		{File: "ext.md", Line: 3, Text: "apiVersion: apps/v1"},
	}
	got := DetectUngroundedReferences(added, gt)
	if len(got) != 1 || got[0].Line != 5 {
		t.Fatalf("want exactly 1 finding at line 5 (unknown llmkube group), got %v", got)
	}
	if got[0].Area != "doc-consistency" || got[0].Severity != "blocker" {
		t.Errorf("wrong severity/area: %+v", got[0])
	}
}

func TestDetectUngroundedReferences_478Fixture(t *testing.T) {
	gt := &GroundTruth{
		Groups:  map[string]bool{"inference.llmkube.dev": true},
		Metrics: map[string]bool{"llmkube_inferenceservice_phase": true},
	}
	added := []AddedLine{
		{File: "docs/site/guides/metrics-driven-autoscaling.md", Line: 32, Text: "apiVersion: llmkube.io/v1alpha1"},
		{
			File: "docs/site/guides/metrics-driven-autoscaling.md", Line: 87,
			Text: "scrape llmkube_inferenceservice_request_rate from /metrics",
		},
	}
	got := DetectUngroundedReferences(added, gt)
	if len(got) < 2 {
		t.Fatalf("expected the wrong group AND the invented metric flagged, got %v", got)
	}
}

func TestDetectUngroundedReferences_MetricAndCLI(t *testing.T) {
	gt := &GroundTruth{
		Groups: map[string]bool{}, Kinds: map[string]bool{}, SpecFields: map[string]map[string]bool{},
		Metrics: map[string]bool{"llmkube_inferenceservice_phase": true},
		CLICmds: map[string]bool{"deploy": true},
	}
	added := []AddedLine{
		{File: "d.md", Line: 1, Text: "scrape llmkube_inferenceservice_request_rate from /metrics"},
		{File: "d.md", Line: 2, Text: "run `llmkube load --endpoint ...`"},
		{File: "d.md", Line: 3, Text: "run `llmkube deploy model.yaml`  # real"},
	}
	got := DetectUngroundedReferences(added, gt)
	if len(got) != 2 {
		t.Fatalf("want 2 (bad metric + bad cli), got %d: %v", len(got), got)
	}
}

func TestDetectUngrounded_FlagsUnknownDCGMMetric(t *testing.T) {
	gt := &GroundTruth{ExporterMetricPrefixes: []string{"DCGM_FI_"}}
	added := []AddedLine{{File: "docs/x.md", Line: 3, Text: "scrape dcgm_gpu_utilization from the exporter"}}
	if len(DetectUngroundedReferences(added, gt)) == 0 {
		t.Fatal("dcgm_gpu_utilization should be flagged (not a DCGM_FI_ metric, not llmkube_)")
	}
}

func TestDetectUngrounded_AcceptsRealDCGMMetric(t *testing.T) {
	gt := &GroundTruth{ExporterMetricPrefixes: []string{"DCGM_FI_"}}
	added := []AddedLine{{File: "docs/x.md", Line: 3, Text: "scrape DCGM_FI_DEV_GPU_UTIL now"}}
	if f := DetectUngroundedReferences(added, gt); len(f) != 0 {
		t.Fatalf("real DCGM metric should not be flagged, got %v", f)
	}
}
