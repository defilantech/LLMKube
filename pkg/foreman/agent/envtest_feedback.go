package agent

import (
	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// defaultMaxEnvtestIterations is the retry bound used when
// Agent.spec.maxEnvtestIterations is unset. One mirrors a single human
// "the gate failed, fix it" round; an explicit 0 opts back into the
// pre-#768 fail-on-first-gate-failure behavior.
const defaultMaxEnvtestIterations = 1

// effectiveMaxEnvtestIterations resolves the *int32 three-state: nil (or
// a nil agent) defaults; explicit values (including 0) win.
func effectiveMaxEnvtestIterations(agent *foremanv1alpha1.Agent) int {
	if agent == nil || agent.Spec.MaxEnvtestIterations == nil {
		return defaultMaxEnvtestIterations
	}
	return int(*agent.Spec.MaxEnvtestIterations)
}
