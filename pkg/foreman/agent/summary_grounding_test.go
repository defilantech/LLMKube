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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loggingDiff is the shape of the diff behind #1411's report: real
// structured-logging work, and not one occurrence of "health".
const loggingDiff = `diff --git a/bridge/app.py b/bridge/app.py
--- a/bridge/app.py
+++ b/bridge/app.py
@@ -1,8 +1,12 @@
+import logging
+
+logger = logging.getLogger(__name__)
+
 def handle(event):
-    print("claiming", event)
+    logger.info("claiming", extra={"event": event})
     return claim(event)
`

// TestUngroundedSummaryClaims_HealthEndpoint is the #1411 regression: the
// PR body claimed a /health endpoint that the diff never added. The claim
// must be reported.
func TestUngroundedSummaryClaims_HealthEndpoint(t *testing.T) {
	summary := "Introduces structured JSON logging across the bridge, replacing all bare " +
		"`print()` calls with `logger.info()`, and adds a `/health` endpoint."
	g := diffGroundTruth{text: loggingDiff, files: []string{"bridge/app.py"}}

	got := ungroundedSummaryClaims(summary, g)

	if len(got) != 1 || got[0] != "/health" {
		t.Fatalf("want exactly [/health] reported, got %v", got)
	}
}

// TestUngroundedSummaryClaims_NoFalsePositives is the guard that matters
// more than the check itself: a summary whose claims the diff does support
// must come back clean. Every case here is a way an over-eager check would
// put a warning on an honest PR.
func TestUngroundedSummaryClaims_NoFalsePositives(t *testing.T) {
	cases := []struct {
		name    string
		summary string
		g       diffGroundTruth
	}{
		{
			// The rename trap: `oldHandler` no longer exists at HEAD, but
			// the diff removed it, so the claim is true.
			name:    "renamed identifier grounds on the removed line",
			summary: "Renames `oldHandler` to `newHandler` for clarity.",
			g: diffGroundTruth{text: "@@\n-func oldHandler() {}\n+func newHandler() {}\n",
				files: []string{"pkg/http/handler.go"}},
		},
		{
			name:    "call claim grounds on its receiver atom",
			summary: "Replaces `print()` with `logger.info()` throughout.",
			g:       diffGroundTruth{text: loggingDiff, files: []string{"bridge/app.py"}},
		},
		{
			name:    "bare lowercase word in backticks is not a checkable claim",
			summary: "Adds `logging` and `tests` for the claim path.",
			g:       diffGroundTruth{text: "@@\n+x := 1\n", files: []string{"a.go"}},
		},
		{
			name:    "backticked prose is not a single claim",
			summary: "Verified with `go test ./... -run TestNothingHere`.",
			g:       diffGroundTruth{text: "@@\n+x := 1\n", files: []string{"a.go"}},
		},
		{
			name:    "fenced code sample is an illustration, not a claim",
			summary: "Adds the helper:\n```go\nfunc totallyAbsentSymbol_XYZ() {}\n```",
			g:       diffGroundTruth{text: "@@\n+x := 1\n", files: []string{"a.go"}},
		},
		{
			name:    "path claim grounds on the changed-file list by basename",
			summary: "Rewrites `agent/loop.go` to drain the channel.",
			g:       diffGroundTruth{text: "@@\n+x := 1\n", files: []string{"pkg/foreman/agent/loop.go"}},
		},
		{
			name:    "no ground truth means no check",
			summary: "Adds a `/health` endpoint and `SomeSymbol`.",
			g:       diffGroundTruth{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ungroundedSummaryClaims(tc.summary, tc.g); len(got) != 0 {
				t.Errorf("honest summary flagged claims %v", got)
			}
		})
	}
}

