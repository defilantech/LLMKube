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
	"strings"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubpr"
)

// prEnsureCall records one EnsurePR invocation on the fake.
type prEnsureCall struct {
	owner, repo, head, base, title, body string
}

// prSubjectCall records one HeadCommitSubject invocation on the fake.
type prSubjectCall struct {
	owner, repo, ref string
}

// fakePREnsurer is a recording githubpr.Ensurer for executor wiring
// tests: it captures the arguments the executor derives (base owner,
// qualified head, fork owner for the subject lookup) without any HTTP.
type fakePREnsurer struct {
	ensures  []prEnsureCall
	subjects []prSubjectCall
	subject  string
	url      string
	err      error
}

func (f *fakePREnsurer) EnsurePR(
	_ context.Context, owner, repo, head, base, title, body, _ string,
) (*githubpr.Result, error) {
	f.ensures = append(f.ensures, prEnsureCall{owner, repo, head, base, title, body})
	if f.err != nil {
		return nil, f.err
	}
	return &githubpr.Result{URL: f.url, Created: true}, nil
}

func (f *fakePREnsurer) HeadCommitSubject(_ context.Context, owner, repo, ref, _ string) string {
	f.subjects = append(f.subjects, prSubjectCall{owner, repo, ref})
	return f.subject
}

// reviewTaskForPR builds the minimal review-kind AgenticTask that
// maybeOpenPullRequest inspects.
func reviewTaskForPR(kind foremanv1alpha1.AgenticTaskKind, openPR bool) *foremanv1alpha1.AgenticTask {
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{
			Name: "review-pr", Namespace: "default",
			Labels: map[string]string{"foreman.llmkube.dev/workload": "wl-x"},
		},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind: kind,
			Payload: foremanv1alpha1.AgenticTaskPayload{
				Repo:            "defilantech/LLMKube",
				Issue:           7,
				Branch:          "foreman/wl-x/issue-7",
				OpenPullRequest: openPR,
			},
		},
	}
}

// TestMaybeOpenPullRequest_Gating pins the wiring the #937 feature hangs
// on: the ensurer runs only for verdict GO + kind review +
// payload.openPullRequest, and never otherwise.
func TestMaybeOpenPullRequest_Gating(t *testing.T) {
	cases := []struct {
		name       string
		verdict    foremanv1alpha1.AgenticTaskVerdict
		kind       foremanv1alpha1.AgenticTaskKind
		openPR     bool
		wantCalled bool
	}{
		{"go review flag on opens", foremanv1alpha1.AgenticTaskVerdictGo,
			foremanv1alpha1.AgenticTaskKindReview, true, true},
		{"no-go review never opens", foremanv1alpha1.AgenticTaskVerdictNoGo,
			foremanv1alpha1.AgenticTaskKindReview, true, false},
		{"go review flag off never opens", foremanv1alpha1.AgenticTaskVerdictGo,
			foremanv1alpha1.AgenticTaskKindReview, false, false},
		{"go non-review never opens", foremanv1alpha1.AgenticTaskVerdictGo,
			foremanv1alpha1.AgenticTaskKindIssueFix, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakePREnsurer{url: "https://github.com/defilantech/LLMKube/pull/1"}
			e := &NativeAgentLoopExecutor{PREnsurer: fe}
			task := reviewTaskForPR(tc.kind, tc.openPR)
			r := &Result{Extra: map[string]any{}}

			e.maybeOpenPullRequest(context.Background(), logr.Discard(), task, nil, tc.verdict, r, "", "", nil)

			if called := len(fe.ensures) > 0; called != tc.wantCalled {
				t.Fatalf("EnsurePR called=%v, want %v (calls=%+v)", called, tc.wantCalled, fe.ensures)
			}
			if _, hasURL := r.Extra["pullRequestURL"]; hasURL != tc.wantCalled {
				t.Errorf("pullRequestURL present=%v, want %v; extra=%+v", hasURL, tc.wantCalled, r.Extra)
			}
		})
	}
}

