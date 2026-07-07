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

// Grounded-finding rail: the mirror image of the scope-overlap / issueAsk
// rails. Those demote a too-lenient GO to NO-GO; this demotes an UNGROUNDED
// NO-GO to GO. A reviewer must EARN a rejection by pointing at code the diff
// actually changed: at least one blocking finding (severity blocker/major)
// must cite a File + Line inside a `git diff -U0 main...HEAD` hunk. A NO-GO
// with zero grounded blocking findings is demoted to GO and the rejected
// findings are archived under extra["ungroundedFindings"].
//
// Motivation (2026-07-06 Nemotron-49B experiments): a checklist prompt
// false-approved and fabricated a finding citing a file the diff never
// touched; an adversarial prompt false-rejected citing an invented
// requirement pinned to a PRE-EXISTING line. Both objections were not
// anchored to changed code. This rail makes the reviewer TRUSTWORTHY (cannot
// block on inventions), not smarter (finding the real defect is the
// stronger-model / panel problem).
//
// Disabled by FOREMAN_GROUNDED_FINDINGS=0.
package agent

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/reviewer"
)

// groundedFindingsDisabled reports whether the grounded-finding rail is off
// via FOREMAN_GROUNDED_FINDINGS=0. Default (unset) is enabled.
func groundedFindingsDisabled() bool {
	return os.Getenv("FOREMAN_GROUNDED_FINDINGS") == "0"
}

// groundedBlockingFindings partitions the blocking findings (severity blocker
// or major; minor is advisory and excluded) into those grounded in a changed
// line and those not. A finding is grounded iff File is non-empty, Line > 0,
// and Line is in changedLines(File) (a new-file line inside a diff hunk).
// changedLines results are cached per file since it may shell out to git.
// Both the grounded-finding demote rail and the verdict-from-findings promote
// rail key on this partition, so they share one grounding definition.
func groundedBlockingFindings(
	findings []reviewer.Finding,
	changedLines func(file string) map[int]bool,
) (grounded, ungrounded []reviewer.Finding) {
	cache := map[string]map[int]bool{}
	lines := func(f string) map[int]bool {
		if m, ok := cache[f]; ok {
			return m
		}
		m := changedLines(f)
		cache[f] = m
		return m
	}
	for _, f := range findings {
		if f.Severity != reviewer.SeverityBlocker && f.Severity != reviewer.SeverityMajor {
			continue // minor is advisory, never blocking
		}
		if f.File != "" && f.Line > 0 && lines(f.File)[f.Line] {
			grounded = append(grounded, f)
		} else {
			ungrounded = append(ungrounded, f)
		}
	}
	return grounded, ungrounded
}

// enforceReviewerGroundedFindings demotes a model NO-GO to GO when none of its
// blocking findings cite a changed line. Returns the (possibly demoted)
// verdict. It is a no-op on any non-NO-GO verdict, when disabled, or when
// changedLines is nil (git unavailable -> degrade-open).
//
// changedLines(file) returns the set of new-file line numbers inside a changed
// hunk for that file, or an empty/nil map for a file the diff did not change.
// Because an unchanged file yields an empty set, the single membership test
// (line in changedLines(file)) subsumes both the fabricated-file case and the
// unchanged-line-in-a-changed-file case.
//
// REJECT (do-not-retry) is exempt from demotion: a wrong-issue rejection
// typically cannot cite a changed line (the defect is what is absent), so
// it would be demoted to GO unless scope-overlap or issueAsk independently
// re-flag it. A do-not-retry rejection should not be overturnable by the
// absence of a line-grounded finding.
func enforceReviewerGroundedFindings(
	log logr.Logger,
	extra map[string]any,
	verdict foremanv1alpha1.AgenticTaskVerdict,
	changedLines func(file string) map[int]bool,
) foremanv1alpha1.AgenticTaskVerdict {
	if groundedFindingsDisabled() ||
		verdict != foremanv1alpha1.AgenticTaskVerdictNoGo ||
		extra == nil || changedLines == nil {
		return verdict
	}

	// REJECT (do-not-retry) is exempt from grounded demotion.
	if reviewOutcome, ok := extra["reviewOutcome"].(string); ok && reviewOutcome == "REJECT" {
		return verdict
	}

	findings, _ := reviewer.ParseFindings(extra)
	grounded, ungrounded := groundedBlockingFindings(findings, changedLines)

	if len(grounded) >= 1 {
		return verdict // rejection earned; NO-GO stands
	}

	// Zero grounded blocking findings: the rejection is ungrounded. Demote to
	// GO and archive, mirroring the filesTouchedClaimed transparency pattern.
	blockingCount := len(grounded) + len(ungrounded)
	extra["groundedFindingDemotion"] = true
	extra["ungroundedFindings"] = ungrounded
	extra["groundedFindingReason"] = fmt.Sprintf(
		"NO-GO demoted to GO: 0 of %d blocking finding(s) cite a changed line", blockingCount)
	log.Info("reviewer grounded-finding: NO-GO demoted to GO; no blocking finding cites a changed line",
		"blockingCount", blockingCount)
	return foremanv1alpha1.AgenticTaskVerdictGo
}
