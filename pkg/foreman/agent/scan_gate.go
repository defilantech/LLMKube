package agent

import (
	"context"
	"fmt"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// ScanJobRunner submits a clean-room gate Job that builds each release
// image for repository@branch cloned from cloneURL and runs a
// container-image vulnerability scan over it, polls to terminal, and
// reports the outcome. It lives in this package (not tools) because tools
// imports agent (for ToolResult), so the agent package cannot import tools
// without a cycle; cmd/foreman-agent wires a closure over the tools-side
// scan gate Job runner into the executor's ScanJobRunner field.
type ScanJobRunner interface {
	// Run reports (pass, ran, feedback). ran is false when the scan Job
	// could not be submitted/polled to a verdict (infra error, deadline
	// kill, registry outage); the caller treats that as could-not-verify.
	// When ran is true, pass reflects the scan verdict and feedback carries
	// the finding table on failure.
	//
	// taskNamespace/taskName identify the originating AgenticTask so the
	// submitted Job carries the foreman.llmkube.dev/task-{namespace,name}
	// labels and a Job/pod can be traced back to its task (#893 parity).
	//
	// upstreamURL is the CANONICAL repo clone URL, separate from cloneURL
	// (where the branch lives, the fork) for the same reason EnvtestJobRunner
	// documents (#1259/#1731).
	Run(
		ctx context.Context,
		taskNamespace, taskName, repository, branch, cloneURL, upstreamURL string,
	) (pass bool, ran bool, feedback string)
}

// scanGateVerdict is the outcome of one post-push scan gate attempt.
type scanGateVerdict int

const (
	// scanGateOK: the scan passed, or no scan gate is declared, or no runner
	// is wired. The GO may stand.
	scanGateOK scanGateVerdict = iota
	// scanGateFailed: the scan ran and at least one blocking finding remains.
	// Feed the findings back and retry, or downgrade at the iteration bound.
	scanGateFailed
	// scanGateUnverified: a scan gate is declared and a runner is wired, but
	// the scan could not be run to a verdict. On the FIRST attempt the GO
	// stands (no prior evidence of a finding); on a retry it must NOT, or a
	// fix nobody confirmed would land as a false GO (#768's rule).
	scanGateUnverified
)

// evaluatePostPushScan classifies the post-push container-image scan gate
// for one attempt. It returns (verdict, feedback): scanGateFailed carries
// the finding table; scanGateOK covers pass / undeclared / nil-runner;
// scanGateUnverified means the scan could not be run to a verdict. The
// caller decides what an unverified scan means by attempt number (GO stands
// on attempt 0, not on a retry).
func evaluatePostPushScan(
	ctx context.Context,
	scanDeclared bool,
	runner ScanJobRunner,
	taskNamespace, taskName, repository, branch, cloneURL, upstreamURL string,
) (scanGateVerdict, string) {
	if !scanDeclared || runner == nil {
		return scanGateOK, ""
	}
	pass, ran, fb := runner.Run(
		ctx, taskNamespace, taskName, repository, branch, cloneURL, upstreamURL)
	if !ran {
		return scanGateUnverified, ""
	}
	if !pass {
		return scanGateFailed, fb
	}
	return scanGateOK, ""
}

// defaultMaxScanIterations is the retry bound used when
// Agent.spec.maxScanIterations is unset. One mirrors a single human
// "the scan failed, fix it" round; an explicit 0 opts back into the
// fail-on-first-scan-failure behavior.
const defaultMaxScanIterations = 1

// effectiveMaxScanIterations resolves the *int32 three-state: nil (or
// a nil agent) defaults; explicit values (including 0) win.
func effectiveMaxScanIterations(agent *foremanv1alpha1.Agent) int {
	if agent == nil || agent.Spec.MaxScanIterations == nil {
		return defaultMaxScanIterations
	}
	return int(*agent.Spec.MaxScanIterations)
}

// scanFeedbackPrompt renders the retry coder prompt after a post-push
// container-image scan gate failure. Sibling to envtestFeedbackPrompt: it
// states that the scan failed on the pushed branch, that the prior attempt
// is already in the workspace (amend minimally, don't rebuild from
// scratch), that the workspace cannot build or scan a container image at
// all — no container runtime, no image build toolchain, no cluster access
// — so the coder fixes the Dockerfile/base image/packaging and re-pushes
// for the gate to re-run, and that the fixes that clear such findings are
// packaging-level. It appends the truncated finding table.
func scanFeedbackPrompt(feedback string) string {
	return fmt.Sprintf(
		"The post-push container-image scan gate failed on your pushed branch (verdict SCAN-FAIL).\n"+
			"Your workspace already contains your previous attempt: its files and history\n"+
			"are present. Do not rebuild the fix from scratch. Amend the existing work with\n"+
			"the smallest changes that clear the findings, then re-push so the scan re-runs.\n"+
			"This workspace cannot build or scan a container image: there is no container\n"+
			"runtime, no image build toolchain, and no cluster access here. Do NOT attempt\n"+
			"`docker`, `buildah`, `docker build`, or `trivy` — none of them are available\n"+
			"and they will not work. The fixes that clear such findings are packaging-level:\n"+
			"bump or replace the base image tag, `apk upgrade` (or pin) the vulnerable\n"+
			"package, or drop a file the image must not ship (a vendored node_modules, an\n"+
			"unused binary from the base). Fix the Dockerfile, base image, or packaging and\n"+
			"re-push; the scan re-runs after the push.\n"+
			"\nScan findings:\n%s\n",
		truncateGateOutput(feedback))
}
