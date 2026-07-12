package grounding

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fixture697 mirrors docs/proposals/697.md as it existed at the base commit:
// a benchmark sentence on line 3 that every case in this file cites,
// misattributes, or alters. Split on "\n" it is 4 lines (the trailing
// newline yields a trailing empty element), which doubles as the
// "4-line file" fixture for the out-of-range regression case.
const fixture697 = "# AMD runtime proposal\n\n" +
	"Validated: Llama-3.2-3B Q4_K_M, 29/29 layers offloaded, ~87 tok/s decode on gfx1151.\n"

// fixtureLower is the same benchmark sentence with a lowercase model
// spelling, standing in for a source file that spells the subject
// differently than the claim under review.
const fixtureLower = "Validated: llama-3.2-3b Q4_K_M, 29/29 layers offloaded, ~87 tok/s decode on gfx1151.\n"

// fakeGitShow is a hermetic CommandRunner: it answers `git show
// base:docs/proposals/697.md` and `git show base:docs/lower.md` with fixture
// content, and errors for every other invocation, exactly as `git show`
// errors for a path absent at the given ref (which is also what it does for
// a file the coder added or edited in this same change: it never existed at
// base, so it cannot self-certify its own claims). It records every call it
// receives so tests can assert git was, or was not, invoked.
func fakeGitShow(calls *[]string) CommandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		if name != "git" || len(args) != 2 || args[0] != "show" {
			return "", errors.New("unsupported command")
		}
		switch args[1] {
		case "base:docs/proposals/697.md":
			return fixture697, nil
		case "base:docs/lower.md":
			return fixtureLower, nil
		default:
			return "", errors.New("fatal: path '" + args[1] + "' does not exist in 'base'")
		}
	}
}

func TestMatchClaims(t *testing.T) {
	const base = "base"
	llamaClaim := Claim{
		File: "examples/README.md", Line: 10, Number: "87", Unit: "tok/s",
		Subjects: []string{"Llama-3.2-3B"},
		Text:     "| Llama-3.2-3B (Q4_K_M) | ~87 tok/s |",
	}
	cases := []struct {
		name       string
		claim      Claim
		evidence   []Evidence
		wantFail   bool
		wantNoCall bool // assert the fake runner recorded zero calls
	}{
		{"cited and correct passes",
			llamaClaim,
			[]Evidence{{Claim: "~87 tok/s Llama-3.2-3B", Source: "docs/proposals/697.md:3"}},
			false, false},
		{"misattributed subject fails (699 mode 1)",
			Claim{File: "examples/README.md", Line: 154, Number: "87", Unit: "tok/s",
				Subjects: []string{"Qwen3", "30B-A3B"},
				Text:     "| Qwen3 30B-A3B (Q8_0) | ~87 tok/s |"},
			[]Evidence{{Claim: "~87 tok/s Qwen3", Source: "docs/proposals/697.md:3"}},
			true, false},
		{"altered number fails (699 mode 2)",
			Claim{File: "examples/README.md", Line: 155, Number: "95", Unit: "tok/s",
				Subjects: []string{"Llama-3.2-3B"},
				Text:     "| Llama 3.2 3B (Q4_K_M) | ~95 tok/s |"},
			[]Evidence{{Claim: "~95 tok/s Llama", Source: "docs/proposals/697.md:3"}},
			true, false},
		{"no evidence at all fails (699 mode 3)",
			Claim{File: "examples/README.md", Line: 156, Number: "45", Unit: "tok/s",
				Subjects: []string{"Mixtral"},
				Text:     "| Mixtral 8x7B | ~45 tok/s |"},
			nil, true, false},

		// --- regression cases (adversarial review round 1) ---

		{"C1: negative cited line does not panic, fails",
			llamaClaim,
			[]Evidence{{Claim: "~87 tok/s Llama-3.2-3B", Source: "docs/proposals/697.md:-3"}},
			true, false},
		{"C1: cited line past end of file does not panic, fails",
			llamaClaim,
			[]Evidence{{Claim: "~87 tok/s Llama-3.2-3B", Source: "docs/proposals/697.md:50"}},
			true, false},
		{"C2: self-authored file (absent at base) fails",
			llamaClaim,
			[]Evidence{{Claim: "~87 tok/s Llama-3.2-3B", Source: "docs/proposals/coder-added.md:1"}},
			true, false},
		{"I3: parent-traversal path rejected without invoking git",
			llamaClaim,
			[]Evidence{{Claim: "~87 tok/s Llama-3.2-3B", Source: "../secret:1"}},
			true, true},
		{"I3: absolute path rejected without invoking git",
			llamaClaim,
			[]Evidence{{Claim: "~87 tok/s Llama-3.2-3B", Source: "/etc/passwd:1"}},
			true, true},
		{"I4: lowercase window subject matches mixed-case claim subject",
			llamaClaim,
			[]Evidence{{Claim: "~87 tok/s llama-3.2-3b", Source: "docs/lower.md:1"}},
			false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var calls []string
			run := fakeGitShow(&calls)
			findings := MatchClaims(context.Background(), "/ws", run, base, []Claim{c.claim}, c.evidence)
			if got := len(findings) > 0; got != c.wantFail {
				t.Errorf("fail=%v, want %v (findings: %+v)", got, c.wantFail, findings)
			}
			if c.wantNoCall && len(calls) != 0 {
				t.Errorf("expected git not to be invoked for a rejected path, got calls: %v", calls)
			}
		})
	}
}

// TestMatchClaims_EmptyBaseRefusesToProve pins the Task 3 review hardening:
// an empty base must never reach `git show`, because `git show :path` (no
// ref before the colon) reads the staging index rather than a commit. A
// coder that `write_file`s a fabricated benchmark and stages it could
// otherwise self-certify its own claim if a caller ever passed base="".
func TestMatchClaims_EmptyBaseRefusesToProve(t *testing.T) {
	llamaClaim := Claim{
		File: "examples/README.md", Line: 10, Number: "87", Unit: "tok/s",
		Subjects: []string{"Llama-3.2-3B"},
		Text:     "| Llama-3.2-3B (Q4_K_M) | ~87 tok/s |",
	}
	var calls []string
	run := fakeGitShow(&calls)
	// Otherwise-valid evidence: a real path:line pair that fakeGitShow would
	// happily prove against a non-empty base.
	evidence := []Evidence{{Claim: "~87 tok/s Llama-3.2-3B", Source: "docs/proposals/697.md:3"}}
	findings := MatchClaims(context.Background(), "/ws", run, "", []Claim{llamaClaim}, evidence)
	if len(findings) == 0 {
		t.Fatal("empty base must not let evidence prove the claim; want a finding")
	}
	if len(calls) != 0 {
		t.Errorf("empty base must never invoke git; got calls: %v", calls)
	}
}

func TestParseEvidence(t *testing.T) {
	extra := map[string]any{"evidence": []any{
		map[string]any{"claim": "~87 tok/s", "source": "docs/x.md:3"},
	}}
	ev := ParseEvidence(extra)
	if len(ev) != 1 || ev[0].Source != "docs/x.md:3" {
		t.Fatalf("ParseEvidence = %+v", ev)
	}
	if got := ParseEvidence(map[string]any{}); len(got) != 0 {
		t.Fatalf("empty extra should parse to no evidence, got %+v", got)
	}
}
