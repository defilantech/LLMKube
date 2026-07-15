package agent

import (
	"context"
	"testing"
)

type fakeEnvtestJobRunner struct {
	pass, ran bool
	feedback  string
	called    bool

	// gotTaskNamespace / gotTaskName capture the task identity the caller
	// threaded through, so tests can assert it reaches the runner (#893).
	gotTaskNamespace string
	gotTaskName      string
}

func (f *fakeEnvtestJobRunner) Run(_ context.Context, taskNamespace, taskName, _, _, _ string) (bool, bool, string) {
	f.called = true
	f.gotTaskNamespace = taskNamespace
	f.gotTaskName = taskName
	return f.pass, f.ran, f.feedback
}

func TestEvaluatePostPushEnvtest(t *testing.T) {
	t.Run("not touched: runner not called, verdict OK", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{}
		v, fb := evaluatePostPushEnvtest(context.Background(), false, r, "ns", "task", "repo", "br", "url")
		if v != envtestGateOK || fb != "" || r.called {
			t.Fatalf("got verdict=%v fb=%q called=%v", v, fb, r.called)
		}
	})
	t.Run("nil runner: verdict OK", func(t *testing.T) {
		v, fb := evaluatePostPushEnvtest(context.Background(), true, nil, "ns", "task", "repo", "br", "url")
		if v != envtestGateOK || fb != "" {
			t.Fatalf("got verdict=%v fb=%q", v, fb)
		}
	})
	t.Run("touched + pass: verdict OK", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{pass: true, ran: true}
		v, _ := evaluatePostPushEnvtest(context.Background(), true, r, "ns", "task", "repo", "br", "url")
		if v != envtestGateOK || !r.called {
			t.Fatalf("got verdict=%v called=%v", v, r.called)
		}
	})
	t.Run("touched + ran + fail: verdict Failed with feedback", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{pass: false, ran: true, feedback: "envtest broke"}
		v, fb := evaluatePostPushEnvtest(context.Background(), true, r, "ns", "task", "repo", "br", "url")
		if v != envtestGateFailed || fb != "envtest broke" {
			t.Fatalf("got verdict=%v fb=%q", v, fb)
		}
	})
	t.Run("touched + could-not-run: verdict Unverified (caller decides by attempt)", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{pass: false, ran: false, feedback: "infra"}
		v, _ := evaluatePostPushEnvtest(context.Background(), true, r, "ns", "task", "repo", "br", "url")
		if v != envtestGateUnverified {
			t.Fatalf("could-not-run should be Unverified; got verdict=%v", v)
		}
	})
	t.Run("task identity is threaded to the runner (#893)", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{pass: true, ran: true}
		evaluatePostPushEnvtest(context.Background(), true, r,
			"foreman-system", "fix-issue-893", "repo", "br", "url")
		if r.gotTaskNamespace != "foreman-system" || r.gotTaskName != "fix-issue-893" {
			t.Fatalf("runner got task identity (%q,%q), want (foreman-system,fix-issue-893)",
				r.gotTaskNamespace, r.gotTaskName)
		}
	})
}
