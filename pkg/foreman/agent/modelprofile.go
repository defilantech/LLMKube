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

// applyModelProfile layers a resolved ModelProfile onto an already-assembled
// LoopConfig, in place, immediately before loop.Run. Precedence:
//
//   - systemPromptAddendum: always appended (never substitutes the Agent's
//     systemPrompt), separated by a blank line.
//   - stuckLoopDetection: the Agent's own block wins wholesale; the profile
//     fills in only when the Agent set none. Non-zero profile fields override
//     the resolved Progress (which is DefaultProgressConfig when the Agent
//     block is nil); zero fields inherit.
//   - restrictReadsInForcingPhase: copied through.
//
// The role-aware invariant from progressConfigFromAgent is preserved: a
// reviewer never runs the edit-free signal, so a profile cannot re-enable it.
// A nil profile is a no-op.
func applyModelProfile(cfg *LoopConfig, agent *foremanv1alpha1.Agent, profile *foremanv1alpha1.ModelProfile) {
	if profile == nil {
		return
	}
	if add := profile.Spec.SystemPromptAddendum; add != "" {
		if cfg.SystemPrompt != "" {
			cfg.SystemPrompt += "\n\n" + add
		} else {
			cfg.SystemPrompt = add
		}
	}
	if agent.Spec.StuckLoopDetection == nil && profile.Spec.StuckLoopDetection != nil {
		s := profile.Spec.StuckLoopDetection
		if s.RepeatedToolThreshold != 0 {
			cfg.Progress.RepeatedToolThreshold = int(s.RepeatedToolThreshold)
		}
		if s.EditFreeTurnsLimit != 0 {
			cfg.Progress.EditFreeTurnsLimit = int(s.EditFreeTurnsLimit)
		}
		if s.ContextSoftCap != 0 {
			cfg.Progress.ContextSoftCap = int(s.ContextSoftCap)
		}
		if s.ContextHardCap != 0 {
			cfg.Progress.ContextHardCap = int(s.ContextHardCap)
		}
	}
	cfg.RestrictReadsInForcingPhase = profile.Spec.RestrictReadsInForcingPhase
	if agent.Spec.Role == foremanv1alpha1.AgentRoleReviewer {
		cfg.Progress.EditFreeTurnsLimit = 0
	}
}
