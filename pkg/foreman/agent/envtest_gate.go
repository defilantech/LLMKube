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

// evaluatePostPushEnvtest decides whether a pushed GO should be downgraded
// because its envtest packages fail in the clean-room gate Job. It returns
// (failed, feedback): failed is true ONLY when the change touched an envtest
// package, the Job ran, and it did not pass. A nil runner, an untouched
// change, or a could-not-run (ran=false) never downgrades.
func evaluatePostPushEnvtest(
	ctx context.Context,
	envtestTouched bool,
	runner EnvtestJobRunner,
	taskNamespace, taskName, repository, branch, cloneURL string,
) (failed bool, feedback string) {
	if !envtestTouched || runner == nil {
		return false, ""
	}
	pass, ran, fb := runner.Run(ctx, taskNamespace, taskName, repository, branch, cloneURL)
	if !ran {
		return false, ""
	}
	if !pass {
		return true, fb
	}
	return false, ""
}
