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

// TestEffectiveScanGate pins the step-over-workload resolution the
// reconciler applies to every rendered AgenticTask (the ScanGate mirror of
// TestEffectiveGateProfile): a step's own gate wins, an unset step falls
// back to the Workload default, and both unset leaves nil — i.e. no scan
// gate, the pre-feature behavior.
func TestEffectiveScanGate(t *testing.T) {
	agentImage := &foremanv1alpha1.ScanGate{Images: []string{"foreman-agent"}}
	all := &foremanv1alpha1.ScanGate{}

	cases := []struct {
		name     string
		step     *foremanv1alpha1.ScanGate
		workload *foremanv1alpha1.ScanGate
		want     *foremanv1alpha1.ScanGate
	}{
		{"both unset -> nil (no scan gate)", nil, nil, nil},
		{"workload default applies when step unset", nil, agentImage, agentImage},
		{"step gate wins over workload default", all, agentImage, all},
		{"step gate applies when workload unset", all, nil, all},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &foremanv1alpha1.Workload{
				Spec: foremanv1alpha1.WorkloadSpec{ScanGate: tc.workload},
			}
			step := foremanv1alpha1.PipelineStep{ScanGate: tc.step}
			if got := effectiveScanGate(step, w); got != tc.want {
				t.Errorf("effectiveScanGate() = %v, want %v", got, tc.want)
			}
		})
	}
}
