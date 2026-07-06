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

// Verdict-from-findings rail: the mirror of the grounded-finding demote rail.
// The demote rail turns an ungrounded NO-GO into GO; this rail turns a GO into
// NO-GO when the reviewer emitted a GROUNDED blocking finding (severity
// blocker/major with file+line inside a changed hunk) yet still voted GO.
// Together the two rails make the reviewer verdict a deterministic function of
// the grounded blocking findings: NO-GO iff at least one exists, else GO.
//
// Motivation (2026-07-06 Nemotron isolation run): the model emitted a correct
// finding ("CLI cache commands don't replicate controller's metal model
// scoping") but voted GO. The model's verdict cannot be trusted; the grounded
// findings can.
//
// Disabled by FOREMAN_VERDICT_FROM_FINDINGS=0.
package agent

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
)

// verdictFromFindingsDisabled reports whether the promote rail is off via
// FOREMAN_VERDICT_FROM_FINDINGS=0. Default (unset) is enabled.
func verdictFromFindingsDisabled() bool {
	return os.Getenv("FOREMAN_VERDICT_FROM_FINDINGS") == "0"
}

// enforceReviewerVerdictFromFindings promotes a model GO to NO-GO when it
// carries at least one grounded blocking finding. No-op on any non-GO verdict,
// when disabled, or when changedLines is nil (git unavailable -> degrade-open).
func enforceReviewerVerdictFromFindings(
	log logr.Logger,
	extra map[string]any,
	verdict foremanv1alpha1.AgenticTaskVerdict,
	changedLines func(file string) map[int]bool,
) foremanv1alpha1.AgenticTaskVerdict {
	if verdictFromFindingsDisabled() ||
		verdict != foremanv1alpha1.AgenticTaskVerdictGo ||
		extra == nil || changedLines == nil {
		return verdict
	}

	findings, _ := reviewer.ParseFindings(extra)
	grounded, _ := groundedBlockingFindings(findings, changedLines)
	if len(grounded) == 0 {
		return verdict // no grounded blocker; the GO stands
	}

	// A grounded blocking finding present under a GO verdict is a
	// found-it-but-approved inconsistency. Promote to NO-GO and archive the
	// findings that forced it, mirroring the demote rail's transparency.
	extra["verdictPromotedFromFindings"] = true
	extra["promotingFindings"] = grounded
	extra["verdictFromFindingsReason"] = fmt.Sprintf(
		"GO promoted to NO-GO: %d grounded blocking finding(s) present", len(grounded))
	log.Info("reviewer verdict-from-findings: GO promoted to NO-GO; grounded blocking finding present",
		"groundedCount", len(grounded))
	return foremanv1alpha1.AgenticTaskVerdictNoGo
}
