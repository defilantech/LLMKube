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

package slicer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GitRunner runs one git subcommand in dir and returns its combined output.
// Production callers use the exec-backed default; tests inject a stub or run
// against a real temp repo. Matches Foreman's commandRunner convention so the
// library never hard-codes a git transport.
type GitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// execGitRunner is the production GitRunner, backed by os/exec.
func execGitRunner(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// IntegrateOptions configures a disjoint-slice union.
type IntegrateOptions struct {
	// RepoDir is the git worktree the slice branches are fetched into.
	RepoDir string
	// Base is the ref every slice was cut from and the union is built on
	// (the current upstream tip, not a stale fork default: see #813/#1029).
	Base string
	// Branch is the integration branch to create at Base and fill with the
	// union.
	Branch string
	// Slices are the slice branch refs to union, in application order.
	Slices []string
	// Run overrides the git transport. Nil uses the exec-backed default.
	Run GitRunner
}

// IntegrateResult is a successful union.
type IntegrateResult struct {
	// Branch is the integration branch that now holds the union commit.
	Branch string
	// UnionDiff is `git diff Base...Branch`, the input the reconcile step
	// hands to its LLM sweep.
	UnionDiff string
	// Owners maps each changed file to the slice that changed it. Because the
	// union is disjoint, every file has exactly one owner.
	Owners map[string]string
}

// OverlapError reports that two slices both changed the same file, so the
// union is not disjoint and cannot be a clean apply. A slice violated its
// assigned file scope; this is real signal, not a transient failure.
type OverlapError struct {
	File   string
	SliceA string
	SliceB string
}

func (e *OverlapError) Error() string {
	return fmt.Sprintf("slices %q and %q both change %q (union is not disjoint)", e.SliceA, e.SliceB, e.File)
}

// ApplyError reports that a slice's diff did not apply cleanly onto the base
// (typically the slice was cut from a stale base and its diff now conflicts).
type ApplyError struct {
	Slice  string
	Output string
}

func (e *ApplyError) Error() string {
	return fmt.Sprintf("slice %q did not apply cleanly onto base: %s", e.Slice, e.Output)
}

// EmptyUnionError reports that no slice changed any file, so there is nothing
// to integrate.
type EmptyUnionError struct{}

func (e *EmptyUnionError) Error() string { return "empty union: no slice changed any file" }

// Integrate unions the disjoint slice branches onto Base on a fresh Branch and
// returns the union diff for reconciliation. It first proves the slices are
// disjoint (no file changed by two slices), then applies each slice's diff onto
// the base and commits. It returns *OverlapError, *ApplyError, or
// *EmptyUnionError for the meaningful failure modes.
func Integrate(ctx context.Context, opts IntegrateOptions) (*IntegrateResult, error) {
	if opts.RepoDir == "" || opts.Base == "" || opts.Branch == "" || len(opts.Slices) == 0 {
		return nil, fmt.Errorf("integrate: RepoDir, Base, Branch and at least one slice are required")
	}
	run := opts.Run
	if run == nil {
		run = execGitRunner
	}

	// 1. Disjointness: no file may be changed by two slices.
	owners := make(map[string]string)
	for _, s := range opts.Slices {
		out, err := run(ctx, opts.RepoDir, "diff", "--name-only", opts.Base+"..."+s)
		if err != nil {
			return nil, fmt.Errorf("integrate: diff --name-only %s...%s: %w: %s", opts.Base, s, err, strings.TrimSpace(out))
		}
		for _, f := range nonEmptyLines(out) {
			if prev, ok := owners[f]; ok && prev != s {
				return nil, &OverlapError{File: f, SliceA: prev, SliceB: s}
			}
			owners[f] = s
		}
	}
	if len(owners) == 0 {
		return nil, &EmptyUnionError{}
	}

	// 2. Fresh integration branch at the current base.
	if out, err := run(ctx, opts.RepoDir, "checkout", "-B", opts.Branch, opts.Base); err != nil {
		return nil, fmt.Errorf("integrate: checkout -B %s %s: %w: %s", opts.Branch, opts.Base, err, strings.TrimSpace(out))
	}

	// 3. Apply each slice's diff onto the base. Disjoint files mean this is a
	// clean apply, not a semantic merge.
	for _, s := range opts.Slices {
		diff, err := run(ctx, opts.RepoDir, "diff", opts.Base+"..."+s)
		if err != nil {
			return nil, fmt.Errorf("integrate: diff %s...%s: %w: %s", opts.Base, s, err, strings.TrimSpace(diff))
		}
		if strings.TrimSpace(diff) == "" {
			continue
		}
		patch, err := writeTempPatch(diff)
		if err != nil {
			return nil, fmt.Errorf("integrate: stage patch for slice %q: %w", s, err)
		}
		out, err := run(ctx, opts.RepoDir, "apply", "--index", patch)
		_ = os.Remove(patch)
		if err != nil {
			return nil, &ApplyError{Slice: s, Output: strings.TrimSpace(out)}
		}
	}

	// 4. Commit the union. Pass the foreman bot identity explicitly via -c:
	// the integrate agent runs in a pod whose clone has no user.name/
	// user.email in any git config scope, where a bare `git commit` dies with
	// "Author identity unknown" (exit 128). This mirrors repo/branch.go, which
	// sets the same identity for the coder's commits.
	msg := "integrate: union of " + strings.Join(opts.Slices, " ")
	if out, err := run(ctx, opts.RepoDir,
		"-c", "user.name=foreman",
		"-c", "user.email=foreman@llmkube.dev",
		"commit", "-m", msg); err != nil {
		return nil, fmt.Errorf("integrate: commit union: %w: %s", err, strings.TrimSpace(out))
	}

	// 5. The union diff feeds the downstream reconcile step's LLM sweep.
	unionDiff, err := run(ctx, opts.RepoDir, "diff", opts.Base+"..."+opts.Branch)
	if err != nil {
		return nil, fmt.Errorf("integrate: diff %s...%s: %w: %s", opts.Base, opts.Branch, err, strings.TrimSpace(unionDiff))
	}

	return &IntegrateResult{Branch: opts.Branch, UnionDiff: unionDiff, Owners: owners}, nil
}

// writeTempPatch writes a diff to a temp file and returns its path; callers
// remove it after `git apply`.
func writeTempPatch(content string) (string, error) {
	f, err := os.CreateTemp("", "slicer-patch-*.diff")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// nonEmptyLines splits on newlines and drops blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}
