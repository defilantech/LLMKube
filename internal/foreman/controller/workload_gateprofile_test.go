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

// TestEffectiveGateProfile pins the step-over-workload resolution the
// reconciler applies to every rendered AgenticTask: a step's own profile
// wins, an unset step falls back to the Workload default, and both unset
// leaves nil (the "go" preset, i.e. pre-feature behavior).
func TestEffectiveGateProfile(t *testing.T) {
	node := &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguageNode}
	python := &foremanv1alpha1.GateProfile{Language: foremanv1alpha1.GateLanguagePython}

	cases := []struct {
		name     string
		step     *foremanv1alpha1.GateProfile
		workload *foremanv1alpha1.GateProfile
		want     *foremanv1alpha1.GateProfile
	}{
		{"both unset -> nil (go preset)", nil, nil, nil},
		{"workload default applies when step unset", nil, node, node},
		{"step profile wins over workload default", python, node, python},
		{"step profile applies when workload unset", python, nil, python},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &foremanv1alpha1.Workload{
				Spec: foremanv1alpha1.WorkloadSpec{GateProfile: tc.workload},
			}
			step := foremanv1alpha1.PipelineStep{GateProfile: tc.step}
			if got := effectiveGateProfile(step, w); got != tc.want {
				t.Errorf("effectiveGateProfile() = %v, want %v", got, tc.want)
			}
		})
	}
}
