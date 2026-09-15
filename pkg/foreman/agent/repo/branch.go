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
	"os"
	"path/filepath"
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

// RebaseOntoBaseOptions configures RebaseOntoBase.
type RebaseOntoBaseOptions struct {
	// Workspace is the working tree whose current branch is rebased.
	Workspace string
	// BaseBranch is the ref to rebase onto. Empty defaults to "main".
	BaseBranch string
	// UpstreamURL is the git URL the base ref is fetched from (the task's
	// upstream/own repo). When empty there is no base source (e.g. a freeform
	// task) and the rebase is a no-op.
	UpstreamURL string
	// Auth, when non-nil, provides the GIT_ASKPASS scaffolding for the fetch.
	Auth *Auth
	// LeaveConflicts, when true, changes the conflict behavior: instead of
	// aborting the half-applied rebase and returning a plain error, the
	// conflicted state is LEFT in the workspace and a *RebaseConflictError
	// naming the unmerged files is returned. The caller can then hand the
	// mid-rebase workspace to the coder loop to resolve (#1839). When false
	// (the default) the conflict aborts and fails loud, preserving the
	// pre-#1839 contract for callers that cannot resolve.
	LeaveConflicts bool
}

// RebaseConflictError reports that RebaseOntoBase hit a conflict and, because
// LeaveConflicts was set, left the workspace mid-rebase rather than aborting.
// Files names the unmerged paths.
type RebaseConflictError struct {
	Base  string
	Files []string
}

func (e *RebaseConflictError) Error() string {
	return fmt.Sprintf("RebaseOntoBase: rebase onto %s left %d conflict(s): %s",
		e.Base, len(e.Files), strings.Join(e.Files, ", "))
}

// RebaseOntoBase fetches BaseBranch from UpstreamURL and rebases the current
// branch onto that fetched tip, so a restored prior attempt (see
// CreateBranchFromRemoteRef) replays its commits ON TOP of the CURRENT base.
// Any work merged into base since the prior attempt is preserved rather than
// reverted — the bug that made a stale revision branch delete already-merged
// files. On a rebase conflict the default is to abort the half-applied rebase
// and return an error so the task fails loud instead of pushing a branch that
// reverts merged work; with opts.LeaveConflicts the conflicted state is left in
// place and a *RebaseConflictError is returned instead (#1839). When
// UpstreamURL is empty there is no base to rebase onto and it is a no-op.
func RebaseOntoBase(ctx context.Context, opts RebaseOntoBaseOptions) error {
	if opts.Workspace == "" {
		return fmt.Errorf("RebaseOntoBase: Workspace is required")
	}
	if opts.UpstreamURL == "" {
		return nil
	}
	base := opts.BaseBranch
	if base == "" {
		base = "main"
	}
	if !gitRefSafe(base) {
		return fmt.Errorf("RebaseOntoBase: invalid base branch %q", base)
	}
	if strings.HasPrefix(opts.UpstreamURL, "-") {
		return fmt.Errorf("RebaseOntoBase: invalid upstream url %q", opts.UpstreamURL)
	}

	env := baseEnv()
	if opts.Auth != nil {
		env = append(env, opts.Auth.Env()...)
	}
	if _, err := runGit(ctx, opts.Workspace, env, "fetch", opts.UpstreamURL, base); err != nil {
		return fmt.Errorf("RebaseOntoBase: fetch %s %s: %w", opts.UpstreamURL, base, err)
	}
	// git rebase re-commits the replayed commits, so it needs a committer
	// identity even though each commit's original author is preserved. Supply a
	// stable foreman identity so the rebase never fails on a freshly-cloned
	// workspace with no user.name/email configured.
	rebaseEnv := append(baseEnv(),
		"GIT_COMMITTER_NAME=foreman",
		"GIT_COMMITTER_EMAIL=foreman@llmkube.dev",
	)
	if _, err := runGit(ctx, opts.Workspace, rebaseEnv, "rebase", "FETCH_HEAD"); err != nil {
		if opts.LeaveConflicts {
			// Hand the conflict to the coder loop rather than aborting: leave
			// the workspace mid-rebase and report the unmerged files (#1839).
			// The #1042/#1364 invariant (never land a branch that reverts
			// merged work) is preserved downstream by verifying the rebase
			// actually completed cleanly before the GO commits.
			files := unmergedFiles(ctx, opts.Workspace)
			return &RebaseConflictError{Base: base, Files: files}
		}
		// Leave the workspace clean: a conflict means the revision genuinely
		// clashes with merged work and must fail loud, not silently revert it.
		_, _ = runGit(ctx, opts.Workspace, baseEnv(), "rebase", "--abort")
		return fmt.Errorf("RebaseOntoBase: rebase onto %s: %w", base, err)
	}
	return nil
}

// unmergedFiles returns the paths git reports as unmerged (conflicted) in the
// workspace. Best-effort: a git error yields an empty slice.
func unmergedFiles(ctx context.Context, workspace string) []string {
	out, err := runGit(ctx, workspace, baseEnv(), "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// RebaseUnresolved reports whether the workspace is still in an unfinished
// rebase or carries unmerged files — i.e. a rebase conflict was never resolved.
// It is the post-loop guard for #1839: a coder GO must not land while the
// workspace is mid-rebase or still conflicted. The returned string explains
// which condition tripped, for the INCOMPLETE reason. Best-effort and
// fail-closed: if it cannot tell (a git error), it reports unresolved so a
// questionable state never lands.
func RebaseUnresolved(ctx context.Context, workspace string) (bool, string) {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(workspace, ".git", dir)); err == nil {
			return true, "workspace is mid-rebase (.git/" + dir + " present)"
		}
	}
	out, err := runGit(ctx, workspace, baseEnv(), "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return true, "could not determine merge state: " + err.Error()
	}
	if strings.TrimSpace(out) != "" {
		return true, "unmerged files remain: " + strings.Join(strings.Fields(out), ", ")
	}
	return false, ""
}

// baseEnv is the minimal env for read/local-only git ops that do not
// need GIT_ASKPASS (branch, status, log). HOME is carried through so
// git can read ~/.gitconfig if present.
func baseEnv() []string {
	return []string{"HOME=" + envOr("HOME", "/tmp")}
}
