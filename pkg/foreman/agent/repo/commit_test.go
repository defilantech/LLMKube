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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSoftResetToBase_AnchorsAtBranchPointNotUpstreamTip reproduces #1002: when
// upstream advances mid-run, BaseBranchSHA returns a tip AHEAD of where the task
// branch was actually cut. The soft reset must anchor at the true branch point
// (merge-base(base, HEAD)) so only the model's own edits are re-staged — the
// intervening upstream delta must never be dragged into the recovered commit.
func TestSoftResetToBase_AnchorsAtBranchPointNotUpstreamTip(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	bare := initBareOrigin(t, filepath.Join(dir, "up"))
	seedOrigin(t, bare) // main @ A (README.md)

	// Workspace cut from A; the model self-commits fix.txt on a task branch.
	ws := mustClone(t, bare, filepath.Join(dir, "ws"))
	mustGit(t, ws, "checkout", "-b", "foreman/wl/issue-1002")
	if err := os.WriteFile(filepath.Join(ws, "fix.txt"), []byte("model edit\n"), 0o644); err != nil {
		t.Fatalf("write fix.txt: %v", err)
	}
	mustGit(t, ws, "-c", "user.email=u@x", "-c", "user.name=u", "add", "fix.txt")
	mustGit(t, ws, "-c", "user.email=u@x", "-c", "user.name=u", "commit", "-m", "model self-commit")

	// Upstream advances to B (adds upstream.txt) AFTER the branch was cut.
	adv := mustClone(t, bare, filepath.Join(dir, "adv"))
	commitFile(t, adv, "upstream.txt", "intervening upstream delta\n")

	// Recovery: BaseBranchSHA fetches the current upstream tip (B) into ws and
	// returns it — the value the executor passes to SoftResetToBase.
	baseSHA, err := BaseBranchSHA(context.Background(), ws, bare, "main")
	if err != nil {
		t.Fatalf("BaseBranchSHA: %v", err)
	}
	if err := SoftResetToBase(context.Background(), ws, baseSHA); err != nil {
		t.Fatalf("SoftResetToBase: %v", err)
	}

	staged := gitOut(t, ws, "diff", "--cached", "--name-only")
	if !strings.Contains(staged, "fix.txt") {
		t.Errorf("model edit fix.txt must be staged for the recovered commit; staged=%q", staged)
	}
	if strings.Contains(staged, "upstream.txt") {
		t.Errorf("intervening upstream delta must NOT be re-staged (would revert merged work); staged=%q", staged)
	}
}

// TestSoftResetToBase_NoSelfCommitIsNothingToCommit verifies that when the model
// added no commits of its own, recovery reports ErrNothingToCommit rather than
// fabricating a commit — even if upstream advanced past the branch point.
func TestSoftResetToBase_NoSelfCommitIsNothingToCommit(t *testing.T) {
	gitOrSkip(t)
	dir := t.TempDir()
	bare := initBareOrigin(t, filepath.Join(dir, "up"))
	seedOrigin(t, bare)

	ws := mustClone(t, bare, filepath.Join(dir, "ws"))
	mustGit(t, ws, "checkout", "-b", "foreman/wl/issue-1002")

	// Upstream advances; the workspace itself made no commits.
	adv := mustClone(t, bare, filepath.Join(dir, "adv"))
	commitFile(t, adv, "upstream.txt", "intervening upstream delta\n")

	baseSHA, err := BaseBranchSHA(context.Background(), ws, bare, "main")
	if err != nil {
		t.Fatalf("BaseBranchSHA: %v", err)
	}
	if err := SoftResetToBase(context.Background(), ws, baseSHA); err != ErrNothingToCommit {
		t.Errorf("want ErrNothingToCommit, got %v", err)
	}
}
