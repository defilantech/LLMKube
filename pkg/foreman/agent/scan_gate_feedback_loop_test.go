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

package agent_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	fake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
)

// The post-push container-image scan gate feedback loop (#1798) exercises
// runLLMPath's commit -> push -> gate retry restructure end-to-end, mirroring
// the #768 envtest-loop tests (executor_native_envtest_loop_test.go). The
// shared harness facts apply unchanged: the terminal verdict comes from the
// seqEnvtestRegistry's scripted submit_result verdict, and its touch makes the
// change look envtest-backed (harmless here: with no EnvtestJobRunner wired
// the envtest gate stays silent, and case 7 arms it deliberately).
//
// The cases assert the independent-budget contract: the scan gate's retry
// counter is incremented only by a scan-forced retry, so envtest-forced
// retries never inflate it (a scan that "could not run" on a non-scan retry
// is still at attempt 0 for scan purposes, where the GO stands).

// scanGateResult is one scripted outcome of a post-push scan gate call.
type scanGateResult struct {
	pass, ran bool
	feedback  string
}

// scriptedScanRunner returns results[i] for gate call i, repeating the final
// entry after the script runs out (mirrors scriptedEnvtestRunner). It records
// the scan config each call received so the harness can assert the executor
// resolved the task's ScanGate once at the decision site and handed the same
// concrete config to every call (the runner must never re-resolve).
type scriptedScanRunner struct {
	results  []scanGateResult
	calls    int
	gotScans []foremanv1alpha1.ResolvedScan
}

func (f *scriptedScanRunner) Run(
	_ context.Context, _, _, _, _, _, _ string, scan foremanv1alpha1.ResolvedScan,
) (pass bool, ran bool, feedback string) {
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	f.gotScans = append(f.gotScans, scan)
	r := f.results[i]
	return r.pass, r.ran, r.feedback
}

// scanLoopExecutor assembles the same *NativeAgentLoopExecutor literal the
// envtest-loop tests build, plus the ScanJobRunner under test and (optionally)
// the EnvtestJobRunner where a case arms both gates.
func scanLoopExecutor(
	t *testing.T, root, bare, oaiURL string, c client.Client,
	reg *seqEnvtestRegistry,
	envRunner foremanagent.EnvtestJobRunner, scanRunner foremanagent.ScanJobRunner,
) *foremanagent.NativeAgentLoopExecutor {
	t.Helper()
	return &foremanagent.NativeAgentLoopExecutor{
		Client:                   c,
		WorkspaceRoot:            filepath.Join(root, "ws"),
		GitRemoteURL:             bare,
		UpstreamURLForRepo:       func(string) string { return bare },
		InferenceBaseURLOverride: oaiURL + "/v1",
		CommitAuthor:             repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		CommitCommitter:          repo.Identity{Name: "Foreman Bot", Email: "bot@foreman.test"},
		RegistryFactory: func(
			_ context.Context, ws string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			reg.workspace = ws
			return reg, nil
		},
		AuthFactory:      fakeAuth(t),
		EnvtestJobRunner: envRunner,
		ScanJobRunner:    scanRunner,
	}
}

// scanGateLoopCase configures one end-to-end scan-gate scenario. run drives it
// to a terminal Result so each test body is just its inputs and assertions.
type scanGateLoopCase struct {
	name        string
	maxEnvtest  *int32 // nil -> default bound
	maxScan     *int32 // nil -> default bound
	declared    bool   // task declares a scan gate
	emptyGate   bool   // declares only scanGate: {} (presence semantics)
	oaiBodies   []string
	regVerdicts []string
	envGate     []envtestGateResult // nil -> no envtest runner
	scanGate    []scanGateResult    // nil -> no scan runner
}

