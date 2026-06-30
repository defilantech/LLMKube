package grounding

import (
	"os"
	"path/filepath"
	"testing"
)

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

// repoRoot resolves the repository root from this package dir
// (pkg/foreman/agent/grounding -> four levels up).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "..", "..", "..", ".."))
}

func TestLoadGroundTruth_RealRepoMetricsRepoWide(t *testing.T) {
	root := repoRoot(t)
	gt, err := LoadGroundTruth(filepath.Join(root, "config/crd/bases"), root, "")
	if err != nil {
		t.Fatal(err)
	}
	// A real metal-agent metric defined in pkg/agent (NOT internal/metrics) must
	// be in the ground truth, else the gate would false-positive real Metal docs.
	if !gt.Metrics["llmkube_metal_agent_apple_power_gpu_watts"] {
		t.Errorf("repo-wide metric scan missed llmkube_metal_agent_apple_power_gpu_watts; have %d metrics", len(gt.Metrics))
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
