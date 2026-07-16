package agent

import (
	"fmt"

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

// envtestFeedbackPrompt renders the retry coder prompt after a post-push
// envtest gate failure (#768). Sibling to reviewFeedbackPrompt: it states
// that the gate failed on the pushed branch, that the prior work is
// already present in the workspace, and that the coder should amend it
// minimally, then appends the truncated gate log tail. It repeats the
// no-envtest-in-workspace directive because running envtest here hangs.
func envtestFeedbackPrompt(feedback string) string {
	return fmt.Sprintf(
		"The post-push envtest gate failed on your pushed branch (verdict GATE-FAIL).\n"+
			"Your workspace already contains your previous attempt: its files and history\n"+
			"are present. Do not rebuild the fix from scratch. Amend the existing work with\n"+
			"the smallest changes that make the failing envtest checks pass, then finish.\n"+
			"Do not run envtest, `make test`, or `go test ./internal/controller/...` here\n"+
			"(this workspace cannot run envtest and it will hang); only `go build ./...`.\n"+
			"\nGate output:\n%s\n",
		truncateGateOutput(feedback))
}

// retryCfg returns a copy of the resolved loop config for a post-push
// envtest gate retry (#768): the user prompt becomes the original issue
// context (base.UserPrompt) plus the gate feedback section, so the retry
// runs with the failure in front of it instead of blind. Every other
// field (system prompt, VerifyTerminal fast gate, model profile, budgets)
// is unchanged. The base is copied by value, so the caller's config is not
// mutated.
func retryCfg(base LoopConfig, feedback string) LoopConfig {
	cfg := base
	cfg.UserPrompt = base.UserPrompt + "\n\n" + envtestFeedbackPrompt(feedback)
	return cfg
}