// TestMaybeOpenPullRequest_NilEnsurerIsDisabled: nil PREnsurer disables
// PR opening entirely (cmd wiring may leave it unset) without panicking.
func TestMaybeOpenPullRequest_NilEnsurerIsDisabled(t *testing.T) {
	e := &NativeAgentLoopExecutor{}
	task := reviewTaskForPR(foremanv1alpha1.AgenticTaskKindReview, true)
	r := &Result{Extra: map[string]any{}}
	e.maybeOpenPullRequest(context.Background(), logr.Discard(), task, nil,
		foremanv1alpha1.AgenticTaskVerdictGo, r, "", "", nil)
	if len(r.Extra) != 0 {
		t.Fatalf("nil ensurer must be a no-op; extra=%+v", r.Extra)
	}
}

// TestMaybeOpenPullRequest_BodyCarriesReviewSummary: the PR body leads with
// the reviewer's summary of the change (it read the diff to reach GO), then
// the issue link; an empty summary falls back to just the link.
func TestMaybeOpenPullRequest_BodyCarriesReviewSummary(t *testing.T) {
	fe := &fakePREnsurer{subject: "fix: the thing", url: "https://example/pr/1"}
	e := &NativeAgentLoopExecutor{PREnsurer: fe}
	task := reviewTaskForPR(foremanv1alpha1.AgenticTaskKindReview, true)
	r := &Result{
		Summary: "Adds provider details to the SSO error path so failures are diagnosable.",
		Extra:   map[string]any{},
	}

	e.maybeOpenPullRequest(context.Background(), logr.Discard(), task, nil,
		foremanv1alpha1.AgenticTaskVerdictGo, r, "", "", nil)

	if len(fe.ensures) != 1 {
		t.Fatalf("want 1 EnsurePR call, got %+v", fe.ensures)
	}
	body := fe.ensures[0].body
	if !strings.Contains(body, r.Summary) {
		t.Errorf("body must lead with the reviewer summary; got %q", body)
	}
	if !strings.Contains(body, "Fixes #") {
		t.Errorf("body must still link the issue; got %q", body)
	}
}

// TestMaybeOpenPullRequest_ForkRemoteQualifiesHead is the #956 review
// fix: the coder pushes to the fork named by --git-remote-url while
// payload.repo names the upstream, so the PR must be cross-fork — head
// "Defilan:branch" against base repo defilantech/LLMKube, and the title
// commit read from the fork where the ref exists.
func TestMaybeOpenPullRequest_ForkRemoteQualifiesHead(t *testing.T) {
	fe := &fakePREnsurer{
		subject: "fix: the thing",
		url:     "https://github.com/defilantech/LLMKube/pull/2",
	}
	e := &NativeAgentLoopExecutor{
		PREnsurer:    fe,
		GitRemoteURL: "https://github.com/Defilan/LLMKube.git",
	}
	task := reviewTaskForPR(foremanv1alpha1.AgenticTaskKindReview, true)
	r := &Result{Extra: map[string]any{}}

	e.maybeOpenPullRequest(context.Background(), logr.Discard(), task, nil,
		foremanv1alpha1.AgenticTaskVerdictGo, r, "", "", nil)

	if len(fe.ensures) != 1 {
		t.Fatalf("want 1 EnsurePR call, got %+v", fe.ensures)
	}
	got := fe.ensures[0]
	got.body = "" // body content is covered by TestMaybeOpenPullRequest_BodyCarriesReviewSummary
	want := prEnsureCall{
		owner: "defilantech", repo: "LLMKube",
		head: "Defilan:foreman/wl-x/issue-7", base: "main", title: "fix: the thing",
	}
	if got != want {
		t.Errorf("EnsurePR args:\n got %+v\nwant %+v", got, want)
	}
	if len(fe.subjects) != 1 {
		t.Fatalf("want 1 HeadCommitSubject call, got %+v", fe.subjects)
	}
	if s := fe.subjects[0]; s.owner != "Defilan" || s.repo != "LLMKube" || s.ref != "foreman/wl-x/issue-7" {
		t.Errorf("HeadCommitSubject must read from the fork; got %+v", s)
	}
	if r.Extra["pullRequestURL"] != fe.url {
		t.Errorf("pullRequestURL: got %v", r.Extra["pullRequestURL"])
	}
}

