# Foreman envtest feedback loop (in-executor)

**Issue:** #768 (envtest half of the cluster-backed gate feedback loop)
**Date:** 2026-07-15
**Status:** Approved design, pre-implementation

## Problem

The Foreman coder gate has two tiers. The fast in-workspace tier (fmt / vet /
build / lint / unit tests, #749/#762) feeds failures back to the coder loop via
the `VerifyTerminal` hook, so a coder iterates until those checks pass. The
heavyweight tier, the post-push envtest gate (#859/#864), runs `make test`
(envtest, `KUBEBUILDER_ASSETS`) in a clean-room Kubernetes Job on the pushed
branch. That tier is **terminal**: `evaluatePostPushEnvtest` captures the gate
failure output as `feedback`, but `envtestGateFailedResult` only downgrades the
GO to INCOMPLETE (outcome `ENVTEST-GATE-FAILED`). The feedback goes nowhere.

The consequence is structural. The coder cannot run envtest in its workspace
(no cluster; the envtest trap of #734 shows it hangs), so on any change whose
behavior only manifests against envtest-backed reconcile code, the coder gets
exactly one blind shot: it writes the change, cannot verify it, submits GO, and
the post-push gate fails with no path to iterate. Landing such a change today
requires a human to re-dispatch the coder by hand with the gate output pasted
in (observed repeatedly while landing the #1110 HF-revision slice).

The re-dispatch machinery already exists but only for a different trigger:
`workload_iteration.go` (#946) re-dispatches a coder on **reviewer NO-GO**, with
the feedback distilled into `payload.prompt` and the prior attempt restored via
`ReviseFromBranch`, bounded by `maxReviewIterations` (default 1). The envtest
gate simply never feeds into it.

## Goal

Automate the manual loop: when the post-push envtest gate fails, feed the gate
output back to the coder and let it iterate, bounded by a cap, before falling
back to today's INCOMPLETE. Coverage is **universal** — it must work for every
coder issue-fix task (issue-batch Workloads, hand-authored Pipeline Workloads,
and bare AgenticTasks), not just the reconciler-synthesized issue-batch
pipeline. That requirement places the loop in the executor, not the Workload
controller.

Non-goals (deferred to separate cycles): the e2e/kind gate tier (`make
test-e2e` in a privileged Job); seed-transcript conversation continuity; a
Workload-level override of the cap.

## Approach

An outer retry loop in `runLLMPath` (executor_native.go) wraps the existing
commit -> push -> envtest-gate sequence. Attempt 0 is exactly today's behavior.
On a gate failure with budget remaining, the executor re-runs a focused coder
loop against the same still-live workspace with the gate output injected into
the user prompt, then re-commits, force-pushes (force-with-lease), and re-gates.
When the cap is exhausted it falls back to today's `envtestGateFailedResult`
(INCOMPLETE). This inlines the proven #946 re-dispatch semantics so they apply
to every Workload shape, at the cost of one small executor test seam.

Rejected alternatives:
- **Extend `VerifyTerminal` to run the envtest gate in-loop.** The verifier runs
  pre-commit/pre-push in-workspace; the envtest gate needs a pushed branch
  (the Job clones `repository@branch`). Unifying would move commit+push
  ownership into the verify hook and force a push on every candidate terminal:
  a large, risky refactor.
- **Seed-transcript continuation.** Continue the same conversation across
  retries for full context. Adds conversation-continuation plumbing to
  `loop.Run` and cross-retry context-window management for little gain: the
  live workspace plus the injected feedback already carry the state, which is
  exactly what the proven reviewer re-dispatch provides.

## Control flow

`runLLMPath` currently runs, in sequence: `loop.Run` -> `repo.Commit` ->
`repo.Push` -> `evaluatePostPushEnvtest` -> (fail) `envtestGateFailedResult`.
The commit -> push -> gate span becomes a bounded loop:

```
attempt := 0
for {
    sha = repo.Commit(...)
    repo.Push(..., ReplaceOnReject: attempt > 0 || payload.AllowOverwrite)
    failed, feedback := evaluatePostPushEnvtest(...)
    if !failed {
        break                                  // GO path continues as today
    }
    if attempt >= maxEnvtestIterations {
        return envtestGateFailedResult(..., feedback)   // INCOMPLETE, as today
    }
    attempt++
    loopRes, loopErr = e.runLoop(ctx, retryCfg(cfg, feedback))  // focused re-run, same workspace
    if loopErr != nil || !isGO(loopRes) {
        return terminalFor(loopRes, loopErr)   // coder gave up / NO-GO; do not push failing work
    }
    // loop back: re-commit -> re-push -> re-gate
}
// on break: goResult + advisories + applyWorkClassPolicyForTask, unchanged
```

The coder-grounding and no-functional-change advisories and
`applyWorkClassPolicyForTask` run once, after the loop settles on a GO, exactly
as today. The workspace is not torn down between attempts (the executor owns it
until it returns), so each retry amends the prior attempt's files in place.

## Components

- **Cap knob — `Agent.spec.maxEnvtestIterations *int32`.** Three-valued: nil ->
  default 1; explicit 0 -> today's fail-on-first-gate-failure; N -> up to N
  retries. Mirrors `Workload.spec.maxReviewIterations`. Agent-level (every task
  has an Agent, including bare AgenticTasks) as required by universal coverage.
  Requires `make manifests` + `make foreman-chart-crds` (and, per repo
  convention, the CRD sync check).

- **`envtestFeedbackPrompt(feedback string) string`.** Sibling to
  `reviewFeedbackPrompt`. States that the envtest gate failed on the pushed
  branch, that the prior work is already present in the workspace, that the coder
  should amend it minimally to fix exactly the reported failures, followed by the
  truncated gate log tail (reusing the existing gate-output truncation).

- **`retryCfg(cfg LoopConfig, feedback string) LoopConfig`.** Returns a copy of
  the resolved loop config with `UserPrompt` rebuilt as the original issue
  context plus the `envtestFeedbackPrompt` section. Every other field (system
  prompt, `VerifyTerminal` fast gate, model profile, budgets) is unchanged, so
  the retry still runs behind the fast gate and the coder is not blind.

- **`runLoop` seam.** A field on `NativeAgentLoopExecutor`,
  `runLoop func(context.Context, LoopConfig) (*LoopResult, error)`, defaulting to
  `loop.Run`. The initial call at line ~583 and the retry call both route through
  it. This is the only loop-adjacent change and exists purely so tests can inject
  a fake loop. No behavior change to `loop.Run` itself.

## Data flow

The gate output originates in the `EnvtestJobRunner.Run` return (`feedback`,
the failing `make test` log tail), surfaces through `evaluatePostPushEnvtest`,
is truncated and embedded into the retry's `UserPrompt` by `retryCfg` /
`envtestFeedbackPrompt`, and reaches the model as the issue-context slot of the
next focused loop. On cap exhaustion the final `feedback` still flows into
`envtestGateFailedResult`'s status extra exactly as today.

## Error handling and edges

- **Coder NO-GO / MaxTurns on a retry:** return that terminal directly. The
  executor never pushes work the coder itself did not GO. Bounded by the cap and
  by each loop's own MaxTurns, so no unbounded spin.
- **Gate could-not-run (`ran=false`):** unchanged. `evaluatePostPushEnvtest`
  returns `failed=false`, the GO stands, no retry is spent on an infra hiccup.
- **Non-coder / non-issue-fix tasks:** `maxEnvtestIterations` is consulted only
  where the envtest gate already runs (coder role, envtest-touched change).
  Every other role/kind path is untouched.
- **Force-push safety:** retries push to the task's own branch with
  `ReplaceOnReject` (force-with-lease), the same compare-and-swap the reviewer
  re-dispatch uses; no unrelated ref is at risk.
- **Transcript:** each attempt's transcript is persisted (WriteTranscript per
  iteration). The returned result references the final attempt; earlier attempts
  remain for audit.

## Testing

Unit tests inject a fake `runLoop` and a fake `EnvtestJobRunner`:

1. **Converges:** gate fails once then passes -> exactly one retry, result GO.
2. **Cap exhausted:** gate fails forever -> INCOMPLETE (`ENVTEST-GATE-FAILED`)
   after exactly `maxEnvtestIterations` retries, and no more.
3. **NO-GO on retry:** the retry loop returns NO-GO -> that terminal surfaces,
   no further push.
4. **Could-not-run:** `ran=false` -> GO stands, zero retries.
5. **Cap resolution:** nil -> 1, explicit 0 -> no retry, N honored.

Existing envtest-gate and executor tests must continue to pass unchanged for the
attempt-0 path.
