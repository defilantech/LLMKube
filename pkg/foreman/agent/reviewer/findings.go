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

// Package reviewer codifies the structured-findings shape the v0.4
// reviewer Agent emits via submit_result.extra. The schema is
// documented in config/foreman/system-prompts/reviewer.md; this
// package keeps the Go and prompt definitions in lockstep so a future
// downstream consumer (GitHub PR-comment poster, analytics, audit
// trail) reads from one place.
//
// Validation is intentionally lenient: a malformed findings payload
// logs a warning and is dropped, but does not change the reviewer's
// verdict. The verdict (GO / NO-GO / ERROR) is the authoritative
// signal; findings enrich it. The cascade rule (defilantech/LLMKube
// #541) gates on verdict, not on findings.
package reviewer

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Severity classifies the urgency of a single review finding. The
// reviewer prompt uses three values; we accept them case-insensitively
// because models vary on capitalization.
type Severity string

const (
	// SeverityBlocker means "this diff should not merge as-is, full
	// stop." Reviewers use this for scope drift, removed working
	// logic, or any defect that would regress production behavior.
	SeverityBlocker Severity = "blocker"
	// SeverityMajor means "this needs a fix before merge but the
	// shape of the diff is correct." Reviewers use this for missing
	// tests, mismatched docs, or unjustified magic numbers.
	SeverityMajor Severity = "major"
	// SeverityMinor means "consider this in a follow-up." Reviewers
	// use this for style nits, naming preferences, or optional
	// docs improvements.
	SeverityMinor Severity = "minor"
)

// validSeverities is the exhaustive set normalize() will accept.
// Anything outside this set produces a validation error and the
// finding is dropped.
var validSeverities = map[Severity]struct{}{
	SeverityBlocker: {},
	SeverityMajor:   {},
	SeverityMinor:   {},
}

// severitySynonyms maps common non-canonical severity labels local reviewer
// models emit onto the canonical set. Without this, a real blocker labeled
// e.g. "critical" or "high" fails validSeverities and is dropped by
// normalize(); the grounded-finding rail then sees zero blocking findings and
// demotes a genuine NO-GO to GO. Only unambiguous synonyms are mapped;
// genuinely ambiguous labels (e.g. "medium") still fail validation.
var severitySynonyms = map[Severity]Severity{
	"critical": SeverityBlocker,
	"crit":     SeverityBlocker,
	"fatal":    SeverityBlocker,
	"severe":   SeverityBlocker,
	"blocking": SeverityBlocker,
	"high":     SeverityMajor,
	"error":    SeverityMajor,
	"warning":  SeverityMinor,
	"warn":     SeverityMinor,
	"low":      SeverityMinor,
	"info":     SeverityMinor,
	"nit":      SeverityMinor,
	"trivial":  SeverityMinor,
}

// Area names the review-checklist section the finding belongs to.
// Tracks the section headers in reviewer.md (A. Scope alignment, B.
// Change is minimal and idiomatic, etc.) so downstream filtering /
// reporting can roll up by category.
type Area string

const (
	AreaScope            Area = "scope"              // Section A: scope alignment
	AreaStyle            Area = "style"              // Section B: minimal / idiomatic
	AreaTests            Area = "tests"              // Section C: meaningful tests
	AreaSideEffects      Area = "side-effects"       // Section D: side effects
	AreaDocs             Area = "docs"               // Section E: documentation / ergonomics
	AreaRegression       Area = "regression"         // Section F: removed working logic
	AreaDocConsist       Area = "doc-consistency"    // Section G: doc vs code mismatch
	AreaConstants        Area = "constants"          // Section H: magic-number justification
	AreaTestProdFidelity Area = "test-prod-fidelity" // Section I: real values, not placeholders
	AreaWiredUp          Area = "wired-up"           // Section J: wired-up, not inert
)

// validAreas is the exhaustive set. Models occasionally invent areas
// (e.g. "security", "performance"); we drop unknown areas with a
// warning rather than failing the parse, because a finding with a
// bad area is still useful to a human reading the review.
var validAreas = map[Area]struct{}{
	AreaScope: {}, AreaStyle: {}, AreaTests: {}, AreaSideEffects: {},
	AreaDocs: {}, AreaRegression: {}, AreaDocConsist: {}, AreaConstants: {},
	AreaTestProdFidelity: {}, AreaWiredUp: {},
}

