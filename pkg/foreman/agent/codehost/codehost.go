/*
Copyright 2026.

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

// Package codehost provides a provider-neutral seam for code-host
// operations (clone URLs, pull request management, commit metadata).
// The GitHub adapter lives in this package so the rest of the agent
// ecosystem never imports githubpr or githubissue directly.
package codehost

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/githubpr"
)

// RepoSlugPattern matches a repo slug with one or more path segments
// separated by slashes, e.g. "owner/name" (GitHub) or
// "group/subgroup/project" (GitLab / nested Forgejo). Each segment is
// limited to git/GitHub-safe characters. The pattern requires at least
// two segments (one slash) and rejects empty segments, whitespace, and
// absolute paths. Path-traversal segments ("..") are rejected by
// IsValidRepoSlug, which wraps this pattern.
var RepoSlugPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)+$`)

// IsValidRepoSlug reports whether slug is a valid repo slug: it matches
// RepoSlugPattern and contains no ".." path-traversal segment. The
// character class in RepoSlugPattern includes ".", so ".." would
// otherwise match; this check closes that gap so multi-segment slugs
// cannot smuggle traversal segments.
func IsValidRepoSlug(slug string) bool {
	if !RepoSlugPattern.MatchString(slug) {
		return false
	}
	for _, seg := range strings.Split(slug, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// SplitRepoSlug splits a repo slug on the last slash so the final
// segment is the repository name and everything before it is the
// namespace. This supports multi-segment slugs like
// "group/subgroup/project" (GitLab / nested Forgejo) while keeping
// "owner/name" working exactly as before. Returns ok=false when the
// slug is empty, has no slash, has an empty namespace or name, or
// contains a ".." traversal segment.
func SplitRepoSlug(slug string) (namespace, name string, ok bool) {
	if !IsValidRepoSlug(slug) {
		return "", "", false
	}
	idx := strings.LastIndex(slug, "/")
	if idx <= 0 || idx == len(slug)-1 {
		return "", "", false
	}
	return slug[:idx], slug[idx+1:], true
}

// CodeHost is the provider-neutral seam for code-host operations.
// Implementations wrap a specific platform (GitHub, Forgejo, etc.) and
// expose operations the executor needs without leaking platform details.
type CodeHost interface {
	// ResolveCloneURL derives the HTTPS git clone URL from a repo slug
	// like "owner/name" (GitHub) or "group/subgroup/project" (GitLab /
	// nested Forgejo). Returns "" for an empty or malformed slug; the
	// executor's fork-HEAD fallback then applies to freeform tasks
	// (which carry no slug by design), while repo-bearing kinds (issue-
	// fix, verify, review) refuse an empty slug as a configuration
	// error (#1625) rather than basing on a possibly-stale fork HEAD.
	ResolveCloneURL(repoSlug string) string

	// EnsureChangeRequest ensures a pull request exists for head → base,
	// creating it if absent. draft opens the PR as a work-in-progress
	// draft (a human reads it and marks it ready) rather than straight
	// for review; the executor passes true so a Foreman PR is reviewable
	// on arrival.
	EnsureChangeRequest(ctx context.Context, repoSlug, headBranch,
		baseBranch, title, body string, draft bool) (url string, created bool, err error)

	// PullRequestUpdate replaces the body of the existing pull request for
	// head so a fix cycle can refresh the description to match what the
	// amended branch now contains. Returns the PR URL, or "" on any
	// failure (the caller keeps the stale body).
	PullRequestUpdate(ctx context.Context, repoSlug, head, body string) (url string, err error)

	// HeadCommitSubject returns the first line of the branch head's commit
	// message — the natural PR title (the coder writes a conventional
	// subject). Empty on any failure so callers can fall back.
	HeadCommitSubject(ctx context.Context, repoSlug, headBranch string) (string, error)
}

// ErrNotSupported reports an operation the selected forge provider cannot
// perform. A provider without an optional capability returns it rather than a
// plausible wrong answer, so the caller degrades explicitly (#1300).
var ErrNotSupported = errors.New("not supported by this forge")

// GitHubCodeHost wraps a githubpr.Ensurer and provides the CodeHost
// interface backed by GitHub. The Ensurer is injected so tests can
// substitute a fake.
type GitHubCodeHost struct {
	Ensurer githubpr.Ensurer
	// Token authenticates the GitHub API calls (PR create, head-commit
	// read). Empty means unauthenticated; PR creation needs a real token,
	// so main wires it from the same source as the git auth.
	Token string
	// TokenSource, when Token is empty, resolves the token on every call, so
	// a rotated credential is picked up without restarting the agent. A
	// resolution error means an unauthenticated call, as an empty Token does.
	TokenSource func() (string, error)
	// CloneBaseURL is the scheme and host clone URLs are built on, for a
	// GitHub Enterprise Server. Empty means https://github.com.
	CloneBaseURL string
}

// token is the credential for one API call.
func (g *GitHubCodeHost) token() string {
	if g.Token != "" || g.TokenSource == nil {
		return g.Token
	}
	t, err := g.TokenSource()
	if err != nil {
		return ""
	}
	return t
}

// NewGitHubCodeHost constructs a GitHubCodeHost from a githubpr.Ensurer.
func NewGitHubCodeHost(ensurer githubpr.Ensurer) *GitHubCodeHost {
	return &GitHubCodeHost{Ensurer: ensurer}
}

// ResolveCloneURL derives the upstream project's HTTPS git URL from a
// payload.repo slug (e.g. "owner/name" for GitHub, "group/subgroup/project"
// for GitLab / nested Forgejo). It returns "" for an empty or malformed
// slug so callers fall back to the cloned fork's HEAD (e.g. freeform tasks
// that carry no repo slug).
func (g *GitHubCodeHost) ResolveCloneURL(repoSlug string) string {
	repoSlug = strings.TrimSpace(repoSlug)
	if !IsValidRepoSlug(repoSlug) {
		return ""
	}
	base := strings.TrimRight(g.CloneBaseURL, "/")
	if base == "" {
		base = "https://github.com"
	}
	return base + "/" + repoSlug + ".git"
}

// EnsureChangeRequest ensures a pull request exists for head → base,
// creating it if absent. draft opens the PR as a work-in-progress draft
// (a human reads it and marks it ready) rather than straight for review,
// so a Foreman PR is reviewable on arrival. Returns the PR URL and
// whether it was created (true) or already existed (false).
func (g *GitHubCodeHost) EnsureChangeRequest(
	ctx context.Context, repoSlug, headBranch, baseBranch, title, body string, draft bool,
) (string, bool, error) {
	owner, name, ok := SplitRepoSlug(repoSlug)
	if !ok {
		return "", false, nil
	}
	res, err := g.Ensurer.EnsurePR(ctx, owner, name, headBranch, baseBranch, title, body, draft, g.token())
	if err != nil {
		return "", false, err
	}
	// A PR that already existed (Created: false) is a fix cycle: refresh
	// its body so the description matches what the amended head branch now
	// contains (#1567). A freshly created PR already carries this body.
	if !res.Created {
		if _, updErr := g.Ensurer.UpdatePR(ctx, owner, name, headBranch, body, g.token()); updErr != nil {
			return res.URL, false, updErr
		}
		return res.URL, false, nil
	}
	return res.URL, res.Created, nil
}

// PullRequestUpdate replaces the body of the existing pull request for
// head so a fix cycle can refresh the description to match the amended
// branch. Returns the PR URL, or "" on any failure (the caller keeps the
// stale body).
func (g *GitHubCodeHost) PullRequestUpdate(
	ctx context.Context, repoSlug, head, body string,
) (string, error) {
	owner, name, ok := SplitRepoSlug(repoSlug)
	if !ok {
		return "", fmt.Errorf("PullRequestUpdate: repo slug %q is not a valid repo slug", repoSlug)
	}
	return g.Ensurer.UpdatePR(ctx, owner, name, head, body, g.token())
}

// HeadCommitSubject returns the first line of the branch head's commit
// message — the natural PR title (the coder writes a conventional
// subject). Empty on any failure so callers can fall back.
func (g *GitHubCodeHost) HeadCommitSubject(ctx context.Context, repoSlug, headBranch string) (string, error) {
	owner, name, ok := SplitRepoSlug(repoSlug)
	if !ok {
		return "", nil
	}
	return g.Ensurer.HeadCommitSubject(ctx, owner, name, headBranch, g.token()), nil
}