// TestMaybeOpenPullRequest_SameRepoRemoteKeepsBareHead: a remote whose
// owner matches payload.repo (case-insensitively, GitHub owners are
// case-insensitive) is not a fork — same-repo behavior stands.
func TestMaybeOpenPullRequest_SameRepoRemoteKeepsBareHead(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/defilantech/LLMKube.git",
		"https://github.com/DefilanTech/LLMKube", // owner case differs only
		"",                                       // no static remote configured (#915 multi-repo mode)
		"/tmp/seed/bare.git",                     // local path (tests, air-gapped mirrors)
		"file:///srv/bare.git",                   // not owner/repo-shaped
	} {
		fe := &fakePREnsurer{subject: "fix: same repo", url: "https://github.com/defilantech/LLMKube/pull/3"}
		e := &NativeAgentLoopExecutor{PREnsurer: fe, GitRemoteURL: remote}
		task := reviewTaskForPR(foremanv1alpha1.AgenticTaskKindReview, true)
		r := &Result{Extra: map[string]any{}}

		e.maybeOpenPullRequest(context.Background(), logr.Discard(), task, nil,
			foremanv1alpha1.AgenticTaskVerdictGo, r, "", "", nil)

		if len(fe.ensures) != 1 {
			t.Fatalf("remote %q: want 1 EnsurePR call, got %+v", remote, fe.ensures)
		}
		if got := fe.ensures[0].head; got != "foreman/wl-x/issue-7" {
			t.Errorf("remote %q: head got %q, want bare branch", remote, got)
		}
		if s := fe.subjects[0]; s.owner != "defilantech" || s.repo != "LLMKube" {
			t.Errorf("remote %q: subject read from %s/%s, want base repo", remote, s.owner, s.repo)
		}
	}
}

// TestGitRemoteOwnerRepo pins the tolerant URL forms the fork-owner
// derivation must understand, and the non-GitHub-shaped remotes it must
// decline to parse.
func TestGitRemoteOwnerRepo(t *testing.T) {
	cases := []struct {
		url         string
		owner, name string
	}{
		{"https://github.com/Defilan/LLMKube.git", "Defilan", "LLMKube"},
		{"https://github.com/Defilan/LLMKube", "Defilan", "LLMKube"},
		{"https://github.com/Defilan/LLMKube/", "Defilan", "LLMKube"},
		{"http://ghes.corp/Defilan/LLMKube.git", "Defilan", "LLMKube"},
		{"https://x-access-token:tok@github.com/Defilan/LLMKube.git", "Defilan", "LLMKube"},
		{"git@github.com:Defilan/LLMKube.git", "Defilan", "LLMKube"},
		{"git@github.com:Defilan/LLMKube", "Defilan", "LLMKube"},
		{"ssh://git@github.com/Defilan/LLMKube.git", "Defilan", "LLMKube"},
		{"", "", ""},
		{"/tmp/seed/bare.git", "", ""},
		{"file:///srv/git/bare.git", "", ""},
		{"https://github.com/onlyowner", "", ""},
		{"https://github.com/a/b/c", "a/b", "c"},
	}
	for _, tc := range cases {
		owner, name := gitRemoteOwnerRepo(tc.url)
		if owner != tc.owner || name != tc.name {
			t.Errorf("gitRemoteOwnerRepo(%q) = %q, %q; want %q, %q",
				tc.url, owner, name, tc.owner, tc.name)
		}
	}
}

