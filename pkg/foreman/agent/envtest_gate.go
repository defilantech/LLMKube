package agent

import "context"

// EnvtestJobRunner submits a clean-room gate Job that runs `make test`
// (envtest, with KUBEBUILDER_ASSETS) on repository@branch cloned from
// cloneURL, polls to terminal, and reports the outcome. It lives in this
// package (not tools) because tools imports agent (for ToolResult), so the
// agent package cannot import tools without a cycle; cmd/foreman-agent wires
// a closure over tools.RunGateJobTool into the executor's EnvtestJobRunner
// field.
type EnvtestJobRunner interface {
	// Run reports (pass, ran, feedback). ran is false when the Job could
	// not be submitted/polled to a verdict (infra error/timeout); the
	// caller treats that as could-not-verify (GO stands). When ran is true,
	// pass reflects the gate verdict and feedback carries the log tail on
	// failure.
	//
	// taskNamespace/taskName identify the originating AgenticTask so the
	// submitted gate Job carries the foreman.llmkube.dev/task-{namespace,name}
	// labels and a Job/pod can be traced back to its task (#893). They mirror
	// the coder Job path's TaskNamespace/TaskName.
	Run(
		ctx context.Context,
		taskNamespace, taskName, repository, branch, cloneURL string,
	) (pass bool, ran bool, feedback string)
}

// envtestGateVerdict is the outcome of one post-push envtest gate attempt.
type envtestGateVerdict int

const (
	// envtestGateOK: the gate passed, the change touched no envtest package,
	// or no runner is wired. The GO may stand.
	envtestGateOK envtestGateVerdict = iota
	// envtestGateFailed: the gate ran and at least one check failed. Feed the
	// output back and retry, or downgrade at the iteration bound.
	envtestGateFailed
	// envtestGateUnverified: the change touched an envtest package and a runner
	// is wired, but the gate could not be run to a verdict (Job submit/poll
	// error, name collision, timeout). On the FIRST attempt the GO stands (the
	// pre-#768 could-not-verify behavior). On a retry it must NOT: a prior
	// attempt already failed the gate, so an unverifiable re-gate cannot confirm
	// the fix, and letting it stand emits a false GO (#768 validation).
	envtestGateUnverified
)

// evaluatePostPushEnvtest classifies the post-push envtest gate for one
// attempt. It returns (verdict, feedback): envtestGateFailed carries the log
// tail; envtestGateOK covers pass / untouched / nil-runner; envtestGateUnverified
// means the gate could not be run to a verdict. The caller decides what an
// unverified gate means by attempt number (GO stands on attempt 0, not on a
// retry).
func evaluatePostPushEnvtest(
	ctx context.Context,
	envtestTouched bool,
	runner EnvtestJobRunner,
	taskNamespace, taskName, repository, branch, cloneURL string,
) (envtestGateVerdict, string) {
	if !envtestTouched || runner == nil {
		return envtestGateOK, ""
	}
	pass, ran, fb := runner.Run(ctx, taskNamespace, taskName, repository, branch, cloneURL)
	if !ran {
		return envtestGateUnverified, ""
	}
	if !pass {
		return envtestGateFailed, fb
	}
	return envtestGateOK, ""
}
