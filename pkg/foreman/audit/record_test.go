/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package audit

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

func coderTask() *foremanv1alpha1.AgenticTask {
	loc := metav1.Now().Location()
	finished := metav1.Date(2026, 6, 25, 1, 29, 0, 0, loc)
	started := metav1.Date(2026, 6, 25, 0, 39, 20, 0, loc)
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "coder-89", Namespace: "default", UID: types.UID("uid-1")},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Kind:     foremanv1alpha1.AgenticTaskKindIssueFix,
			AgentRef: &corev1.LocalObjectReference{Name: "gateway-coder-amd"},
			Payload:  foremanv1alpha1.AgenticTaskPayload{Repo: "defilantech/LLMKube", Issue: 89},
		},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase:         foremanv1alpha1.AgenticTaskPhaseSucceeded,
			AssignedNode:  "m5max-coder",
			StartedAt:     &started,
			FinishedAt:    &finished,
			Verdict:       foremanv1alpha1.AgenticTaskVerdictGo,
			Branch:        "foreman/issue-89",
			CommitSHA:     "71f4343",
			TranscriptRef: "foreman-transcript-coder-89",
			Result: &runtime.RawExtension{Raw: []byte(`{
				"verdict":"GO","elapsedSec":2980,
				"extra":{"branch":"foreman/issue-89","commitSHA":"71f4343","turnCount":38,
					"modelExtra":{"gateAttempts":1,"outcome":"","scopeDriftDetected":false,
						"scopeMatched":["internal/router"],"filesChanged":["internal/router/pure_test.go"],
						"testsAdded":35}}}`)},
		},
	}
}

func coderAgent() *foremanv1alpha1.Agent {
	return &foremanv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "gateway-coder-amd", Namespace: "default"},
		Spec: foremanv1alpha1.AgentSpec{
			Role:           foremanv1alpha1.AgentRoleCoder,
			Model:          "coder-amd",
			Provider:       foremanv1alpha1.AgentProviderCloudProxy,
			ProviderConfig: &foremanv1alpha1.ProviderConfig{BaseURL: "http://gw/v1"},
		},
	}
}

func TestBuildRecordCoder(t *testing.T) {
	rec := BuildRecord(coderTask(), coderAgent())

	if rec.SchemaVersion != SchemaVersion {
		t.Errorf("schemaVersion = %q, want %q", rec.SchemaVersion, SchemaVersion)
	}
	if rec.Task.Name != "coder-89" || rec.Task.Namespace != "default" || rec.Task.Kind != "issue-fix" {
		t.Errorf("task ref wrong: %+v", rec.Task)
	}
	if rec.Repo != "defilantech/LLMKube" || rec.Issue != 89 {
		t.Errorf("repo/issue wrong: %q #%d", rec.Repo, rec.Issue)
	}
	if rec.Agent == nil || rec.Agent.Model != "coder-amd" ||
		rec.Agent.Endpoint != "http://gw/v1" || rec.Agent.Role != "coder" {
		t.Errorf("agent ref wrong: %+v", rec.Agent)
	}
	if rec.Verdict != "GO" || !rec.SucceededOnTarget {
		t.Errorf("verdict/onTarget wrong: %q %v", rec.Verdict, rec.SucceededOnTarget)
	}
	if rec.RecordedAt == "" || rec.FinishedAt == "" {
		t.Errorf("timestamps unset: recordedAt=%q finishedAt=%q", rec.RecordedAt, rec.FinishedAt)
	}
	if rec.Gate == nil || rec.Gate.Attempts != 1 || !rec.Gate.Passed {
		t.Errorf("gate wrong: %+v", rec.Gate)
	}
	if rec.ScopeGuard == nil || rec.ScopeGuard.Drift || len(rec.ScopeGuard.Matched) != 1 {
		t.Errorf("scope wrong: %+v", rec.ScopeGuard)
	}
	if rec.TurnCount != 38 || rec.TestsAdded != 35 || len(rec.FilesChanged) != 1 {
		t.Errorf("effort fields wrong: turns=%d tests=%d files=%v", rec.TurnCount, rec.TestsAdded, rec.FilesChanged)
	}
	if rec.IssueAsk != nil || rec.Reviewer != nil {
		t.Errorf("coder task must not carry reviewer/issueAsk blocks: %+v %+v", rec.IssueAsk, rec.Reviewer)
	}
}

func TestBuildRecordNilAgentAndNoResult(t *testing.T) {
	task := coderTask()
	task.Spec.AgentRef = nil
	task.Status.Result = nil
	rec := BuildRecord(task, nil)
	if rec.Agent != nil {
		t.Errorf("nil agent should produce no agent block, got %+v", rec.Agent)
	}
	if rec.Gate != nil || rec.ScopeGuard != nil {
		t.Errorf("nil result should produce no rail blocks")
	}
	if rec.Task.Name != "coder-89" {
		t.Errorf("task ref still required, got %+v", rec.Task)
	}
}
