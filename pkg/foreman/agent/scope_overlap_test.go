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

// Whitebox tests for the computable scope-overlap check (#647). The
// extraction fixtures are the real issue bodies from the cases that
// motivated the feature: #379 (the scope-drift trap every reviewer
// model except tool-using Claude has missed at least once) and #510
// (a docs nudge a reviewer once NO-GO'd against a hallucinated ask).

import (
	"reflect"
	"testing"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// issue379Body is the verbatim body of defilantech/LLMKube#379,
// including the escaped backticks GitHub's API returns.
const issue379Body = "## Background\n\n" +
	"PR #376 (the metal-accelerator phase-Ready fix) added a new RBAC marker " +
	"(\\`core/endpoints\\` get/list/watch) to the controller via \\`kubebuilder:rbac\\` " +
	"annotation. \\`make manifests\\` regenerated \\`config/rbac/role.yaml\\` " +
	"as expected. But the Helm chart's hand-synced " +
	"\\`charts/llmkube/templates/clusterrole.yaml\\` was NOT updated " +
	"automatically — there's no equivalent to PR #367's CRD sync guard for RBAC.\n\n" +
	"## What's needed\n\n" +
	"A CI guard that diffs the Helm chart's ClusterRole rules against the " +
	"kubebuilder-source ClusterRole rules " +
	"(\\`config/rbac/role.yaml\\` after \\`make manifests\\`) and fails if they " +
	"disagree. Mirror of \\`scripts/sync-crds.sh\\` " +
	"and the corresponding CI step from PR #367, but for RBAC.\n\n" +
	"## Implementation sketch\n\n" +
	"1. Script \\`scripts/check-helm-rbac.sh\\` (or similar): parse both YAMLs, " +
	"compare rules sets, exit non-zero on diff.\n" +
	"2. Workflow step in the existing GitHub Actions job that runs CRD sync check, " +
	"or a sibling job.\n" +
	"3. Optionally: a companion \\`scripts/sync-helm-rbac.sh\\` that auto-applies " +
	"the kubebuilder-generated rules into " +
	"the chart template, so contributors can run \\`make chart-rbac\\` like they " +
	"run \\`make chart-crds\\`.\n"

const issue510Body = "## Feature Description\n\n" +
	"After PR #508 lands the `make lint-all` target, point contributors at it from\n" +
	"`AGENTS.md` (and/or `CONTRIBUTING.md`) so they actually use it. Today the\n" +
	"target is discoverable only via `make help`; a one-line nudge in the standards\n" +
	"doc costs almost nothing and saves a CI round\n"

func TestExtractIssuePathRefs_Issue379(t *testing.T) {
	got := extractIssuePathRefs(issue379Body, nil)
	want := []string{
		"config/rbac/role.yaml",
		"charts/llmkube/templates/clusterrole.yaml",
		"scripts/sync-crds.sh",
		"scripts/check-helm-rbac.sh",
		"scripts/sync-helm-rbac.sh",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractIssuePathRefs(#379) = %v, want %v", got, want)
	}
}

func TestExtractIssuePathRefs_Issue510BareFilenames(t *testing.T) {
	got := extractIssuePathRefs(issue510Body, nil)
	want := []string{"AGENTS.md", "CONTRIBUTING.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractIssuePathRefs(#510) = %v, want %v", got, want)
	}
}

func TestExtractIssuePathRefs_IgnoresCommandsAndAPIGroups(t *testing.T) {
	body := "Run `make manifests` then check `core/endpoints` and `kubebuilder:rbac` " +
		"plus `discovery.k8s.io/v1.EndpointSlice` and plain words."
	if got := extractIssuePathRefs(body, nil); len(got) != 0 {
		t.Errorf("commands, API groups, and identifiers must not extract as path refs; got %v", got)
	}
}

func TestExtractIssuePathRefs_EmptyBody(t *testing.T) {
	if got := extractIssuePathRefs("", nil); len(got) != 0 {
		t.Errorf("empty body should yield no refs; got %v", got)
	}
}

// godotIssueBody mirrors misospace/windowstead#234: a Godot issue that
// names `.gd` source files. `.gd` is not in the language-agnostic base
// set, so extraction depends entirely on the task declaring its source
// language via GateProfile.SourceExtensions.
const godotIssueBody = "## Problem\n\n" +
	"`_on_tick()` in `scripts/main.gd` (line 1360) fires before the world is\n" +
	"ready, so the first tick reads an empty reservation table.\n\n" +
	"## Fix\n\n" +
	"Guard the early call and add coverage in `tests/test_tick_integration.gd`\n" +
	"alongside the existing `tests/test_e2e.gd` suite.\n"

// TestExtractIssuePathRefs_GodotExtensionsFromGateProfile is the
// windowstead-234 regression: without the task's source extensions the
// extractor is blind to `.gd`, so the scope-overlap vouch never fires
// and an honest GO gets demoted by the stochastic issueAsk rail. With
// sourceExtensions=[".gd"] the named files extract as refs.
func TestExtractIssuePathRefs_GodotExtensionsFromGateProfile(t *testing.T) {
	if got := extractIssuePathRefs(godotIssueBody, nil); len(got) != 0 {
		t.Errorf("without .gd in the source extensions, no .gd refs should extract; got %v", got)
	}
	got := extractIssuePathRefs(godotIssueBody, []string{".gd"})
	want := []string{
		"scripts/main.gd",
		"tests/test_tick_integration.gd",
		"tests/test_e2e.gd",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractIssuePathRefs(godot, [.gd]) = %v, want %v", got, want)
	}
}

// TestExtractIssuePathRefs_SourceExtensionsUnionWithBase verifies the
// declared extensions are unioned with, not substituted for, the
// language-agnostic base: a Godot issue that also names a `.md` doc
// yields both once `.gd` is declared.
func TestExtractIssuePathRefs_SourceExtensionsUnionWithBase(t *testing.T) {
	body := "See `scripts/main.gd` and update `README.md` accordingly."
	got := extractIssuePathRefs(body, []string{".gd"})
	want := []string{"scripts/main.gd", "README.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("union extraction = %v, want %v", got, want)
	}
}

// TestEnforceReviewerScopeOverlap_GodotRefMatchVouches is the end-to-end
// windowstead-234 case: the diff touches two files the issue names, so
// scope overlap is non-empty, drift is false, the GO stands, and
// scopeMatched is populated for the issueAsk vouch to consume.
func TestEnforceReviewerScopeOverlap_GodotRefMatchVouches(t *testing.T) {
	diff := []string{"scripts/main.gd", "tests/test_tick_integration.gd"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, godotIssueBody, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".gd"})
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("a diff touching issue-named .gd files must not demote; got %v", got)
	}
	if v, _ := extra["scopeDriftDetected"].(bool); v {
		t.Errorf("scopeDriftDetected should be false when .gd refs match")
	}
	matched, _ := extra["scopeMatched"].([]string)
	if !reflect.DeepEqual(matched, []string{"scripts/main.gd", "tests/test_tick_integration.gd"}) {
		t.Errorf("scopeMatched = %v, want the two touched .gd files", matched)
	}
}

