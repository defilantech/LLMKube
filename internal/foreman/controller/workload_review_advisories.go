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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// patchReviewAdvisories wires gate advisories from completed coder tasks into
// their corresponding pending review tasks. It is called on every second-pass
// reconcile (when children already exist), so it is idempotent: once a review
// task's GateAdvisories field is non-empty the patch is skipped.
//
// For each issue N in the issue-batch pipeline:
//  1. Find the "code-N" child whose status.result carries gateAdvisories.
//  2. Unmarshal the JSON array into []foremanv1alpha1.GateAdvisory.
//  3. For every "review-N-*" or "escalate-N-*" child that is still Pending
//     and has no GateAdvisories set, patch spec.payload.gateAdvisories.
//
// Non-fatal: a single patch failure logs a warning and continues so the
// remaining review tasks still receive their advisories. This mirrors the
// graceful-degradation pattern in emitEscalations.
//
// Pure issue-batch concern: explicit Pipeline-mode workloads do not use the
// "code-N" / "review-N-*" naming conventions, so we bail out early when
// len(w.Spec.Pipeline) > 0.
func (r *WorkloadReconciler) patchReviewAdvisories(
	ctx context.Context, w *foremanv1alpha1.Workload, children []foremanv1alpha1.AgenticTask,
) {
	if len(w.Spec.Pipeline) > 0 {
		return
	}
	log := logf.FromContext(ctx).WithName("workload").WithValues("workload", client.ObjectKeyFromObject(w))

	// Index children by step label for O(1) lookup.
	byStep := make(map[string]*foremanv1alpha1.AgenticTask, len(children))
	for i := range children {
		step := children[i].Labels[labelStep]
		if step != "" {
			byStep[step] = &children[i]
		}
	}

	for _, n := range w.Spec.Issues {
		codeStep := fmt.Sprintf("code-%d", n)
		codeTask, ok := byStep[codeStep]
		if !ok {
			continue
		}

		advisories := extractGateAdvisories(codeTask)
		if len(advisories) == 0 {
			continue
		}

		// Patch every review/escalation task for this issue that is still
		// Pending and does not yet carry advisories.
		reviewPrefix := fmt.Sprintf("review-%d-", n)
		escalatePrefix := fmt.Sprintf("escalate-%d-", n)
		for i := range children {
			step := children[i].Labels[labelStep]
			if !strings.HasPrefix(step, reviewPrefix) && !strings.HasPrefix(step, escalatePrefix) {
				continue
			}
			reviewTask := &children[i]
			if reviewTask.Status.Phase != foremanv1alpha1.AgenticTaskPhasePending &&
				reviewTask.Status.Phase != "" {
				// Already claimed or running; do not mutate spec mid-flight.
				continue
			}
			if len(reviewTask.Spec.Payload.GateAdvisories) > 0 {
				// Already patched on a previous reconcile.
				continue
			}
			patch := client.MergeFrom(reviewTask.DeepCopy())
			reviewTask.Spec.Payload.GateAdvisories = advisories
			if err := r.Patch(ctx, reviewTask, patch); err != nil {
				log.Error(err, "could not patch review task advisories; skipping",
					"task", reviewTask.Name)
			}
		}
	}
}

// extractGateAdvisories unmarshals the gateAdvisories array from a coder
// task's status.result. Returns nil when the task has no result, the result
// is not valid JSON, or gateAdvisories is absent or empty.
func extractGateAdvisories(task *foremanv1alpha1.AgenticTask) []foremanv1alpha1.GateAdvisory {
	if task.Status.Result == nil || len(task.Status.Result.Raw) == 0 {
		return nil
	}

	// Unmarshal only the fields we need from the opaque result JSON.
	var envelope struct {
		Extra map[string]json.RawMessage `json:"extra"`
	}
	if err := json.Unmarshal(task.Status.Result.Raw, &envelope); err != nil {
		return nil
	}
	raw, ok := envelope.Extra["gateAdvisories"]
	if !ok || len(raw) == 0 {
		return nil
	}

	var advisories []foremanv1alpha1.GateAdvisory
	if err := json.Unmarshal(raw, &advisories); err != nil {
		return nil
	}
	if len(advisories) == 0 {
		return nil
	}
	return advisories
}
