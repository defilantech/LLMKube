package agent

import (
	"context"
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

type fakeScanJobRunner struct {
	pass, ran bool
	feedback  string
	called    bool

	// got* capture the exact identity the caller threaded through, so
	// tests can assert each value reaches the runner (#893 task stamping,
	// #1731 canonical upstream URL).
	gotTaskNamespace string
	gotTaskName      string
	gotRepository    string
	gotBranch        string
	gotCloneURL      string
	gotUpstreamURL   string
}

func (f *fakeScanJobRunner) Run(
	_ context.Context,
	taskNamespace, taskName, repository, branch, cloneURL, upstreamURL string,
) (bool, bool, string) {
	f.called = true
	f.gotTaskNamespace = taskNamespace
	f.gotTaskName = taskName
	f.gotRepository = repository
	f.gotBranch = branch
	f.gotCloneURL = cloneURL
	f.gotUpstreamURL = upstreamURL
	return f.pass, f.ran, f.feedback
}

func TestEvaluatePostPushScan(t *testing.T) {
	t.Run("not declared: runner not called, verdict OK", func(t *testing.T) {
		r := &fakeScanJobRunner{}
		v, fb := evaluatePostPushScan(context.Background(), false, r, "ns", "task", "repo", "br", "url", "up")
		if v != scanGateOK || fb != "" || r.called {
			t.Fatalf("got verdict=%v fb=%q called=%v", v, fb, r.called)
		}
	})
	t.Run("declared + nil runner: verdict OK", func(t *testing.T) {
		v, fb := evaluatePostPushScan(context.Background(), true, nil, "ns", "task", "repo", "br", "url", "up")
		if v != scanGateOK || fb != "" {
			t.Fatalf("got verdict=%v fb=%q", v, fb)
		}
	})
	t.Run("declared + pass: verdict OK", func(t *testing.T) {
		r := &fakeScanJobRunner{pass: true, ran: true}
		v, _ := evaluatePostPushScan(context.Background(), true, r, "ns", "task", "repo", "br", "url", "up")
		if v != scanGateOK || !r.called {
			t.Fatalf("got verdict=%v called=%v", v, r.called)
		}
	})
	t.Run("declared + ran + fail: verdict Failed with feedback verbatim", func(t *testing.T) {
		r := &fakeScanJobRunner{pass: false, ran: true, feedback: "trivy: 2 CRITICAL findings"}
		v, fb := evaluatePostPushScan(context.Background(), true, r, "ns", "task", "repo", "br", "url", "up")
		if v != scanGateFailed || fb != "trivy: 2 CRITICAL findings" {
			t.Fatalf("got verdict=%v fb=%q", v, fb)
		}
	})
	t.Run("declared + could-not-run: verdict Unverified (caller decides by attempt)", func(t *testing.T) {
		r := &fakeScanJobRunner{pass: false, ran: false, feedback: "infra"}
		v, _ := evaluatePostPushScan(context.Background(), true, r, "ns", "task", "repo", "br", "url", "up")
		if v != scanGateUnverified {
			t.Fatalf("could-not-run should be Unverified; got verdict=%v", v)
		}
	})
	t.Run("task identity is threaded to the runner (#893/#1731)", func(t *testing.T) {
		r := &fakeScanJobRunner{pass: true, ran: true}
		evaluatePostPushScan(context.Background(), true, r,
			"foreman-system", "fix-issue-893", "defilantech/llmkube", "feat/x",
			"https://github.com/fork/llmkube.git", "https://github.com/defilantech/llmkube.git")
		if r.gotTaskNamespace != "foreman-system" || r.gotTaskName != "fix-issue-893" {
			t.Fatalf("runner got task identity (%q,%q), want (foreman-system,fix-issue-893)",
				r.gotTaskNamespace, r.gotTaskName)
		}
		if r.gotRepository != "defilantech/llmkube" || r.gotBranch != "feat/x" ||
			r.gotCloneURL != "https://github.com/fork/llmkube.git" ||
			r.gotUpstreamURL != "https://github.com/defilantech/llmkube.git" {
			t.Fatalf("runner got (repo=%q branch=%q clone=%q up=%q), want the exact values it was called with",
				r.gotRepository, r.gotBranch, r.gotCloneURL, r.gotUpstreamURL)
		}
	})
}

func TestEffectiveMaxScanIterations(t *testing.T) {
	cases := []struct {
		name string
		in   *int32
		want int
	}{
		{"nil defaults to 1", nil, 1},
		{"explicit 0 opts out", i32p(0), 0},
		{"explicit N honored", i32p(3), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &foremanv1alpha1.Agent{}
			a.Spec.MaxScanIterations = tc.in
			if got := effectiveMaxScanIterations(a); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	t.Run("nil agent defaults to 1", func(t *testing.T) {
		if got := effectiveMaxScanIterations(nil); got != 1 {
			t.Fatalf("got %d want %d", got, 1)
		}
	})
}

func TestScanFeedbackPrompt(t *testing.T) {
	got := scanFeedbackPrompt("CRITICAL: CVE-2024-0001 in openssl 3.1.0 (fixed in 3.1.4)")
	for _, want := range []string{
		"scan gate failed",        // the gate that fired
		"SCAN-FAIL",               // the verdict the coder sees
		"already contains",        // points the coder at its prior work
		"cannot build or scan",    // the no-image-build directive
		"Do NOT attempt",          // no docker/buildah/trivy in the workspace
		"trivy",                   // named in the do-not-attempt list
		"node_modules",            // packaging-level fix examples
		"Scan findings:",          // the findings header
		"CRITICAL: CVE-2024-0001", // the finding table is embedded
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q; got:\n%s", want, got)
		}
	}
}

func TestScanFeedbackPromptTruncatesLargeFeedback(t *testing.T) {
	// One byte over the 32 KiB gate-output cap: the prompt must carry the
	// truncated tail, not the whole finding table.
	feedback := strings.Repeat("x", maxGateOutputBytes+1)
	got := scanFeedbackPrompt(feedback)
	if strings.Contains(got, feedback) {
		t.Fatalf("prompt carried the full %d-byte finding table; expected truncation", len(feedback))
	}
	if !strings.Contains(got, "...(truncated)...") {
		t.Fatalf("prompt missing the truncation marker; got:\n%s", got)
	}
	if !strings.HasSuffix(strings.TrimSuffix(got, "\n"), strings.Repeat("x", maxGateOutputBytes)) {
		t.Fatalf("truncated prompt did not keep the last %d bytes of the findings", maxGateOutputBytes)
	}
}