// TestEnforceReviewerScopeOverlap_GodotBlindWithoutExtensions locks in
// the pre-fix failure mode: with no declared extensions the same Godot
// diff produces zero refs, so the check stays observe-only and writes
// no scope annotations — leaving the issueAsk vouch nothing to consume.
func TestEnforceReviewerScopeOverlap_GodotBlindWithoutExtensions(t *testing.T) {
	diff := []string{"scripts/main.gd", "tests/test_tick_integration.gd"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, godotIssueBody, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("no refs must pass through; got %v", got)
	}
	if _, present := extra["scopeMatched"]; present {
		t.Errorf("no refs should add no scope annotations; got %v", extra)
	}
}

func TestEnforceReviewerScopeOverlap_Issue379DriftDemotesGo(t *testing.T) {
	// The actual drifted diff from foreman/issue-379.
	diff := []string{
		"internal/foreman/controller/agentictask_controller.go",
		"internal/foreman/controller/agentictask_controller_test.go",
	}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, issue379Body, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("zero scope overlap on GO must demote to NO-GO; got %v", got)
	}
	if v, _ := extra["scopeDriftDetected"].(bool); !v {
		t.Errorf("scopeDriftDetected must be true; got %v", extra["scopeDriftDetected"])
	}
	if v, _ := extra["verdictDemoted"].(bool); !v {
		t.Errorf("demotion must set verdictDemoted=true")
	}
	if extra["verdictClaimed"] != string(foremanv1alpha1.AgenticTaskVerdictGo) {
		t.Errorf("verdictClaimed should archive GO; got %v", extra["verdictClaimed"])
	}
	if reason, _ := extra["demotionReason"].(string); reason == "" {
		t.Errorf("demotionReason must explain the demotion")
	}
	refs, _ := extra["scopeRefs"].([]string)
	if len(refs) != 5 {
		t.Errorf("scopeRefs should carry the 5 extracted paths; got %v", refs)
	}
	matched, _ := extra["scopeMatched"].([]string)
	if len(matched) != 0 {
		t.Errorf("scopeMatched should be empty on full drift; got %v", matched)
	}
}

