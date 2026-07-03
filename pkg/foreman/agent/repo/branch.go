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

package repo

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// gitRefAllowed limits refs/positional git arguments to git-ref-safe
// characters. Combined with the leading-dash and ".." checks in gitRefSafe,
// it prevents argv flag smuggling (a value like "--upload-pack=..." being
// read as a git option) and path-traversal in refs.
var gitRefAllowed = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// gitRefSafe reports whether s is safe to pass to git as a ref/positional
// argument: non-empty, not an option (no leading '-'), no ".." component, and
// composed only of git-ref-safe characters.
func gitRefSafe(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") &&
		!strings.Contains(s, "..") && gitRefAllowed.MatchString(s)
}

// BranchPrefix is the namespace under which foreman branches live.
// Keeping it consistent makes it trivial to write a fork-side cleanup
// job that prunes abandoned foreman branches.
const BranchPrefix = "foreman"

// maxSlugLen caps the issue-title slug inside the branch name so the
// branch fits comfortably in dashboards + git refs.
const maxSlugLen = 32

// BranchNameForIssue returns "foreman/issue-<N>-<slug>" where slug is
// the lowercase kebab-cased issue title, truncated to 32 chars and
// stripped of trailing dashes. The shape mirrors the autofix pipeline's
// convention so reviewers recognize foreman-authored branches at a
// glance.
func BranchNameForIssue(issueNumber int, issueTitle string) string {
	slug := slugify(issueTitle, maxSlugLen)
	if slug == "" {
		// Defensive: empty title still produces a stable branch name.
		return fmt.Sprintf("%s/issue-%d", BranchPrefix, issueNumber)
	}
	return fmt.Sprintf("%s/issue-%d-%s", BranchPrefix, issueNumber, slug)
}

// slugify lowercases s, replaces non-alphanumeric runs with a single
// '-', trims dashes from the ends, and truncates to maxLen.
func slugify(s string, maxLen int) string {
	var b strings.Builder
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	return out
}

// CreateAndCheckoutBranch creates a new branch at HEAD and switches to
// it. Equivalent to "git checkout -b <branch>".
func CreateAndCheckoutBranch(ctx context.Context, workspace, branch string) error {
	if branch == "" {
		return fmt.Errorf("CreateAndCheckoutBranch: branch is required")
	}
	if _, err := runGit(ctx, workspace, baseEnv(), "checkout", "-b", branch); err != nil {
		return err
	}
	return nil
}

// UpstreamBranchOptions configures CreateBranchFromUpstream.
type UpstreamBranchOptions struct {
	// Workspace is the cloned fork working tree to create the branch in.
	Workspace string
	// Branch is the new task branch name.
	Branch string
	// UpstreamURL is the upstream project's git URL (e.g.
	// https://github.com/<owner>/<name>.git). The base ref is fetched from here.
	UpstreamURL string
	// BaseBranch is the upstream ref to branch from. Empty defaults to "main".
	BaseBranch string
	// Auth, when non-nil, provides the GIT_ASKPASS scaffolding for the fetch.
	// A public upstream can leave it nil.
	Auth *Auth
}

// CreateBranchFromUpstream fetches BaseBranch from UpstreamURL and creates
// Branch at that fetched tip, so the task branch is cut from the CURRENT
// upstream base regardless of the cloned fork's sync state (#813). The fork
// stays origin (the branch still pushes there for the PR); only the base moves
// to upstream. The fetched tip is referenced via FETCH_HEAD, so no extra
// remote is left in the workspace and the helper is safe to re-run.
func CreateBranchFromUpstream(ctx context.Context, opts UpstreamBranchOptions) error {
	if opts.Workspace == "" {
		return fmt.Errorf("CreateBranchFromUpstream: Workspace is required")
	}
	if opts.Branch == "" {
		return fmt.Errorf("CreateBranchFromUpstream: Branch is required")
	}
	if opts.UpstreamURL == "" {
		return fmt.Errorf("CreateBranchFromUpstream: UpstreamURL is required")
	}
	base := opts.BaseBranch
	if base == "" {
		base = "main"
	}

	// Validate refs and the upstream URL before they reach git argv, so a
	// value beginning with '-' (or carrying a ".." traversal) cannot be
	// smuggled in as a git option. The URL is not a ref, so it only needs the
	// leading-dash guard.
	if !gitRefSafe(base) {
		return fmt.Errorf("CreateBranchFromUpstream: invalid base branch %q", base)
	}
	if !gitRefSafe(opts.Branch) {
		return fmt.Errorf("CreateBranchFromUpstream: invalid branch name %q", opts.Branch)
	}
	if strings.HasPrefix(opts.UpstreamURL, "-") {
		return fmt.Errorf("CreateBranchFromUpstream: invalid upstream url %q", opts.UpstreamURL)
	}

	env := baseEnv()
	if opts.Auth != nil {
		env = append(env, opts.Auth.Env()...)
	}

	// Fetch the current upstream base into FETCH_HEAD. The fork clone is full
	// (no --depth), so the new branch carries upstream history.
	if _, err := runGit(ctx, opts.Workspace, env, "fetch", opts.UpstreamURL, base); err != nil {
		return fmt.Errorf("CreateBranchFromUpstream: fetch %s %s: %w", opts.UpstreamURL, base, err)
	}

	// Create (or reset) the task branch at the fetched upstream tip and switch
	// to it. -B is used so the helper is idempotent.
	if _, err := runGit(ctx, opts.Workspace, baseEnv(), "checkout", "-B", opts.Branch, "FETCH_HEAD"); err != nil {
		return fmt.Errorf("CreateBranchFromUpstream: checkout -B %s FETCH_HEAD: %w", opts.Branch, err)
	}
	return nil
}

