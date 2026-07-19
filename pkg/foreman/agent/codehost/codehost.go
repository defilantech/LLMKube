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

// Package codehost provides a provider-neutral seam for code-host
// operations (clone URLs, pull request management, commit metadata).
// The GitHub adapter lives in this package so the rest of the agent
// ecosystem never imports githubpr or githubissue directly.
package codehost

import (
	"context"
	"regexp"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/githubpr"
)

// repoSlugPattern matches a single "owner/name" GitHub slug. Each segment
// is limited to git/GitHub-safe characters and exactly one slash is allowed,
// so "..", multiple path segments, and whitespace are rejected.
var repoSlugPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// CodeHost is the provider-neutral seam for code-host operations.
// Implementations wrap a specific platform (GitHub, Forgejo, etc.) and
// expose operations the executor needs without leaking platform details.
type CodeHost interface {
	// ResolveCloneURL derives the HTTPS git clone URL from a repo slug
	// like "owner/name". Returns "" for an empty or malformed slug so
	// callers fall back to the cloned fork's HEAD.
	ResolveCloneURL(repoSlug string) string

	// EnsureChangeRequest ensures a pull request exists for head → base,
	// creating it if absent. Returns the PR URL and whether it was
	// created (true) or already existed (false).
	EnsureChangeRequest(ctx context.Context, repoSlug, headBranch,
		baseBranch, title, body string) (url string, created bool, err error)

	// HeadCommitSubject returns the first line of the branch head's commit
	// message — the natural PR title (the coder writes a conventional
	// subject). Empty on any failure so callers can fall back.
	HeadCommitSubject(ctx context.Context, repoSlug, headBranch string) (string, error)
}

// GitHubCodeHost wraps a githubpr.Ensurer and provides the CodeHost
// interface backed by GitHub. The Ensurer is injected so tests can
// substitute a fake.
type GitHubCodeHost struct {
	Ensurer githubpr.Ensurer
	// Token authenticates the GitHub API calls (PR create, head-commit
	// read). Empty means unauthenticated; PR creation needs a real token,
	// so main wires it from the same source as the git auth.
	Token string
}

// NewGitHubCodeHost constructs a GitHubCodeHost from a githubpr.Ensurer.
func NewGitHubCodeHost(ensurer githubpr.Ensurer) *GitHubCodeHost {
	return &GitHubCodeHost{Ensurer: ensurer}
}

// ResolveCloneURL derives the upstream project's HTTPS git URL from a
// payload.repo "owner/name" slug (the GitHub convention LLMKube uses).
// It returns "" for an empty or malformed slug so callers fall back to
// the cloned fork's HEAD (e.g. freeform tasks that carry no repo slug).
func (g *GitHubCodeHost) ResolveCloneURL(repoSlug string) string {
	repoSlug = strings.TrimSpace(repoSlug)
	if !repoSlugPattern.MatchString(repoSlug) {
		return ""
	}
	return "https://github.com/" + repoSlug + ".git"
}

// EnsureChangeRequest ensures a pull request exists for head → base,
// creating it if absent. Returns the PR URL and whether it was created.
func (g *GitHubCodeHost) EnsureChangeRequest(
	ctx context.Context, repoSlug, headBranch, baseBranch, title, body string,
) (string, bool, error) {
	owner, name, ok := strings.Cut(repoSlug, "/")
	if !ok || owner == "" || name == "" {
		return "", false, nil
	}
	res, err := g.Ensurer.EnsurePR(ctx, owner, name, headBranch, baseBranch, title, body, g.Token)
	if err != nil {
		return "", false, err
	}
	return res.URL, res.Created, nil
}

// HeadCommitSubject returns the first line of the branch head's commit
// message — the natural PR title (the coder writes a conventional
// subject). Empty on any failure so callers can fall back.
func (g *GitHubCodeHost) HeadCommitSubject(ctx context.Context, repoSlug, headBranch string) (string, error) {
	owner, name, ok := strings.Cut(repoSlug, "/")
	if !ok || owner == "" || name == "" {
		return "", nil
	}
	return g.Ensurer.HeadCommitSubject(ctx, owner, name, headBranch, g.Token), nil
}