func TestEnforceReviewerScopeOverlap_MatchedRefStands(t *testing.T) {
	diff := []string{"AGENTS.md"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, issue510Body, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("a diff touching a referenced file must not demote; got %v", got)
	}
	if v, _ := extra["scopeDriftDetected"].(bool); v {
		t.Errorf("scopeDriftDetected should be false when a ref matches")
	}
	matched, _ := extra["scopeMatched"].([]string)
	if !reflect.DeepEqual(matched, []string{"AGENTS.md"}) {
		t.Errorf("scopeMatched = %v, want [AGENTS.md]", matched)
	}
}

func TestEnforceReviewerScopeOverlap_BasenameMatch(t *testing.T) {
	// Issue says `role.yaml`-style short ref; diff touches it under its
	// full path. Basename equality must count as a match.
	body := "Update `role.yaml` to include the new verbs."
	diff := []string{"config/rbac/role.yaml"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("basename match must not demote; got %v", got)
	}
}

func TestEnforceReviewerScopeOverlap_NoRefsObserveOnly(t *testing.T) {
	body := "The error message when a model is missing is confusing; improve it."
	diff := []string{"pkg/agent/agent.go"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("no path refs in issue must not demote; got %v", got)
	}
	if _, present := extra["scopeDriftDetected"]; present {
		t.Errorf("no refs should add no scope annotations; got %v", extra)
	}
}

func TestEnforceReviewerScopeOverlap_DriftOnNoGoAnnotatesOnly(t *testing.T) {
	diff := []string{"internal/foreman/controller/agentictask_controller.go"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, issue379Body, diff,
		foremanv1alpha1.AgenticTaskVerdictNoGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("NO-GO must stay NO-GO; got %v", got)
	}
	if v, _ := extra["scopeDriftDetected"].(bool); !v {
		t.Errorf("drift should still be annotated on NO-GO")
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Errorf("a NO-GO verdict was not demoted and must not claim to be")
	}
}

// TestEnforceReviewerScopeOverlap_ZeroGoFilesSkipsScopeCheck verifies that
// a diff with zero indexable Go files is not treated as scope drift (#800).
// A legitimate docs- or YAML-only change should proceed.
func TestEnforceReviewerScopeOverlap_ZeroGoFilesSkipsScopeCheck(t *testing.T) {
	// Issue references Go files; diff contains only non-Go files.
	diff := []string{"README.md", "config/crd/bases/inference.llmkube.dev_models.yaml"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, issue379Body, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("zero-Go-file diff must not demote GO; got %v", got)
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Error("should not claim demotion when zero Go files changed")
	}
}

