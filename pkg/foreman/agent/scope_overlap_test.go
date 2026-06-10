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
	got := extractIssuePathRefs(issue379Body)
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
	got := extractIssuePathRefs(issue510Body)
	want := []string{"AGENTS.md", "CONTRIBUTING.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractIssuePathRefs(#510) = %v, want %v", got, want)
	}
}

func TestExtractIssuePathRefs_IgnoresCommandsAndAPIGroups(t *testing.T) {
	body := "Run `make manifests` then check `core/endpoints` and `kubebuilder:rbac` " +
		"plus `discovery.k8s.io/v1.EndpointSlice` and plain words."
	if got := extractIssuePathRefs(body); len(got) != 0 {
		t.Errorf("commands, API groups, and identifiers must not extract as path refs; got %v", got)
	}
}

func TestExtractIssuePathRefs_EmptyBody(t *testing.T) {
	if got := extractIssuePathRefs(""); len(got) != 0 {
		t.Errorf("empty body should yield no refs; got %v", got)
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
		foremanv1alpha1.AgenticTaskVerdictGo)
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
		foremanv1alpha1.AgenticTaskVerdictGo)
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
		foremanv1alpha1.AgenticTaskVerdictGo)
	if got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Fatalf("basename match must not demote; got %v", got)
	}
}

func TestEnforceReviewerScopeOverlap_NoRefsObserveOnly(t *testing.T) {
	body := "The error message when a model is missing is confusing; improve it."
	diff := []string{"pkg/agent/agent.go"}
	extra := map[string]any{}
	got := enforceReviewerScopeOverlap(logr.Discard(), extra, body, diff,
		foremanv1alpha1.AgenticTaskVerdictGo)
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
		foremanv1alpha1.AgenticTaskVerdictNoGo)
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

func TestEnforceReviewerScopeOverlap_NilOrEmptyInputsPassThrough(t *testing.T) {
	if got := enforceReviewerScopeOverlap(logr.Discard(), nil, issue379Body,
		[]string{"x.go"}, foremanv1alpha1.AgenticTaskVerdictGo); got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("nil extra must pass through; got %v", got)
	}
	extra := map[string]any{}
	if got := enforceReviewerScopeOverlap(logr.Discard(), extra, "", nil,
		foremanv1alpha1.AgenticTaskVerdictGo); got != foremanv1alpha1.AgenticTaskVerdictGo {
		t.Errorf("empty body and diff must pass through; got %v", got)
	}
}