// TestUngroundedSummaryClaims_UnchangedButRealFileIsGrounded: a summary
// that points at a file it did not change ("follows the pattern in X") is
// making a true statement. Grounding path claims on existence in the
// workspace, not only on the diff, keeps that out of the warning.
func TestUngroundedSummaryClaims_UnchangedButRealFileIsGrounded(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "pkg", "foreman"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(ws, "pkg", "foreman", "existing_helper.go")
	if err := os.WriteFile(real, []byte("package foreman\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := diffGroundTruth{workspace: ws, text: "@@\n+x := 1\n", files: []string{"other.go"}}

	summary := "Follows the pattern in `pkg/foreman/existing_helper.go`."
	if got := ungroundedSummaryClaims(summary, g); len(got) != 0 {
		t.Errorf("a real (if unchanged) path must not be flagged; got %v", got)
	}

	summary = "Follows the pattern in `pkg/foreman/invented_helper.go`."
	if got := ungroundedSummaryClaims(summary, g); len(got) != 1 || got[0] != "pkg/foreman/invented_helper.go" {
		t.Errorf("an invented path must be flagged; got %v", got)
	}
}

// TestUngroundedSummaryClaims_PathAtomsDoNotGround: a fabricated path must
// not be rescued by its leading segments. `pkg` and `agent` appear in
// almost every diff of this repo.
func TestUngroundedSummaryClaims_PathAtomsDoNotGround(t *testing.T) {
	g := diffGroundTruth{
		text:  "diff --git a/pkg/foreman/agent/loop.go b/pkg/foreman/agent/loop.go\n@@\n+x := 1\n",
		files: []string{"pkg/foreman/agent/loop.go"},
	}
	summary := "Adds the retry to `pkg/foreman/agent/nonexistent_gate.go`."
	got := ungroundedSummaryClaims(summary, g)
	if len(got) != 1 || got[0] != "pkg/foreman/agent/nonexistent_gate.go" {
		t.Fatalf("fabricated path must be flagged, got %v", got)
	}
}

// TestClassifySummaryClaim pins which backtick spans are treated as
// concrete claims. Everything classified claimSkip is prose the check
// refuses to interpret.
func TestClassifySummaryClaim(t *testing.T) {
	cases := []struct {
		span string
		want claimKind
		norm string
	}{
		{"/health", claimRoute, "/health"},
		{"/api/v1/models", claimRoute, "/api/v1/models"},
		{"pkg/foreman/agent/loop.go", claimPath, "pkg/foreman/agent/loop.go"},
		{"README.md", claimPath, "README.md"},
		{"submit_result", claimIdent, "submit_result"},
		{"logger.info()", claimIdent, "logger.info()"},
		{"EnsurePR", claimIdent, "EnsurePR"},
		{"issueAsk", claimIdent, "issueAsk"},
		{"Handler.", claimIdent, "Handler"}, // trailing sentence period trimmed
		// Not checkable: indistinguishable from emphasis or prose.
		{"logging", claimSkip, ""},
		{"health", claimSkip, ""},
		{"git diff --name-only", claimSkip, ""},
		{"logger.info()/warning()/error()", claimSkip, ""},
		{"", claimSkip, ""},
		{"...", claimSkip, ""},
	}
	for _, tc := range cases {
		t.Run(tc.span, func(t *testing.T) {
			norm, kind := classifySummaryClaim(tc.span)
			if kind != tc.want || norm != tc.norm {
				t.Errorf("classifySummaryClaim(%q) = (%q, %d), want (%q, %d)",
					tc.span, norm, kind, tc.norm, tc.want)
			}
		})
	}
}

// TestAnnotateUnverifiedClaims: the note is appended, never substituted --
// the model's prose survives verbatim so an accurate description is never
// destroyed by a wrong guess about which sentence a claim belongs to.
func TestAnnotateUnverifiedClaims(t *testing.T) {
	summary := "Adds structured logging and a `/health` endpoint."
	got := annotateUnverifiedClaims(summary, []string{"/health"})

	if !strings.HasPrefix(got, summary) {
		t.Errorf("original summary must survive verbatim at the head; got %q", got)
	}
	if !strings.Contains(got, "Unverified claims") || !strings.Contains(got, "`/health`") {
		t.Errorf("note must name the unverified claim; got %q", got)
	}
	if unchanged := annotateUnverifiedClaims(summary, nil); unchanged != summary {
		t.Errorf("no claims means no note; got %q", unchanged)
	}
}

// TestAnnotateUnverifiedClaims_ListIsBounded keeps a confabulated summary
// from burying the diff link under a wall of generated text.
func TestAnnotateUnverifiedClaims_ListIsBounded(t *testing.T) {
	claims := []string{"/a1", "/b2", "/c3", "/d4", "/e5", "/f6", "/g7"}
	got := annotateUnverifiedClaims("x", claims)
	if strings.Contains(got, "`/g7`") {
		t.Errorf("list must be capped at %d entries; got %q", maxListedUnverifiedClaims, got)
	}
	if !strings.Contains(got, "+2 more") {
		t.Errorf("the elided count must be stated; got %q", got)
	}
}

// TestBranchDiffText_GitFailureIsEmpty: a git error must produce no ground
// truth at all, so ungroundedSummaryClaims skips rather than warning.
func TestBranchDiffText_GitFailureIsEmpty(t *testing.T) {
	failing := func(context.Context, string, []string, string, ...string) (string, error) {
		return "partial output", errors.New("fatal: bad revision")
	}
	if got := branchDiffText(context.Background(), t.TempDir(), "main", failing); got != "" {
		t.Errorf("git failure must yield empty diff text, got %q", got)
	}
}
