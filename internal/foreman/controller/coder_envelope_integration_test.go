/*
Copyright 2026.

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

package controller

// Integration-shaped regression tests for #1077: drive a REAL agent-package
// Result envelope (marshaled exactly as the watcher stores it in
// status.result) into this package's escalation classifiers, instead of
// hand-building synthetic JSON. This is the cross-package contract the
// original bug slipped through: the executor nested the machine outcome
// under modelExtra while the classifiers read the top level, and every
// existing test constructed its own envelope on the side it was testing.

import (
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
)

// realEnvelopeTask marshals an actual agent.Result into an AgenticTask the
// way the watcher persists it, so the classifiers read the true wire shape.
func realEnvelopeTask(t *testing.T, r *foremanagent.Result) *foremanv1alpha1.AgenticTask {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal real envelope: %v", err)
	}
	task := &foremanv1alpha1.AgenticTask{}
	task.Status.Phase = foremanv1alpha1.AgenticTaskPhaseSucceeded
	task.Status.Verdict = r.Verdict
	task.Status.Result = &runtime.RawExtension{Raw: raw}
	return task
}

func TestRealAgentEnvelope_DrivesClassifiers(t *testing.T) {
	t.Run("promoted ALREADY-RESOLVED stands escalation down", func(t *testing.T) {
		r := foremanagent.NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictNoGo,
			"already fixed by abc1234", 5*time.Second)
		// The shape modelDecidedResult + promoteTerminalOutcome (and, post
		// #1077, coderJobResultToResult) produce for this outcome.
		r.Extra = map[string]any{
			"outcome":    "ALREADY-RESOLVED",
			"resolvedBy": "abc1234",
			"modelExtra": map[string]any{"outcome": "ALREADY-RESOLVED", "resolvedBy": "abc1234"},
		}
		task := realEnvelopeTask(t, r)
		if !isAlreadyResolvedCoder(task) {
			t.Fatal("real ALREADY-RESOLVED envelope not recognized by isAlreadyResolvedCoder")
		}
		v, top, model := coderTerminalOutcome(task)
		if shouldEscalateCoder(v, top, model) {
			t.Fatal("ALREADY-RESOLVED must not escalate")
		}
	})

	t.Run("promoted NEEDS-VERIFICATION stands escalation down", func(t *testing.T) {
		r := foremanagent.NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictNoGo,
			"claims unverified", 5*time.Second)
		r.Extra = map[string]any{
			"outcome":    "NEEDS-VERIFICATION",
			"unverified": []any{"claim A"},
			"modelExtra": map[string]any{"outcome": "NEEDS-VERIFICATION"},
		}
		task := realEnvelopeTask(t, r)
		if !isNeedsVerificationCoder(task) {
			t.Fatal("real NEEDS-VERIFICATION envelope not recognized")
		}
		v, top, model := coderTerminalOutcome(task)
		if shouldEscalateCoder(v, top, model) {
			t.Fatal("NEEDS-VERIFICATION must not escalate")
		}
	})

	t.Run("nested-only outcome (the #1077 bug shape) escalates: regression canary", func(t *testing.T) {
		// If a future producer regresses to nesting the outcome without
		// promotion, the classifiers must treat it as a generic NO-GO and
		// escalate. This pins the reason the promotion must exist at the
		// producer, and fails loudly if someone "fixes" the classifiers to
		// read modelExtra instead (the documented contract is top-level).
		r := foremanagent.NewResult("issue-fix", foremanv1alpha1.AgenticTaskVerdictNoGo,
			"already fixed", 5*time.Second)
		r.Extra = map[string]any{
			"outcome":    "MODEL-DECIDED",
			"modelExtra": map[string]any{"outcome": "ALREADY-RESOLVED"},
		}
		task := realEnvelopeTask(t, r)
		if isAlreadyResolvedCoder(task) {
			t.Fatal("nested-only outcome must NOT classify as already-resolved (top-level is the contract)")
		}
		v, top, model := coderTerminalOutcome(task)
		if !shouldEscalateCoder(v, top, model) {
			t.Fatal("generic MODEL-DECIDED NO-GO should escalate")
		}
	})
}