// RemoteRefBranchOptions configures CreateBranchFromRemoteRef.
type RemoteRefBranchOptions struct {
	// Workspace is the cloned working tree to create the branch in.
	Workspace string
	// Branch is the new task branch name.
	Branch string
	// Remote is the remote the ref is fetched from — the push remote
	// ("origin" in executor workspaces), where the prior attempt lives.
	Remote string
	// Ref is the branch name on Remote to restore from (the prior
	// attempt's branch).
	Ref string
	// Auth, when non-nil, provides the GIT_ASKPASS scaffolding for the
	// probe + fetch. A public remote can leave it nil.
	Auth *Auth
}

// CreateBranchFromRemoteRef fetches Ref from Remote and creates Branch
// at the fetched tip, so a revision task's workspace starts with the
// prior attempt's files present (#951: the executor owns git; the
// restore must not be prompt-driven). Returns found=false with a nil
// error when Remote has no such ref (prior attempt pruned or never
// pushed) so the caller can fall back to the base-branch path;
// transport/auth failures return an error.
func CreateBranchFromRemoteRef(ctx context.Context, opts RemoteRefBranchOptions) (found bool, err error) {
	if opts.Workspace == "" {
		return false, fmt.Errorf("CreateBranchFromRemoteRef: Workspace is required")
	}
	if !gitRefSafe(opts.Branch) {
		return false, fmt.Errorf("CreateBranchFromRemoteRef: invalid branch name %q", opts.Branch)
	}
	if !gitRefSafe(opts.Remote) {
		return false, fmt.Errorf("CreateBranchFromRemoteRef: invalid remote %q", opts.Remote)
	}
	if !gitRefSafe(opts.Ref) {
		return false, fmt.Errorf("CreateBranchFromRemoteRef: invalid ref %q", opts.Ref)
	}

	env := baseEnv()
	if opts.Auth != nil {
		env = append(env, opts.Auth.Env()...)
	}

	// Probe first: ls-remote exits 0 with empty output for a missing
	// ref, which cleanly separates "fall back to base" from transport
	// failures (which error out and should fail the task loudly).
	out, err := runGit(ctx, opts.Workspace, env, "ls-remote", opts.Remote, "refs/heads/"+opts.Ref)
	if err != nil {
		return false, fmt.Errorf("CreateBranchFromRemoteRef: ls-remote %s %s: %w", opts.Remote, opts.Ref, err)
	}
	if strings.TrimSpace(out) == "" {
		return false, nil
	}

	if _, err := runGit(ctx, opts.Workspace, env, "fetch", opts.Remote, opts.Ref); err != nil {
		return false, fmt.Errorf("CreateBranchFromRemoteRef: fetch %s %s: %w", opts.Remote, opts.Ref, err)
	}
	// -B for idempotency, mirroring CreateBranchFromUpstream.
	if _, err := runGit(ctx, opts.Workspace, baseEnv(), "checkout", "-B", opts.Branch, "FETCH_HEAD"); err != nil {
		return false, fmt.Errorf("CreateBranchFromRemoteRef: checkout -B %s FETCH_HEAD: %w", opts.Branch, err)
	}
	return true, nil
}

// baseEnv is the minimal env for read/local-only git ops that do not
// need GIT_ASKPASS (branch, status, log). HOME is carried through so
// git can read ~/.gitconfig if present.
func baseEnv() []string {
	return []string{"HOME=" + envOr("HOME", "/tmp")}
}
