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

package agent

import (
	"context"
	"errors"
	"testing"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func TestApplyVerdictPolicy(t *testing.T) {
	goResult := func() *Result {
		return &Result{Verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			Extra: map[string]any{"workClass": "code-fix"}}
	}
	t.Run("ci-policy footprint downgrades GO", func(t *testing.T) {
		res := applyVerdictPolicy(goResult(),
			map[string]int{".github/workflows/release.yml": 40}, defaultSelfGO)
		if res.Verdict != foremanv1alpha1.AgenticTaskVerdictNoGo {
			t.Fatalf("verdict = %v, want NO-GO", res.Verdict)
		}
		if res.Extra["outcome"] != "NEEDS-VERIFICATION" {
			t.Errorf("outcome = %v", res.Extra["outcome"])
		}
		if res.Extra["actualWorkClass"] != "ci-policy" {
			t.Errorf("actualWorkClass = %v", res.Extra["actualWorkClass"])
		}
		unverified, ok := res.Extra["unverified"].([]map[string]string)
		if !ok || len(unverified) == 0 {
			t.Fatalf("want a non-empty unverified list, got %v (%T)",
				res.Extra["unverified"], res.Extra["unverified"])
		}
		for _, u := range unverified {
			if u["fact"] == "" || u["whyItMatters"] == "" || u["howToVerify"] == "" {
				t.Errorf("unverified entry missing a field: %+v", u)
			}
		}
	})
	t.Run("code-fix GO stands", func(t *testing.T) {
		res := applyVerdictPolicy(goResult(),
			map[string]int{"pkg/foreman/agent/loop.go": 40}, defaultSelfGO)
		if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
			t.Fatalf("verdict = %v, want GO", res.Verdict)
		}
	})
	t.Run("operator opt-in allows ci-policy", func(t *testing.T) {
		res := applyVerdictPolicy(goResult(),
			map[string]int{".github/workflows/release.yml": 40},
			append(defaultSelfGO, "ci-policy"))
		if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
			t.Fatalf("verdict = %v, want GO with opt-in", res.Verdict)
		}
	})
	t.Run("NO-GO passes through untouched", func(t *testing.T) {
		res := &Result{Verdict: foremanv1alpha1.AgenticTaskVerdictNoGo}
		if got := applyVerdictPolicy(res, nil, defaultSelfGO); got.Verdict != foremanv1alpha1.AgenticTaskVerdictNoGo {
			t.Fatal("non-GO must pass through")
		}
	})
	t.Run("actualWorkClass always recorded even when it stands", func(t *testing.T) {
		res := applyVerdictPolicy(goResult(),
			map[string]int{"pkg/foreman/agent/loop.go": 40}, defaultSelfGO)
		if res.Extra["actualWorkClass"] != "code-fix" {
			t.Errorf("actualWorkClass = %v", res.Extra["actualWorkClass"])
		}
	})
	t.Run("declared work class is recorded when the coder set one", func(t *testing.T) {
		res := applyVerdictPolicy(goResult(),
			map[string]int{"pkg/foreman/agent/loop.go": 40}, defaultSelfGO)
		if res.Extra["declaredWorkClass"] != "code-fix" {
			t.Errorf("declaredWorkClass = %v", res.Extra["declaredWorkClass"])
		}
	})
	t.Run("declared-vs-actual mismatch between two self-GO classes flags without downgrading", func(t *testing.T) {
		res := &Result{Verdict: foremanv1alpha1.AgenticTaskVerdictGo,
			Extra: map[string]any{"workClass": "docs"}}
		res = applyVerdictPolicy(res, map[string]int{"pkg/foreman/agent/loop.go": 40}, defaultSelfGO)
		if res.Verdict != foremanv1alpha1.AgenticTaskVerdictGo {
			t.Fatalf("verdict = %v, want GO to stand despite the mismatch", res.Verdict)
		}
		if res.Extra["workClassMismatch"] != true {
			t.Errorf("workClassMismatch = %v, want true", res.Extra["workClassMismatch"])
		}
	})
	t.Run("no declared class means no mismatch flag", func(t *testing.T) {
		res := applyVerdictPolicy(
			&Result{Verdict: foremanv1alpha1.AgenticTaskVerdictGo, Extra: map[string]any{}},
			map[string]int{"pkg/foreman/agent/loop.go": 40}, defaultSelfGO)
		if _, ok := res.Extra["workClassMismatch"]; ok {
			t.Errorf("workClassMismatch should be absent, got %v", res.Extra["workClassMismatch"])
		}
	})
	t.Run("nil Extra on a GO result does not panic", func(t *testing.T) {
		res := applyVerdictPolicy(&Result{Verdict: foremanv1alpha1.AgenticTaskVerdictGo},
			map[string]int{".github/workflows/release.yml": 40}, defaultSelfGO)
		if res.Extra["outcome"] != "NEEDS-VERIFICATION" {
			t.Errorf("outcome = %v", res.Extra["outcome"])
		}
	})
}

