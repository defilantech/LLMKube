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

package slicer

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// writeRepo materializes files under a temp dir and returns its path.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// stubModel is a canned ModelCaller: no live model in unit tests.
type stubModel struct {
	resp string
	err  error
}

func (s stubModel) Call(string) (string, error) { return s.resp, s.err }

func TestPinnedCheck_PresentIsClean(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"config/monitoring/exp.yaml": "emits rocm_smi_gpu_temp_deg here",
		"config/grafana/dash.json":   `{"expr":"rocm_smi_gpu_temp_deg"}`,
	})
	ids := []SharedIdentifier{{ID: "rocm_smi_gpu_temp_deg", DefinedBy: "exp", ReferencedBy: []string{"dash"}}}
	sf := map[string][]string{"exp": {"config/monitoring/exp.yaml"}, "dash": {"config/grafana/dash.json"}}
	if drifts := PinnedCheck(ids, repo, sf); len(drifts) != 0 {
		t.Fatalf("want clean, got %+v", drifts)
	}
}

func TestPinnedCheck_MissingFromDefinerFlags(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"config/monitoring/exp.yaml": "emits rocm_smi_temperature_edge instead",
		"config/grafana/dash.json":   `{"expr":"rocm_smi_gpu_temp_deg"}`,
	})
	ids := []SharedIdentifier{{ID: "rocm_smi_gpu_temp_deg", DefinedBy: "exp", ReferencedBy: []string{"dash"}}}
	sf := map[string][]string{"exp": {"config/monitoring/exp.yaml"}, "dash": {"config/grafana/dash.json"}}
	drifts := PinnedCheck(ids, repo, sf)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %+v", drifts)
	}
	got := drifts[0]
	if got.Identifier != "rocm_smi_gpu_temp_deg" || got.Slice != "exp" || got.Kind != DriftPinnedMissing {
		t.Fatalf("unexpected drift: %+v", got)
	}
}

// A pin must match a WHOLE TOKEN, not a substring: the different metric
// rocm_smi_gpu_temperature must NOT satisfy the pin rocm_smi_gpu_temp
// (regression the live validation run caught).
func TestPinnedCheck_SuperstringIsNotAMatch(t *testing.T) {
	repo := writeRepo(t, map[string]string{"docs/g.md": "the exporter emits rocm_smi_gpu_temperature per gpu"})
	ids := []SharedIdentifier{{ID: "rocm_smi_gpu_temp", DefinedBy: "docs"}}
	sf := map[string][]string{"docs": {"docs/g.md"}}
	drifts := PinnedCheck(ids, repo, sf)
	if len(drifts) != 1 || drifts[0].Identifier != "rocm_smi_gpu_temp" {
		t.Fatalf("want 1 drift for rocm_smi_gpu_temp, got %+v", drifts)
	}
}

// rocm_smi_gpu_temp must not be satisfied by rocm_smi_gpu_temp_degC (a
// different metric); the exact token IS a match.
// A pin that is itself a prefix (e.g. "rocm_smi_") can never match as a whole
// token because the trailing underscore is an identifier-continuation byte:
// "rocm_smi_sensor_temperature" contains the prefix as a substring but never as
// a whole token. This is the bug from #1058 — the planner pinned "rocm_smi_"
// and reconcile reported a spurious pinned-missing GATE-FAIL.
func TestPinnedCheck_PrefixPinNeverMatchesWholeToken(t *testing.T) {
	repo := writeRepo(t, map[string]string{
		"config/monitoring/exp.yaml": "emits rocm_smi_sensor_temperature here",
	})
	ids := []SharedIdentifier{{ID: "rocm_smi_", DefinedBy: "exp"}}
	sf := map[string][]string{"exp": {"config/monitoring/exp.yaml"}}
	drifts := PinnedCheck(ids, repo, sf)
	if len(drifts) != 1 || drifts[0].Identifier != "rocm_smi_" {
		t.Fatalf("prefix pin must be reported missing, got %+v", drifts)
	}
}

func TestPinnedCheck_SuffixedTokenIsNotAMatch(t *testing.T) {
	ids := []SharedIdentifier{{ID: "rocm_smi_gpu_temp", DefinedBy: "a"}}
	sf := map[string][]string{"a": {"a.yaml"}}

	suffixed := writeRepo(t, map[string]string{"a.yaml": "rocm_smi_gpu_temp_degC only"})
	if drifts := PinnedCheck(ids, suffixed, sf); len(drifts) != 1 {
		t.Fatalf("suffixed: want 1 drift, got %+v", drifts)
	}

	exact := writeRepo(t, map[string]string{"a.yaml": `value rocm_smi_gpu_temp{gpu="0"} 42`})
	if drifts := PinnedCheck(ids, exact, sf); len(drifts) != 0 {
		t.Fatalf("exact: want clean, got %+v", drifts)
	}
}

func TestLLMSweep_ParsesFlaggedDrift(t *testing.T) {
	model := stubModel{resp: "```yaml\n- identifier: foo_metric\n  slice: dash\n  reason: mismatch\n```"}
	drifts := LLMSweep("diff", "contract", model)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %+v", drifts)
	}
	if drifts[0].Identifier != "foo_metric" || drifts[0].Kind != DriftLLMFlagged {
		t.Fatalf("unexpected drift: %+v", drifts[0])
	}
}

func TestLLMSweep_NoneIsClean(t *testing.T) {
	if drifts := LLMSweep("d", "c", stubModel{resp: "NONE"}); len(drifts) != 0 {
		t.Fatalf("want clean, got %+v", drifts)
	}
}

func TestLLMSweep_MalformedIsClean(t *testing.T) {
	if drifts := LLMSweep("d", "c", stubModel{resp: "garbage {["}); len(drifts) != 0 {
		t.Fatalf("want clean, got %+v", drifts)
	}
}

func TestLLMSweep_ModelErrorIsClean(t *testing.T) {
	if drifts := LLMSweep("d", "c", stubModel{err: errors.New("unreachable")}); len(drifts) != 0 {
		t.Fatalf("want clean, got %+v", drifts)
	}
}

func TestReconcile_ComposesPinnedAndLLM(t *testing.T) {
	repo := writeRepo(t, map[string]string{"a.yaml": "wrong_name"})
	plan := SlicePlan{
		Contract:          "c",
		SharedIdentifiers: []SharedIdentifier{{ID: "right_name", DefinedBy: "a"}},
		Slices:            []Slice{{Name: "a", Files: []string{"a.yaml"}}},
	}
	model := stubModel{resp: "- identifier: other\n  slice: a\n  reason: r"}
	res := Reconcile(plan, repo, "diff", model)
	if res.Clean {
		t.Fatalf("want not clean")
	}
	kinds := make([]string, 0, len(res.Drifts))
	for _, d := range res.Drifts {
		kinds = append(kinds, d.Kind)
	}
	sort.Strings(kinds)
	if len(kinds) != 2 || kinds[0] != DriftLLMFlagged || kinds[1] != DriftPinnedMissing {
		t.Fatalf("want [llm-flagged pinned-missing], got %v", kinds)
	}
}