func (tc scanGateLoopCase) run(t *testing.T) (*foremanagent.Result, int, int) {
	t.Helper()
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)
	oaiSrv := scriptedOAI(t, tc.oaiBodies)
	agent, task := taskAndAgent(tc.name)
	agent.Spec.MaxEnvtestIterations = tc.maxEnvtest
	agent.Spec.MaxScanIterations = tc.maxScan
	if tc.emptyGate {
		// Presence semantics: an empty-but-present gate arms the scan at
		// Resolve()'s CI defaults. Distinct from "undeclared" (nil).
		task.Spec.ScanGate = &foremanv1alpha1.ScanGate{}
	} else if tc.declared {
		task.Spec.ScanGate = &foremanv1alpha1.ScanGate{Images: []string{"controller"}}
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).
		WithObjects(agent, task).Build()
	reg := &seqEnvtestRegistry{verdicts: tc.regVerdicts}

	// Assign through the interface so an absent runner is a NIL interface,
	// not a typed-nil pointer (the gate checks runner == nil before calling).
	var (
		envRunner  foremanagent.EnvtestJobRunner
		scanRunner foremanagent.ScanJobRunner
		envScript  *scriptedEnvtestRunner
		scanScript *scriptedScanRunner
	)
	if tc.envGate != nil {
		envScript = &scriptedEnvtestRunner{results: tc.envGate}
		envRunner = envScript
	}
	if tc.scanGate != nil {
		scanScript = &scriptedScanRunner{results: tc.scanGate}
		scanRunner = scanScript
	}

	e := scanLoopExecutor(t, root, bare, oaiSrv.URL, c, reg, envRunner, scanRunner)
	res, err := execWithAgent(t, e, task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	envCalls, scanCalls := 0, 0
	if envScript != nil {
		envCalls = envScript.calls
	}
	if scanScript != nil {
		scanCalls = scanScript.calls
		// Every call must carry the executor's resolved view of the task's
		// gate verbatim -- the runner sees a ResolvedScan, never a raw
		// ScanGate, so there is no second defaulting path to drift.
		want := task.Spec.ScanGate.Resolve()
		for i, got := range scanScript.gotScans {
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("scan call %d got config %+v, want the resolved task gate %+v", i, got, want)
			}
		}
	}
	return res, envCalls, scanCalls
}

// The gate fails on attempt 0; with the default bound (1) the executor
// re-runs the coder against the same workspace and re-gates, which passes,
// so the task settles GO after exactly two scan calls.
func TestNativeExecutor_ScanLoop_ConvergesAfterOneRetry(t *testing.T) {
	res, _, calls := scanGateLoopCase{
		name: "scan-converge", declared: true, oaiBodies: []string{submitGoBody},
		regVerdicts: []string{"GO"},
		scanGate: []scanGateResult{
			{pass: false, ran: true, feedback: "CRITICAL: CVE-2024-0001 in openssl"}, // attempt 0 fails
			{pass: true, ran: true}, // retry passes
		},
	}.run(t)
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO got %s (result=%+v)", res.Verdict, res)
	}
	if calls != 2 {
		t.Fatalf("scan calls: want 2 got %d", calls)
	}
}

// Presence, not zero-ness, arms the gate: a task declaring scanGate: {} —
// every field empty — must run the scan at Resolve()'s CI defaults (all
// built-in targets, CRITICAL,HIGH) and feed a failure back consuming the
// scan budget exactly like a gate with explicit fields. The pre-review
// IsZero check treated this YAML as "no gate at all", contradicting
// ScanGate.Images' own "empty means all built-in targets" contract.
func TestNativeExecutor_ScanLoop_EmptyGateIsDeclared(t *testing.T) {
	res, _, calls := scanGateLoopCase{
		name: "scan-empty-gate", emptyGate: true, oaiBodies: []string{submitGoBody},
		regVerdicts: []string{"GO"},
		scanGate: []scanGateResult{
			{pass: false, ran: true, feedback: "CRITICAL: CVE-2024-0002 in libxml2"}, // attempt 0 fails
			{pass: true, ran: true}, // retry passes
		},
	}.run(t)
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO got %s (result=%+v)", res.Verdict, res)
	}
	if calls != 2 {
		t.Fatalf("scan calls: want 2 got %d (an empty gate must still arm the scan)", calls)
	}
}