// footprintRunner returns a commandRunner that resolves merge-base to
// fakeForkPointSHA and answers the numstat diff with numstatOut, capturing
// the diff invocation's args into *diffArgs (pass nil to skip capture).
func footprintRunner(numstatOut string, diffArgs *[]string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "merge-base" {
			return fakeForkPointSHA, nil
		}
		if name == "git" && len(args) > 0 && args[0] == "diff" {
			if diffArgs != nil {
				*diffArgs = args
			}
			return numstatOut, nil
		}
		return "", nil
	}
}

// assertFootprint fails the test unless got matches want exactly.
func assertFootprint(t *testing.T, got, want map[string]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("got[%q] = %d, want %d", k, got[k], v)
		}
	}
}

func TestDiffFootprint(t *testing.T) {
	t.Run("empty evidenceBaseSHA fails without invoking the numstat diff", func(t *testing.T) {
		run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "diff" {
				t.Fatal("diffFootprint must not run git diff when the anchor cannot be resolved")
			}
			return "", nil
		}
		if _, err := diffFootprint(context.Background(), t.TempDir(), run, ""); err == nil {
			t.Error("expected an error for an empty evidenceBaseSHA")
		}
	})

	t.Run("merge-base failure propagates", func(t *testing.T) {
		stubErr := errors.New("boom")
		run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
			if name == "git" && len(args) > 0 && args[0] == "merge-base" {
				return "", stubErr
			}
			return "", nil
		}
		if _, err := diffFootprint(context.Background(), t.TempDir(), run, fakeEvidenceBaseSHA); err == nil {
			t.Error("expected the merge-base error to propagate")
		}
	})

	t.Run("numstat diff failure propagates", func(t *testing.T) {
		stubErr := errors.New("diff boom")
		run := func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
			if name == "git" && len(args) > 0 && args[0] == "merge-base" {
				return fakeForkPointSHA, nil
			}
			if name == "git" && len(args) > 0 && args[0] == "diff" {
				return "", stubErr
			}
			return "", nil
		}
		if _, err := diffFootprint(context.Background(), t.TempDir(), run, fakeEvidenceBaseSHA); err == nil {
			t.Error("expected the numstat diff error to propagate")
		}
	})
}

func TestDiffFootprint_ParsesNumstat(t *testing.T) {
	var diffArgs []string
	run := footprintRunner(
		"10\t5\t.github/workflows/release.yml\n"+
			"3\t0\tpkg/foreman/agent/loop.go\n"+
			"-\t-\tassets/logo.png\n", &diffArgs)
	got, err := diffFootprint(context.Background(), t.TempDir(), run, fakeEvidenceBaseSHA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFootprint(t, got, map[string]int{
		".github/workflows/release.yml": 15,
		"pkg/foreman/agent/loop.go":     3,
	})
	if len(diffArgs) < 2 || diffArgs[0] != "diff" || diffArgs[1] != "--numstat" {
		t.Errorf("diff args = %v, want to start with [diff --numstat]", diffArgs)
	}
	if diffArgs[len(diffArgs)-1] != fakeForkPointSHA {
		t.Errorf("diff args = %v, want anchor %q last", diffArgs, fakeForkPointSHA)
	}
}

func TestDiffFootprint_RenameLinesRecordUnderPostRenamePath(t *testing.T) {
	// The three rename shapes git numstat prints: shared prefix (braced),
	// a path segment removed entirely (braced with an empty new side),
	// and nothing shared (plain "old => new").
	run := footprintRunner(
		"4\t2\tpkg/foreman/agent/{old_gate.go => new_gate.go}\n"+
			"1\t1\t{cmd => }/main.go\n"+
			"7\t0\tdocs/design.md => .github/workflows/release.yml\n", nil)
	got, err := diffFootprint(context.Background(), t.TempDir(), run, fakeEvidenceBaseSHA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertFootprint(t, got, map[string]int{
		"pkg/foreman/agent/new_gate.go": 6,
		"main.go":                       2,
		".github/workflows/release.yml": 7,
	})
}
