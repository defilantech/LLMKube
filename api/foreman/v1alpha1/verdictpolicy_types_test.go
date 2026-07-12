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

package v1alpha1

import (
	"reflect"
	"testing"
)

func TestVerdictPolicyResolve(t *testing.T) {
	tests := []struct {
		name   string
		policy *VerdictPolicy
		want   []string
	}{
		{
			name:   "nil policy resolves to the default self-GO list",
			policy: nil,
			want:   DefaultSelfGO,
		},
		{
			name:   "empty SelfGO resolves to the default self-GO list",
			policy: &VerdictPolicy{},
			want:   DefaultSelfGO,
		},
		{
			name:   "explicit SelfGO is returned as given",
			policy: &VerdictPolicy{SelfGO: []string{"code-fix", "ci-policy"}},
			want:   []string{"code-fix", "ci-policy"},
		},
		{
			name:   "explicit single-class SelfGO is returned as given",
			policy: &VerdictPolicy{SelfGO: []string{"release-policy"}},
			want:   []string{"release-policy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.Resolve()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