// A retry whose re-gate cannot be run to a verdict (ran=false: e.g. a gate
// Job name collision) must NOT land as GO. A prior attempt already failed the
// scan, so the executor downgrades to INCOMPLETE rather than let an
// unverified branch through (the #768 unverified-retry rule, scan side).
func TestNativeExecutor_ScanLoop_UnverifiedRetryDoesNotFalseGo(t *testing.T) {
	res, _, calls := scanGateLoopCase{
		name: "scan-unverified", declared: true, oaiBodies: []string{submitGoBody},
		regVerdicts: []string{"GO"},
		scanGate: []scanGateResult{
			{pass: false, ran: true, feedback: "attempt 0 fails"}, // attempt 0: real failure -> retry
			{pass: false, ran: false},                             // retry: could-not-run (infra)
		},
	}.run(t)
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("verdict: want INCOMPLETE (unverified retry must not GO) got %s (result=%+v)", res.Verdict, res)
	}
	if calls != 2 {
		t.Fatalf("scan calls: want 2 got %d", calls)
	}
	if got, _ := res.Extra["outcome"].(string); got != "SCAN-GATE-FAILED" {
		t.Fatalf("outcome: want SCAN-GATE-FAILED got %q", got)
	}
}

// With the bound set to 0, the first scan failure is terminal: the executor
// downgrades to SCAN-GATE-FAILED / INCOMPLETE and never re-runs the coder.
func TestNativeExecutor_ScanLoop_IncompleteAfterCapExhausted(t *testing.T) {
	zero := int32(0)
	res, _, calls := scanGateLoopCase{
		name: "scan-cap", maxScan: &zero, declared: true, oaiBodies: []string{submitGoBody},
		regVerdicts: []string{"GO"},
		scanGate:    []scanGateResult{{pass: false, ran: true, feedback: "still blocked"}},
	}.run(t)
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictIncomplete {
		t.Fatalf("verdict: want INCOMPLETE got %s", res.Verdict)
	}
	if calls != 1 {
		t.Fatalf("scan calls with cap 0: want 1 got %d", calls)
	}
	if got, _ := res.Extra["outcome"].(string); got != "SCAN-GATE-FAILED" {
		t.Fatalf("outcome: want SCAN-GATE-FAILED got %q", got)
	}
	if got, _ := res.Extra["feedback"].(string); got != "still blocked" {
		t.Fatalf("feedback: want the finding table got %q", got)
	}
}

// A scan that could not be run to a verdict (ran=false) on the FIRST attempt
// settles as GO (no prior evidence of a finding) with zero retries: the
// pre-#1798 behavior for an unverifiable gate.
func TestNativeExecutor_ScanLoop_UnverifiedFirstAttemptGoes(t *testing.T) {
	res, _, calls := scanGateLoopCase{
		name: "scan-unverified-first", declared: true, oaiBodies: []string{submitGoBody},
		regVerdicts: []string{"GO"},
		scanGate:    []scanGateResult{{pass: false, ran: false}},
	}.run(t)
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO (attempt-0 could-not-run) got %s (result=%+v)", res.Verdict, res)
	}
	if calls != 1 {
		t.Fatalf("scan calls: want 1 got %d", calls)
	}
}

// A task with no declared scan gate never calls the scan runner, even with
// one wired: the result is byte-identical to today's no-scan behavior.
func TestNativeExecutor_ScanLoop_UndeclaredSkipsRunner(t *testing.T) {
	res, _, calls := scanGateLoopCase{
		name: "scan-undeclared", oaiBodies: []string{submitGoBody},
		regVerdicts: []string{"GO"},
		scanGate:    []scanGateResult{{pass: false, ran: true, feedback: "must not be reached"}},
	}.run(t)
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO got %s (result=%+v)", res.Verdict, res)
	}
	if calls != 0 {
		t.Fatalf("scan calls: want 0 (undeclared) got %d", calls)
	}
}

