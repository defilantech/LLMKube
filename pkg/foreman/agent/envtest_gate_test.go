package agent

import (
	"context"
	"testing"
)

type fakeEnvtestJobRunner struct {
	pass, ran bool
	feedback  string
	called    bool
}

func (f *fakeEnvtestJobRunner) Run(_ context.Context, _, _, _ string) (bool, bool, string) {
	f.called = true
	return f.pass, f.ran, f.feedback
}

func TestEvaluatePostPushEnvtest(t *testing.T) {
	t.Run("not touched: runner not called, no downgrade", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{}
		failed, fb := evaluatePostPushEnvtest(context.Background(), false, r, "repo", "br", "url")
		if failed || fb != "" || r.called {
			t.Fatalf("got failed=%v fb=%q called=%v", failed, fb, r.called)
		}
	})
	t.Run("nil runner: no downgrade", func(t *testing.T) {
		failed, fb := evaluatePostPushEnvtest(context.Background(), true, nil, "repo", "br", "url")
		if failed || fb != "" {
			t.Fatalf("got failed=%v fb=%q", failed, fb)
		}
	})
	t.Run("touched + pass: no downgrade", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{pass: true, ran: true}
		failed, _ := evaluatePostPushEnvtest(context.Background(), true, r, "repo", "br", "url")
		if failed || !r.called {
			t.Fatalf("got failed=%v called=%v", failed, r.called)
		}
	})
	t.Run("touched + ran + fail: downgrade with feedback", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{pass: false, ran: true, feedback: "envtest broke"}
		failed, fb := evaluatePostPushEnvtest(context.Background(), true, r, "repo", "br", "url")
		if !failed || fb != "envtest broke" {
			t.Fatalf("got failed=%v fb=%q", failed, fb)
		}
	})
	t.Run("touched + could-not-run: no downgrade (GO stands)", func(t *testing.T) {
		r := &fakeEnvtestJobRunner{pass: false, ran: false, feedback: "infra"}
		failed, _ := evaluatePostPushEnvtest(context.Background(), true, r, "repo", "br", "url")
		if failed {
			t.Fatalf("could-not-run should not downgrade; got failed=%v", failed)
		}
	})
}
