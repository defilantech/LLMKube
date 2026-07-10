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

package controller

import (
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// TestClassifyChildren_SlicerVerdicts locks in that a sliced Workload's
// integrate and reconcile steps roll up through the same verdict-based
// classification as every other kind (#1033). The rollup is deliberately
// kind-agnostic: a Succeeded + GATE-FAIL reconcile (pinned interface drift) or
// integrate (overlap / stale-base apply) lands in the incomplete bucket, which
// keeps the Workload out of Completed; a clean GATE-PASS counts as succeeded.
// If a future change special-cases kinds in classifyChildren, this fails.
func TestClassifyChildren_SlicerVerdicts(t *testing.T) {
	mk := func(kind foremanv1alpha1.AgenticTaskKind, verdict foremanv1alpha1.AgenticTaskVerdict) foremanv1alpha1.AgenticTask {
		return foremanv1alpha1.AgenticTask{
			Spec:   foremanv1alpha1.AgenticTaskSpec{Kind: kind},
			Status: foremanv1alpha1.AgenticTaskStatus{Phase: foremanv1alpha1.AgenticTaskPhaseSucceeded, Verdict: verdict},
		}
	}

	tests := []struct {
		name           string
		task           foremanv1alpha1.AgenticTask
		wantSucceeded  int32
		wantIncomplete int32
	}{
		{"integrate clean", mk(foremanv1alpha1.AgenticTaskKindIntegrate, foremanv1alpha1.AgenticTaskVerdictGatePass), 1, 0},
		{"integrate overlap", mk(foremanv1alpha1.AgenticTaskKindIntegrate, foremanv1alpha1.AgenticTaskVerdictGateFail), 0, 1},
		{"reconcile clean", mk(foremanv1alpha1.AgenticTaskKindReconcile, foremanv1alpha1.AgenticTaskVerdictGatePass), 1, 0},
		{"reconcile drift", mk(foremanv1alpha1.AgenticTaskKindReconcile, foremanv1alpha1.AgenticTaskVerdictGateFail), 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := classifyChildren([]foremanv1alpha1.AgenticTask{tc.task})
			if c.succeeded != tc.wantSucceeded || c.incomplete != tc.wantIncomplete {
				t.Fatalf("classifyChildren = {succeeded:%d incomplete:%d}, want {succeeded:%d incomplete:%d}",
					c.succeeded, c.incomplete, tc.wantSucceeded, tc.wantIncomplete)
			}
		})
	}
}
