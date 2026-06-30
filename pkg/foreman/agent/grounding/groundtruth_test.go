package grounding

import "testing"

func TestLoadGroundTruth_MetricsAndCLI(t *testing.T) {
	gt, err := LoadGroundTruth("testdata/crd-bases", "testdata/metrics", "testdata/cmd")
	if err != nil {
		t.Fatal(err)
	}
	if !gt.Metrics["llmkube_inferenceservice_phase"] {
		t.Errorf("missing metric; have %v", gt.Metrics)
	}
	if !gt.CLICmds["deploy"] {
		t.Errorf("missing cli command; have %v", gt.CLICmds)
	}
}

func TestLoadGroundTruth_FromCRDBases(t *testing.T) {
	gt, err := LoadGroundTruth("testdata/crd-bases", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !gt.Groups["inference.llmkube.dev"] {
		t.Errorf("missing group inference.llmkube.dev; have %v", gt.Groups)
	}
	if !gt.Kinds["InferenceService"] {
		t.Errorf("missing kind InferenceService")
	}
	if !gt.SpecFields["InferenceService"]["modelRef"] || !gt.SpecFields["InferenceService"]["turboQuantBits"] {
		t.Errorf("missing spec fields; have %v", gt.SpecFields["InferenceService"])
	}
	if gt.SpecFields["InferenceService"]["bogusField"] {
		t.Errorf("invented field should not be present")
	}
}
