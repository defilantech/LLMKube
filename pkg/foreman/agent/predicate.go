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

package agent

import (
	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// IsDeterministicAgent reports whether the Agent runs the model-free
// branch. Deterministic = no LLM at all, only direct tool dispatch (the
// M4 gate Agent shape). A cloud-proxy Agent is NEVER deterministic; it
// always runs the LLM loop against its remote endpoint.
//
// This is the single source of truth for the deterministic predicate. The
// executor's isDeterministicAgent delegates here, and the admission
// webhook keeps a private copy that the webhook tests assert is
// behaviorally identical to this function (the webhook must not import
// this package into the operator binary).
func IsDeterministicAgent(spec foremanv1alpha1.AgentSpec) bool {
	if spec.Provider != "" && spec.Provider != foremanv1alpha1.AgentProviderLocal {
		return false
	}
	return spec.InferenceServiceRef.Name == ""
}

// FirstDeterministicTool returns the first non-terminal tool in the
// agent's whitelist, i.e. the tool the deterministic executor would
// actually dispatch. submit_result is always terminal (the LLM-loop exit
// tool) and empty entries are skipped. Returns "" when no candidate
// exists.
//
// This is the single source of truth for deterministic tool selection.
// The executor's pickDeterministicTool delegates here, and the admission
// webhook keeps a private copy asserted equivalent by the webhook tests.
func FirstDeterministicTool(tools []string) string {
	for _, t := range tools {
		if t == "" || t == "submit_result" {
			continue
		}
		return t
	}
	return ""
}
