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

package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func samplePlan() slicePlan {
	return slicePlan{
		Issue:    700,
		Repo:     "defilantech/LLMKube",
		Contract: "all slices agree on the metric names",
		SharedIdentifiers: []planSharedID{
			{ID: "rocm_smi_gpu_temp", DefinedBy: "exporter", ReferencedBy: []string{"dashboard"}},
		},
		Slices: []planSlice{
			{Name: "exporter", Files: []string{"config/exp.yaml"}, Task: "emit rocm_smi_gpu_temp"},
			{Name: "dashboard", Files: []string{"config/dash.json"}, Task: "query rocm_smi_gpu_temp"},
		},
	}
}

func defaultOpts() *sliceOptions {
	return &sliceOptions{
		namespace: "default", coderAgent: "coder-metal",
		integrateAgent: "integrate", reconcileAgent: "reconcile", baseBranch: "main",
	}
}

func TestBuildSliceWorkload_PipelineShape(t *testing.T) {
	wl := buildSliceWorkload(samplePlan(), defaultOpts())

	if wl.Name != "slicer-700" || wl.Namespace != "default" {
		t.Fatalf("meta = %s/%s", wl.Namespace, wl.Name)
	}
	steps := wl.Spec.Pipeline
	if len(steps) != 4 {
		t.Fatalf("want 4 steps (2 slices + integrate + reconcile), got %d", len(steps))
	}

	// slice steps
	for i, name := range []string{"exporter", "dashboard"} {
		s := steps[i]
		if s.Name != name || s.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
			t.Fatalf("step %d = %s/%s, want %s/issue-fix", i, s.Name, s.Kind, name)
		}
		wantBranch := "foreman/slicer-700/" + name
		if s.Payload.Branch != wantBranch {
			t.Errorf("slice %s branch = %q, want %q", name, s.Payload.Branch, wantBranch)
		}
		if s.AgentRef.Name != "coder-metal" {
			t.Errorf("slice %s agent = %q", name, s.AgentRef.Name)
		}
		if !strings.Contains(s.Payload.Prompt, "config/") || !strings.Contains(s.Payload.Prompt, "ONE SLICE") {
			t.Errorf("slice %s prompt missing scope/contract: %q", name, s.Payload.Prompt)
		}
	}

	// integrate step
	integ := steps[2]
	if integ.Name != "integrate" || integ.Kind != foremanv1alpha1.AgenticTaskKindIntegrate {
		t.Fatalf("integrate step = %s/%s", integ.Name, integ.Kind)
	}
	if got := integ.DependsOn; len(got) != 2 || got[0] != "exporter" || got[1] != "dashboard" {
		t.Errorf("integrate dependsOn = %v, want [exporter dashboard]", got)
	}
	if len(integ.Payload.Slices) != 2 || integ.Payload.Slices[0].Branch != "foreman/slicer-700/exporter" {
		t.Errorf("integrate payload slices wrong: %+v", integ.Payload.Slices)
	}

	// reconcile step
	rec := steps[3]
	if rec.Name != "reconcile" || rec.Kind != foremanv1alpha1.AgenticTaskKindReconcile {
		t.Fatalf("reconcile step = %s/%s", rec.Name, rec.Kind)
	}
	if len(rec.DependsOn) != 1 || rec.DependsOn[0] != "integrate" {
		t.Errorf("reconcile dependsOn = %v, want [integrate]", rec.DependsOn)
	}
	if len(rec.Payload.SharedIdentifiers) != 1 || rec.Payload.SharedIdentifiers[0].ID != "rocm_smi_gpu_temp" {
		t.Errorf("reconcile pins wrong: %+v", rec.Payload.SharedIdentifiers)
	}
	if rec.Payload.Contract == "" || rec.Payload.Branch != "foreman/slicer-700/integ" {
		t.Errorf("reconcile payload missing contract/branch: %+v", rec.Payload)
	}
	// reconcile carries files (for pinned_check), integrate carries branches.
	if len(rec.Payload.Slices) != 2 || len(rec.Payload.Slices[0].Files) == 0 {
		t.Errorf("reconcile payload slices need files: %+v", rec.Payload.Slices)
	}
}

func TestValidateSlicePlan(t *testing.T) {
	valid := samplePlan()
	if err := validateSlicePlan(valid); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}

	overlap := samplePlan()
	overlap.Slices[1].Files = []string{"config/exp.yaml"} // same file as slice 0
	if err := validateSlicePlan(overlap); err == nil {
		t.Error("overlapping files must be rejected")
	}

	noIssue := samplePlan()
	noIssue.Issue = 0
	if err := validateSlicePlan(noIssue); err == nil {
		t.Error("missing issue must be rejected")
	}

	noSlices := samplePlan()
	noSlices.Slices = nil
	if err := validateSlicePlan(noSlices); err == nil {
		t.Error("no slices must be rejected")
	}
}

func TestRunSlice_DryRunRendersYAML(t *testing.T) {
	dir := t.TempDir()
	planFile := dir + "/plan.yaml"
	planYAML := `issue: 700
repo: defilantech/LLMKube
contract: |
  agree on names
shared_identifiers:
  - id: rocm_smi_gpu_temp
    defined_by: exporter
    referenced_by: [dashboard]
slices:
  - name: exporter
    files: [config/exp.yaml]
    task: emit the metric
  - name: dashboard
    files: [config/dash.json]
    task: query the metric
`
	if err := os.WriteFile(planFile, []byte(planYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := defaultOpts()
	opts.planFile = planFile
	opts.dryRun = true

	var buf bytes.Buffer
	if err := runSlice(context.Background(), &buf, opts); err != nil {
		t.Fatalf("runSlice: %v", err)
	}
	out := buf.String()
	wants := []string{"kind: Workload", "name: slicer-700", "kind: integrate", "kind: reconcile", "rocm_smi_gpu_temp"}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, out)
		}
	}
}
