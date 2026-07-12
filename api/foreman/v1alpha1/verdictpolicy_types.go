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

// VerdictPolicy controls which work classes a coder GO may self-certify
// (proposal 1075, section 3.2). Absent fields take the harness defaults.
type VerdictPolicy struct {
	// SelfGO lists work classes allowed to self-certify GO. Classes:
	// code-fix, docs, packaging, config, ci-policy, release-policy.
	// Default (nil): [code-fix, docs, packaging, config]; ci-policy and
	// release-policy always require human sign-off unless listed here.
	// +optional
	// +kubebuilder:validation:items:Enum=code-fix;docs;packaging;config;ci-policy;release-policy
	SelfGO []string `json:"selfGO,omitempty"`
}

// DefaultSelfGO is the work-class self-certification default (proposal
// 1075, section 3.1): the footprint classes a coder's own GO verdict may
// stand for without further human sign-off when a Workload/AgenticTask
// carries no VerdictPolicy, or an explicit one with an empty SelfGO.
// ci-policy and release-policy are deliberately absent -- the fast
// in-workspace gate (fmt/vet/build/lint) can tell a GitHub Actions
// workflow or a release config still parses and the repo still builds,
// but it cannot tell whether the workflow logic or release behavior is
// actually correct.
//
// This is the single source of truth for the default list.
// pkg/foreman/agent's defaultSelfGO variable is declared as a direct
// reference to DefaultSelfGO rather than a second copy of the same four
// literals: api/foreman/v1alpha1 cannot import pkg/foreman/agent (that
// package already imports this one for every CRD type it consumes, so
// the reverse import would cycle), which is why the literal classes live
// here rather than as string(workClassX) conversions of the agent
// package's workClass constants. Keeping exactly one declaration avoids
// the two lists silently drifting apart.
var DefaultSelfGO = []string{"code-fix", "docs", "packaging", "config"}

// Resolve returns the effective self-GO allowlist: a nil receiver or an
// empty SelfGO resolves to DefaultSelfGO; an explicit non-empty SelfGO is
// returned as given. Mirrors GateProfile.Resolve's nil-safe pattern.
//
// Resolve is a pure function with no I/O.
func (p *VerdictPolicy) Resolve() []string {
	if p == nil || len(p.SelfGO) == 0 {
		return DefaultSelfGO
	}
	return p.SelfGO
}