// Independent budgets (#1798): the envtest gate fails on the first push and
// passes on its one retry; the scan gate PASSES on the first push and then
// cannot be run on the second. The second push exists because the ENVTEST
// gate forced a retry, so for scan purposes it is still attempt 0 -- where a
// could-not-run settles as GO. A shared attempt counter would see the scan's
// second call as attempt 1 and downgrade the whole task to INCOMPLETE
// (unverified-on-retry); the independent counters let the GO stand.
func TestNativeExecutor_ScanLoop_IndependentBudgets(t *testing.T) {
	res, envCalls, scanCalls := scanGateLoopCase{
		name: "scan-independent", declared: true, oaiBodies: []string{submitGoBody},
		regVerdicts: []string{"GO"},
		envGate: []envtestGateResult{
			{pass: false, ran: true, feedback: "envtest boom"}, // attempt 0: envtest fails -> retry
			{pass: true, ran: true},                            // envtest retry passes
		},
		scanGate: []scanGateResult{
			{pass: true, ran: true},   // scan attempt 0: passes
			{pass: false, ran: false}, // push 2: scan could not run, but it is NOT a scan retry
		},
	}.run(t)
	if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("verdict: want GO (envtest-forced retry must not inflate the scan budget) got %s "+
			"(result=%+v)", res.Verdict, res)
	}
	if envCalls != 2 {
		t.Fatalf("envtest calls: want 2 got %d", envCalls)
	}
	if scanCalls != 2 {
		t.Fatalf("scan calls: want 2 got %d", scanCalls)
	}
}

// --- Deterministic verify path: declared scan gate re-runs on GATE-PASS ----

// verifyScanCase configures one deterministic verify scan re-gate scenario.
type verifyScanCase struct {
	name         string
	declared     bool
	scan         []scanGateResult // nil -> no runner wired
	wantVerdict  foremanv1alpha1.AgenticTaskVerdict
	wantFailure  foremanv1alpha1.AgenticTaskFailureReason
	wantOutcome  string // Extra["scanOutcome"]; "" means absent
	wantFindings bool
	wantCalls    int
}

