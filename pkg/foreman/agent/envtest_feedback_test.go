package agent

import (
	"strings"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func i32p(v int32) *int32 { return &v }

func TestEffectiveMaxEnvtestIterations(t *testing.T) {
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
			a.Spec.MaxEnvtestIterations = tc.in
			if got := effectiveMaxEnvtestIterations(a); got != tc.want {
				t.Fatalf("got %d want %d", got, tc.want)
			}
		})
	}
	t.Run("nil agent defaults to 1", func(t *testing.T) {
		if got := effectiveMaxEnvtestIterations(nil); got != 1 {
			t.Fatalf("got %d want 1", got)
		}
	})
}

func TestEnvtestFeedbackPrompt(t *testing.T) {
	got := envtestFeedbackPrompt("controller_test.go:42 Expected true got false")
	for _, want := range []string{
		"envtest gate failed",
		"already contains",      // points the coder at its prior work
		"go build ./...",        // the only self-check allowed here
		"controller_test.go:42", // the gate output is embedded
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q; got:\n%s", want, got)
		}
	}
}

func TestRetryCfgAppendsFeedbackAndPreservesFields(t *testing.T) {
	base := LoopConfig{
		SystemPrompt:     "sys",
		UserPrompt:       "ORIGINAL ISSUE CONTEXT",
		MaxTurns:         50,
		MaxVerifyRetries: 3,
	}
	out := retryCfg(base, "GATE OUTPUT HERE")

	if !strings.HasPrefix(out.UserPrompt, "ORIGINAL ISSUE CONTEXT") {
		t.Fatalf("retry prompt dropped the original issue context: %q", out.UserPrompt)
	}
	if !strings.Contains(out.UserPrompt, "GATE OUTPUT HERE") {
		t.Fatalf("retry prompt missing the gate feedback")
	}
	if out.SystemPrompt != base.SystemPrompt || out.MaxTurns != base.MaxTurns ||
		out.MaxVerifyRetries != base.MaxVerifyRetries {
		t.Fatalf("retryCfg mutated a non-prompt field: %+v", out)
	}
	if base.UserPrompt != "ORIGINAL ISSUE CONTEXT" {
		t.Fatalf("retryCfg mutated the base config in place")
	}
}
