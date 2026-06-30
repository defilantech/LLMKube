package grounding

import "testing"

func TestDetectUngroundedReferences(t *testing.T) {
	gt := &GroundTruth{
		Groups:     map[string]bool{"inference.llmkube.dev": true},
		Kinds:      map[string]bool{"InferenceService": true},
		SpecFields: map[string]map[string]bool{"InferenceService": {"modelRef": true, "turboQuantBits": true}},
		Metrics:    map[string]bool{}, CLICmds: map[string]bool{},
	}
	added := []AddedLine{
		// good block: correct group, metadata.name must NOT be flagged, real spec field passes
		{File: "good.md", Line: 1, Text: "apiVersion: inference.llmkube.dev/v1alpha1"},
		{File: "good.md", Line: 2, Text: "kind: InferenceService"},
		{File: "good.md", Line: 3, Text: "metadata:"},
		{File: "good.md", Line: 4, Text: "  name: my-svc"},
		{File: "good.md", Line: 5, Text: "spec:"},
		{File: "good.md", Line: 6, Text: "  modelRef: mistral-7b"},
		// bad block: wrong group (line 5) + invented spec field (line 9); real field (line 8) passes
		{File: "bad.md", Line: 5, Text: "apiVersion: llmkube.io/v1alpha1"},
		{File: "bad.md", Line: 6, Text: "kind: InferenceService"},
		{File: "bad.md", Line: 7, Text: "spec:"},
		{File: "bad.md", Line: 8, Text: "  turboQuantBits: 8"},
		{File: "bad.md", Line: 9, Text: "  servingMode: chat"},
		// external block: never judged
		{File: "ext.md", Line: 3, Text: "apiVersion: apps/v1"},
		{File: "ext.md", Line: 4, Text: "spec:"},
		{File: "ext.md", Line: 5, Text: "  replicas: 3"},
	}
	got := DetectUngroundedReferences(added, gt)
	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2 (bad group + invented field): %v", len(got), got)
	}
	if got[0].Line != 5 || got[1].Line != 9 {
		t.Errorf("unexpected finding lines (want 5 then 9): %v", got)
	}
	for _, f := range got {
		if f.Area != "doc-consistency" || f.Severity != "blocker" {
			t.Errorf("wrong severity/area: %+v", f)
		}
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

func TestDetectUngroundedReferences_478Fixture(t *testing.T) {
	gt := &GroundTruth{
		Groups:     map[string]bool{"inference.llmkube.dev": true},
		Kinds:      map[string]bool{"InferenceService": true},
		SpecFields: map[string]map[string]bool{"InferenceService": {"modelRef": true, "mode": true, "resources": true, "autoscaling": true}},
	}
	added := []AddedLine{
		{File: "docs/site/guides/metrics-driven-autoscaling.md", Line: 10, Text: "apiVersion: llmkube.io/v1alpha1"},
		{File: "docs/site/guides/metrics-driven-autoscaling.md", Line: 11, Text: "kind: InferenceService"},
		{File: "docs/site/guides/metrics-driven-autoscaling.md", Line: 12, Text: "spec:"},
		{File: "docs/site/guides/metrics-driven-autoscaling.md", Line: 13, Text: "  servingMode: chat"},
	}
	got := DetectUngroundedReferences(added, gt)
	if len(got) < 2 {
		t.Fatalf("expected the wrong group AND the invented field to be flagged, got %v", got)
	}
}