func (tc verifyScanCase) run(t *testing.T) *foremanagent.Result {
	t.Helper()
	gitOrSkip(t)
	root := t.TempDir()
	bare := initBareWithSeed(t, root)

	// A deterministic gate Agent (M4 shape): no inference, the registry's
	// run_gate_job result is the verdict.
	agent := &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "scan-verify-" + tc.name, Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:               foremanv1alpha1.AgentRoleVerifier,
			Tools:              []string{"run_gate_job"},
			RequiredCapability: foremanv1alpha1.RequiredCapability{Roles: []string{"verifier"}},
		},
	}
	task := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: tc.name, Namespace: "default", UID: types.UID("test-uid-" + tc.name),
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: foremanv1alpha1.AgenticTaskKindVerify,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:   "defilantech/LLMKube",
				Issue:  9999,
				Branch: "foreman/issue-9999",
			},
			AgentRef: &corev1.LocalObjectReference{Name: agent.Name},
		},
	}
	if tc.declared {
		task.Spec.ScanGate = &foremanv1alpha1.ScanGate{Images: []string{"controller"}}
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(agent, task).Build()

	reg := &fakeRegistry{
		results: map[string]*foremanagent.ToolResult{
			"run_gate_job": {
				Terminal: true,
				Verdict:  "GATE-PASS",
				Summary:  "all checks green",
				Output:   map[string]any{"jobName": "foreman-gate-fake-001"},
			},
		},
	}
	// Assign through the interface so an absent runner is a nil interface,
	// not a typed-nil pointer (verifyScanReGate checks == nil).
	var (
		scanRunner foremanagent.ScanJobRunner
		scanScript *scriptedScanRunner
	)
	if tc.scan != nil {
		scanScript = &scriptedScanRunner{results: tc.scan}
		scanRunner = scanScript
	}
	e := &foremanagent.NativeAgentLoopExecutor{
		Client:             c,
		WorkspaceRoot:      filepath.Join(root, "ws"),
		GitRemoteURL:       bare,
		UpstreamURLForRepo: func(string) string { return bare },
		CommitAuthor:       repo.Identity{Name: "Bot", Email: "b@x"},
		CommitCommitter:    repo.Identity{Name: "Bot", Email: "b@x"},
		RegistryFactory: func(
			_ context.Context, _ string, _ *foremanv1alpha1.Agent, _ bool,
		) (foremanagent.ToolRegistry, error) {
			return reg, nil
		},
		AuthFactory:   fakeAuth(t),
		ScanJobRunner: scanRunner,
	}

	res, err := execWithAgent(t, e, task)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != tc.wantVerdict {
		t.Fatalf("verdict: want %s got %s (result=%+v)", tc.wantVerdict, res.Verdict, res)
	}
	if res.FailureReason != tc.wantFailure {
		t.Fatalf("failureReason: want %q got %q", tc.wantFailure, res.FailureReason)
	}
	gotOutcome, present := res.Extra["scanOutcome"].(string)
	if tc.wantOutcome == "" {
		if present {
			t.Fatalf("Extra[scanOutcome]: want absent got %q", gotOutcome)
		}
	} else if gotOutcome != tc.wantOutcome {
		t.Fatalf("Extra[scanOutcome]: want %q got %q", tc.wantOutcome, gotOutcome)
	}
	if _, hasFindings := res.Extra["scanFindings"]; hasFindings != tc.wantFindings {
		t.Fatalf("Extra[scanFindings] present=%t want %t", hasFindings, tc.wantFindings)
	}
	if scanScript != nil && scanScript.calls != tc.wantCalls {
		t.Fatalf("scan calls: want %d got %d", tc.wantCalls, scanScript.calls)
	}
	if scanScript != nil {
		want := task.Spec.ScanGate.Resolve()
		for i, got := range scanScript.gotScans {
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("verify re-gate call %d got config %+v, want the resolved task gate %+v", i, got, want)
			}
		}
	}
	return res
}

// The verify tool's GATE-PASS stands only if the declared scan re-gate
// clears: unwired / could-not-run downgrade to GATE-ERROR (GateError), a
// failed scan downgrades to GATE-FAIL (GateFailed) with the findings
// attached, a passed scan keeps the GATE-PASS, and an undeclared gate is
// byte-identical to today (zero runner calls).
func TestNativeExecutor_VerifyScanReGate(t *testing.T) {
	const finding = "CRITICAL: CVE-2024-0001 in openssl 3.1.0 (fixed in 3.1.4)"
	cases := []verifyScanCase{
		{
			name: "undeclared", wantVerdict: foremanv1alpha1.AgenticTaskVerdictGatePass,
			wantOutcome: "", wantCalls: 0,
		},
		{
			name: "unwired", declared: true,
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictGateError,
			wantFailure: foremanv1alpha1.FailureGateError,
			wantOutcome: "UNVERIFIED", wantCalls: 0,
		},
		{
			name: "could-not-run", declared: true,
			scan:        []scanGateResult{{pass: false, ran: false}},
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictGateError,
			wantFailure: foremanv1alpha1.FailureGateError,
			wantOutcome: "UNVERIFIED", wantCalls: 1,
		},
		{
			name: "scan-fails", declared: true,
			scan:        []scanGateResult{{pass: false, ran: true, feedback: finding}},
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictGateFail,
			wantFailure: foremanv1alpha1.FailureGateFailed,
			wantOutcome: "FAIL", wantFindings: true, wantCalls: 1,
		},
		{
			name: "scan-passes", declared: true,
			scan:        []scanGateResult{{pass: true, ran: true}},
			wantVerdict: foremanv1alpha1.AgenticTaskVerdictGatePass,
			wantOutcome: "PASS", wantCalls: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(t) })
	}
}