// TestEnforceReviewerScopeOverlap_RealDriftStillBites verifies that a diff
// with Go files that do not match the issue refs is still flagged as drift
// (#800).
func TestEnforceReviewerScopeOverlap_RealDriftStillBites(t *testing.T) {
	// Issue references Go files; diff contains Go files that don't match.
	diff := []string{"internal/foreman/controller/agentictask_controller.go"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, issue379Body, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, nil)
	if got != foremanv1alpha1.AgenticTaskVerdictNoGo {
		t.Fatalf("real drift with Go files must still demote GO; got %v", got)
	}
	if v, _ := extra["scopeDriftDetected"].(bool); !v {
		t.Error("scopeDriftDetected must be true for real drift")
	}
}

func TestEnforceReviewerScopeOverlap_NilOrEmptyInputsPassThrough(t *testing.T) {
	if got := enforceReviewerScopeOverlap(logr.Discard(), nil, issue379Body,
		[]string{"x.go"}, foremanv1alpha1.AgenticTaskVerdictGo, nil); got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("nil extra must pass through; got %v", got)
	}
	extra := map[string]any{}
	if got := enforceReviewerScopeOverlap(logr.Discard(), extra, "", nil,
		foremanv1alpha1.AgenticTaskVerdictGo, nil); got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("empty body and diff must pass through; got %v", got)
	}
}

func TestScopeMatchHasNonDoc(t *testing.T) {
	tests := []struct {
		name    string
		matched []string
		want    bool
	}{
		{"empty", nil, false},
		{"doc only (md)", []string{"SECURITY_REVIEW.md"}, false},
		{"docs only (mixed doc exts)", []string{"README.md", "notes.rst", "changes.txt"}, false},
		{"source file", []string{"scripts/main.gd"}, true},
		{"config file", []string{"package.json"}, true},
		{"mixed doc + source", []string{"README.md", "pkg/foo.go"}, true},
		{"no extension treated as non-doc", []string{"Makefile"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scopeMatchHasNonDoc(tt.matched); got != tt.want {
				t.Errorf("scopeMatchHasNonDoc(%v) = %v, want %v", tt.matched, got, tt.want)
			}
		})
	}
}

func TestHasSourceFile(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		exts  []string
		want  bool
	}{
		{
			name:  "matches .go",
			paths: []string{"pkg/foo.go"},
			exts:  []string{".go"},
			want:  true,
		},
		{
			name:  "matches .py",
			paths: []string{"src/main.py"},
			exts:  []string{".py"},
			want:  true,
		},
		{
			name:  "no match for unrelated files",
			paths: []string{"README.md", "config/crd/bases/model.yaml"},
			exts:  []string{".go"},
			want:  false,
		},
		{
			name:  "empty exts defaults to .go",
			paths: []string{"pkg/foo.go"},
			exts:  nil,
			want:  true,
		},
		{
			name:  "empty exts defaults to .go - no match",
			paths: []string{"src/main.py"},
			exts:  nil,
			want:  false,
		},
		{
			name:  "multiple extensions",
			paths: []string{"src/main.py"},
			exts:  []string{".go", ".py"},
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasSourceFile(tt.paths, tt.exts); got != tt.want {
				t.Errorf("hasSourceFile(%v, %v) = %v, want %v", tt.paths, tt.exts, got, tt.want)
			}
		})
	}
}

// TestEnforceReviewerScopeOverlap_PythonExtensions verifies that with
// sourceExtensions=[".py"], a diff of only non-.py files (e.g. .md)
// whose paths do NOT match the issue refs skips the scope check
// (returns the input verdict unchanged), because hasSourceFile returns
// false for a diff with no .py files.
func TestEnforceReviewerScopeOverlap_PythonExtensions(t *testing.T) {
	// Issue references Go files; diff contains only .md files (no .py).
	diff := []string{"README.md", "docs/guide.md"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, issue379Body, diff,
		foremanv1alpha1.AgenticTaskVerdictGo, []string{".py"})
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("with .py extensions and no .py diff, scope check should skip; got %v", got)
	}
	if _, demoted := extra["verdictDemoted"]; demoted {
		t.Error("should not claim demotion when no source files of configured type changed")
	}
}