// Finding is one item in the reviewer's structured-findings list.
// File and Line are optional but strongly encouraged: a finding the
// reviewer can pin to a specific source location is much more useful
// downstream than a free-form complaint.
type Finding struct {
	// Severity is required. Empty / unknown values cause the finding
	// to be dropped with a warning.
	Severity Severity `json:"severity"`
	// Area is required. Tracks reviewer.md section headers.
	Area Area `json:"area"`
	// Message is the finding text. Required, non-empty.
	Message string `json:"message"`
	// File is the workspace-relative path the finding cites, when
	// applicable. Optional but strongly preferred.
	File string `json:"file,omitempty"`
	// Line is a 1-based line number in File. Zero means "not pinned
	// to a specific line." Use a range like "10-25" via Message
	// when a finding spans multiple lines.
	Line int `json:"line,omitempty"`
	// Suggestion is an optional fix the reviewer proposes. Free-form
	// text the model can quote or paraphrase from the issue body.
	Suggestion string `json:"suggestion,omitempty"`
}

// ParseFindings extracts the findings list from a reviewer's
// submit_result.extra payload. The expected shape is:
//
//	{"findings": [{"severity":"major","area":"scope","message":"..."}]}
//
// Returns the parsed findings (only the valid ones) plus a slice of
// warning strings describing dropped findings. A nil extra map or a
// missing/empty findings key returns (nil, nil) without error.
//
// Lenient by design: a malformed findings array does not fail the
// whole reviewer task. The verdict remains authoritative; this
// function exists so downstream consumers can light up structured
// findings when present and gracefully degrade when not.
func ParseFindings(extra map[string]any) ([]Finding, []string) {
	if len(extra) == 0 {
		return nil, nil
	}
	raw, ok := extra["findings"]
	if !ok {
		return nil, nil
	}
	// Round-trip through JSON to handle both []any (decoded JSON) and
	// []Finding (Go-native callers) uniformly. Marshal+Unmarshal is
	// cheap relative to anything else in the executor's terminal
	// path and keeps the parser shape-agnostic.
	buf, err := json.Marshal(raw)
	if err != nil {
		return nil, []string{fmt.Sprintf("findings: marshal failed: %v", err)}
	}
	var decoded []Finding
	if err := json.Unmarshal(buf, &decoded); err != nil {
		return nil, []string{fmt.Sprintf("findings: unmarshal failed: %v", err)}
	}

	var (
		valid    []Finding
		warnings []string
	)
	for i, f := range decoded {
		ok, reason := f.normalize()
		if !ok {
			warnings = append(warnings, fmt.Sprintf("findings[%d]: %s", i, reason))
			continue
		}
		valid = append(valid, f)
	}
	return valid, warnings
}

// normalize lowercases the severity / area, trims whitespace, and
// validates the required fields. Returns ok=true with the receiver
// mutated in place, or ok=false with a human-readable reason.
//
// Lenient on casing because model output varies ("Major", "MAJOR",
// "major"); strict on emptiness and unknown values because those
// indicate the model did not understand the schema.
func (f *Finding) normalize() (ok bool, reason string) {
	f.Severity = Severity(strings.ToLower(strings.TrimSpace(string(f.Severity))))
	if canon, ok := severitySynonyms[f.Severity]; ok {
		f.Severity = canon
	}
	f.Area = Area(strings.ToLower(strings.TrimSpace(string(f.Area))))
	f.Message = strings.TrimSpace(f.Message)

	if f.Message == "" {
		return false, "empty message"
	}
	if _, ok := validSeverities[f.Severity]; !ok {
		return false, fmt.Sprintf("unknown severity %q", f.Severity)
	}
	if _, ok := validAreas[f.Area]; !ok {
		return false, fmt.Sprintf("unknown area %q", f.Area)
	}
	if f.Line < 0 {
		return false, fmt.Sprintf("negative line number %d", f.Line)
	}
	return true, ""
}

// CountBySeverity returns a map of severity -> count for the input
// findings. Useful for the executor's log line and for future
// metrics / dashboards.
func CountBySeverity(findings []Finding) map[Severity]int {
	counts := make(map[Severity]int, 3)
	for _, f := range findings {
		counts[f.Severity]++
	}
	return counts
}

// HasBlockers reports whether any finding is severity=blocker. The
// reviewer's verdict (NO-GO) is still the authoritative cascade
// signal; this helper is for downstream consumers (e.g. a future
// PR-comment poster) that want to render the worst-severity finding
// prominently.
func HasBlockers(findings []Finding) bool {
	for _, f := range findings {
		if f.Severity == SeverityBlocker {
			return true
		}
	}
	return false
}