// TestMaybeOpenPullRequest_BodyFlagsUngroundedClaim is the #1411 wiring
// test: the summary that becomes the PR description is cross-checked
// against the branch diff before it is rendered, the note lands in the
// body a human reads, and the model's original is archived under
// extra.summaryClaimed the way issueAskClaimed is (#644).
func TestMaybeOpenPullRequest_BodyFlagsUngroundedClaim(t *testing.T) {
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string,
		name string, args ...string) (string, error) {
		if name != "git" || args[0] != "diff" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return loggingDiff, nil
	}

	fe := &fakePREnsurer{subject: "feat: structured logging", url: "https://example/pr/9"}
	e := &NativeAgentLoopExecutor{PREnsurer: fe}
	task := reviewTaskForPR(foremanv1alpha1.AgenticTaskKindReview, true)
	summary := "Replaces bare `print()` calls with `logger.info()` and adds a `/health` endpoint."
	r := &Result{Summary: summary, Extra: map[string]any{}}

	e.maybeOpenPullRequest(context.Background(), logr.Discard(), task, nil,
		foremanv1alpha1.AgenticTaskVerdictGo, r, t.TempDir(), "main", []string{"bridge/app.py"})

	if len(fe.ensures) != 1 {
		t.Fatalf("want 1 EnsurePR call, got %+v", fe.ensures)
	}
	body := fe.ensures[0].body
	if !strings.Contains(body, summary) {
		t.Errorf("the reviewer's prose must survive verbatim; got %q", body)
	}
	if !strings.Contains(body, "Unverified claims") || !strings.Contains(body, "`/health`") {
		t.Errorf("body must flag the ungrounded claim; got %q", body)
	}
	if r.Extra["summaryClaimed"] != summary {
		t.Errorf("summaryClaimed must archive the model's original; got %v", r.Extra["summaryClaimed"])
	}
	claims, _ := r.Extra["summaryUnverifiedClaims"].([]string)
	if len(claims) != 1 || claims[0] != "/health" {
		t.Errorf("summaryUnverifiedClaims = %v, want [/health]", r.Extra["summaryUnverifiedClaims"])
	}
	if r.Summary != summary {
		t.Errorf("r.Summary must stay the model's own; got %q", r.Summary)
	}
}

// TestMaybeOpenPullRequest_GroundedSummaryUntouched: the common case. A
// summary whose claims the diff supports renders byte-for-byte as before,
// with no note and no summaryClaimed archive.
func TestMaybeOpenPullRequest_GroundedSummaryUntouched(t *testing.T) {
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string,
		_ string, _ ...string) (string, error) {
		return loggingDiff, nil
	}

	fe := &fakePREnsurer{subject: "feat: structured logging", url: "https://example/pr/10"}
	e := &NativeAgentLoopExecutor{PREnsurer: fe}
	task := reviewTaskForPR(foremanv1alpha1.AgenticTaskKindReview, true)
	summary := "Replaces bare `print()` calls in `bridge/app.py` with `logger.info()`."
	r := &Result{Summary: summary, Extra: map[string]any{}}

	e.maybeOpenPullRequest(context.Background(), logr.Discard(), task, nil,
		foremanv1alpha1.AgenticTaskVerdictGo, r, t.TempDir(), "main", []string{"bridge/app.py"})

	body := fe.ensures[0].body
	if strings.Contains(body, "Unverified claims") {
		t.Errorf("a grounded summary must render clean; got %q", body)
	}
	if _, archived := r.Extra["summaryClaimed"]; archived {
		t.Errorf("nothing to archive when the body equals the summary; extra=%+v", r.Extra)
	}
}

// TestMaybeOpenPullRequest_NoDiffSkipsGrounding: an unresolved review base
// (the coder-retry path) or a git failure must leave the summary alone.
// Warning on missing ground truth would put the note on honest PRs.
func TestMaybeOpenPullRequest_NoDiffSkipsGrounding(t *testing.T) {
	orig := execCommandRunner
	t.Cleanup(func() { execCommandRunner = orig })
	execCommandRunner = func(_ context.Context, _ string, _ []string,
		_ string, _ ...string) (string, error) {
		return "", errors.New("fatal: bad revision")
	}

	for _, tc := range []struct{ name, workspace, base string }{
		{"git failure", "/tmp", "main"},
		{"no resolved base", "/tmp", ""},
		{"no workspace", "", "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakePREnsurer{subject: "feat: x", url: "https://example/pr/11"}
			e := &NativeAgentLoopExecutor{PREnsurer: fe}
			task := reviewTaskForPR(foremanv1alpha1.AgenticTaskKindReview, true)
			summary := "Adds a `/health` endpoint."
			r := &Result{Summary: summary, Extra: map[string]any{}}

			e.maybeOpenPullRequest(context.Background(), logr.Discard(), task, nil,
				foremanv1alpha1.AgenticTaskVerdictGo, r, tc.workspace, tc.base, nil)

			if body := fe.ensures[0].body; strings.Contains(body, "Unverified claims") {
				t.Errorf("no ground truth must mean no note; got %q", body)
			}
		})
	}
}
