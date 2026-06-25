/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

// Package audit builds and persists durable, structured records of every
// terminal Foreman run for compliance/audit export. See GitHub issue #837.
package audit

import (
	"encoding/json"
	"time"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// SchemaVersion identifies the on-disk shape of an audit Record.
const SchemaVersion = "foreman.audit.v1"

// Record is one terminal run's audit entry. Optional blocks are nil when
// the run did not produce them (e.g. a coder run has no reviewer block).
type Record struct {
	SchemaVersion     string           `json:"schemaVersion"`
	RecordedAt        string           `json:"recordedAt,omitempty"`
	Task              TaskRef          `json:"task"`
	Repo              string           `json:"repo,omitempty"`
	Issue             int              `json:"issue,omitempty"`
	Agent             *AgentRef        `json:"agent,omitempty"`
	AssignedNode      string           `json:"assignedNode,omitempty"`
	ClaimedAt         string           `json:"claimedAt,omitempty"`
	StartedAt         string           `json:"startedAt,omitempty"`
	FinishedAt        string           `json:"finishedAt,omitempty"`
	ElapsedSec        float64          `json:"elapsedSec,omitempty"`
	Verdict           string           `json:"verdict,omitempty"`
	FailureReason     string           `json:"failureReason,omitempty"`
	SucceededOnTarget bool             `json:"succeededOnTarget"`
	Gate              *GateOutcome     `json:"gate,omitempty"`
	ScopeGuard        *ScopeOutcome    `json:"scopeGuard,omitempty"`
	IssueAsk          *IssueAskOutcome `json:"issueAsk,omitempty"`
	Reviewer          *ReviewerOutcome `json:"reviewer,omitempty"`
	Branch            string           `json:"branch,omitempty"`
	CommitSHA         string           `json:"commitSHA,omitempty"`
	TurnCount         int              `json:"turnCount,omitempty"`
	FilesChanged      []string         `json:"filesChanged,omitempty"`
	TestsAdded        int              `json:"testsAdded,omitempty"`
	TranscriptRef     string           `json:"transcriptRef,omitempty"`
}

// TaskRef identifies the AgenticTask the record describes.
type TaskRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	UID       string `json:"uid,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// AgentRef records which agent/model/endpoint served the run. The Endpoint
// is the inference-provenance proof (where the model traffic went).
type AgentRef struct {
	Name     string `json:"name"`
	Role     string `json:"role,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
}

// GateOutcome is the fast in-workspace gate result. Passed is false when
// the run terminated CODER-GATE-FAILED.
type GateOutcome struct {
	Attempts int  `json:"attempts"`
	Passed   bool `json:"passed"`
}

// ScopeOutcome is the scope-drift guard result.
type ScopeOutcome struct {
	Drift   bool     `json:"drift"`
	Matched []string `json:"matched,omitempty"`
}

// IssueAskOutcome is the reviewer issueAsk verification result.
type IssueAskOutcome struct {
	Verified bool   `json:"verified"`
	Claimed  string `json:"claimed,omitempty"`
}

// ReviewerOutcome is the reviewer's verdict for a review-kind task.
type ReviewerOutcome struct {
	Outcome string `json:"outcome,omitempty"`
}

// resultPayload is the subset of the foreman.v1 Result JSON we read.
type resultPayload struct {
	ElapsedSec float64 `json:"elapsedSec"`
	Extra      struct {
		TurnCount  int `json:"turnCount"`
		ModelExtra struct {
			GateAttempts       int      `json:"gateAttempts"`
			Outcome            string   `json:"outcome"`
			ScopeDriftDetected bool     `json:"scopeDriftDetected"`
			ScopeMatched       []string `json:"scopeMatched"`
			IssueAskVerified   bool     `json:"issueAskVerified"`
			IssueAskClaimed    string   `json:"issueAskClaimed"`
			ReviewOutcome      string   `json:"reviewOutcome"`
			FilesChanged       []string `json:"filesChanged"`
			TestsAdded         int      `json:"testsAdded"`
		} `json:"modelExtra"`
	} `json:"extra"`
}

// BuildRecord maps a terminal AgenticTask (and its resolved Agent, which
// may be nil) to an audit Record. It reads typed status fields plus the
// opaque status.Result JSON. Missing optional data yields nil blocks, never
// fabricated values. RecordedAt is the run's FinishedAt (deterministic).
func BuildRecord(task *foremanv1alpha1.AgenticTask, agent *foremanv1alpha1.Agent) Record {
	rec := Record{
		SchemaVersion: SchemaVersion,
		Task: TaskRef{
			Name:      task.Name,
			Namespace: task.Namespace,
			UID:       string(task.UID),
			Kind:      string(task.Spec.Kind),
		},
		Repo:              task.Spec.Payload.Repo,
		Issue:             int(task.Spec.Payload.Issue),
		AssignedNode:      task.Status.AssignedNode,
		Verdict:           string(task.Status.Verdict),
		FailureReason:     string(task.Status.FailureReason),
		SucceededOnTarget: task.SucceededOnTarget(),
		Branch:            task.Status.Branch,
		CommitSHA:         task.Status.CommitSHA,
		TranscriptRef:     task.Status.TranscriptRef,
	}
	if task.Status.ClaimedAt != nil {
		rec.ClaimedAt = task.Status.ClaimedAt.UTC().Format(time.RFC3339)
	}
	if task.Status.StartedAt != nil {
		rec.StartedAt = task.Status.StartedAt.UTC().Format(time.RFC3339)
	}
	if task.Status.FinishedAt != nil {
		rec.FinishedAt = task.Status.FinishedAt.UTC().Format(time.RFC3339)
		rec.RecordedAt = rec.FinishedAt
	}
	if agent != nil {
		rec.Agent = &AgentRef{
			Name:     agent.Name,
			Role:     string(agent.Spec.Role),
			Model:    agent.Spec.Model,
			Provider: string(agent.Spec.Provider),
		}
		if agent.Spec.ProviderConfig != nil {
			rec.Agent.Endpoint = agent.Spec.ProviderConfig.BaseURL
		}
	}
	if task.Status.Result != nil && len(task.Status.Result.Raw) > 0 {
		var rp resultPayload
		if err := json.Unmarshal(task.Status.Result.Raw, &rp); err == nil {
			rec.ElapsedSec = rp.ElapsedSec
			rec.TurnCount = rp.Extra.TurnCount
			me := rp.Extra.ModelExtra
			rec.FilesChanged = me.FilesChanged
			rec.TestsAdded = me.TestsAdded
			if me.GateAttempts > 0 || me.Outcome != "" {
				rec.Gate = &GateOutcome{Attempts: me.GateAttempts, Passed: me.Outcome != "CODER-GATE-FAILED"}
			}
			if me.ScopeDriftDetected || len(me.ScopeMatched) > 0 {
				rec.ScopeGuard = &ScopeOutcome{Drift: me.ScopeDriftDetected, Matched: me.ScopeMatched}
			}
			if me.ReviewOutcome != "" || me.IssueAskClaimed != "" {
				rec.IssueAsk = &IssueAskOutcome{Verified: me.IssueAskVerified, Claimed: me.IssueAskClaimed}
				rec.Reviewer = &ReviewerOutcome{Outcome: me.ReviewOutcome}
			}
		}
	}
	return rec
}
