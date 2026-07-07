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

package reviewer

import (
	"encoding/json"
	"testing"
)

// jsonExtra is a test helper: round-trip a JSON string through
// json.Unmarshal into the map[string]any shape ParseFindings expects.
// Mirrors how the executor builds the extra map from submit_result.
func jsonExtra(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("jsonExtra: %v", err)
	}
	return m
}

func TestParseFindings_HappyPath(t *testing.T) {
	extra := jsonExtra(t, `{
		"findings": [
			{"severity":"blocker","area":"scope","message":"diff addresses a different bug","file":"pkg/agent/registry.go"},
			{"severity":"major","area":"regression","message":"removed DNS fallback","file":"pkg/agent/registry.go","line":307},
			{"severity":"minor","area":"docs","message":"godoc claim does not match","suggestion":"update the godoc"}
		]
	}`)
	got, warnings := ParseFindings(extra)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(got) != 3 {
		t.Fatalf("got %d findings; want 3", len(got))
	}
	if got[0].Severity != SeverityBlocker || got[0].Area != AreaScope {
		t.Errorf("first finding: got %+v", got[0])
	}
	if got[1].Line != 307 {
		t.Errorf("second finding: want line 307; got %d", got[1].Line)
	}
	if got[2].Suggestion != "update the godoc" {
		t.Errorf("third finding suggestion not preserved: %q", got[2].Suggestion)
	}
}

func TestParseFindings_NilOrEmpty(t *testing.T) {
	if got, warns := ParseFindings(nil); got != nil || warns != nil {
		t.Errorf("nil extra: want (nil, nil); got (%v, %v)", got, warns)
	}
	if got, warns := ParseFindings(map[string]any{}); got != nil || warns != nil {
		t.Errorf("empty extra: want (nil, nil); got (%v, %v)", got, warns)
	}
	if got, warns := ParseFindings(map[string]any{"unrelated": "value"}); got != nil || warns != nil {
		t.Errorf("no findings key: want (nil, nil); got (%v, %v)", got, warns)
	}
}

// TestParseFindings_DropsInvalidLenient covers the lenient-by-design
// promise: bad rows are dropped with a warning, good rows still
// parse, the verdict (separately on the result envelope) wins
// regardless. Models WILL emit oddly-cased severities or invent area
// names; that should not fail the run.
func TestParseFindings_DropsInvalidLenient(t *testing.T) {
	extra := jsonExtra(t, `{
		"findings": [
			{"severity":"BLOCKER","area":"SCOPE","message":"loud caps still ok"},
			{"severity":"bogus","area":"scope","message":"unknown severity dropped"},
			{"severity":"major","area":"security","message":"unknown area dropped"},
			{"severity":"minor","area":"docs","message":""},
			{"severity":"major","area":"tests","message":"valid one"}
		]
	}`)
	got, warnings := ParseFindings(extra)
	if len(got) != 2 {
		t.Errorf("want 2 valid; got %d (%+v)", len(got), got)
	}
	if len(warnings) != 3 {
		t.Errorf("want 3 warnings; got %d (%v)", len(warnings), warnings)
	}
	// Loud-caps row was normalized successfully.
	if got[0].Severity != SeverityBlocker || got[0].Area != AreaScope {
		t.Errorf("normalize did not lowercase: %+v", got[0])
	}
}

// TestParseFindings_SeveritySynonymsNormalized verifies that common
// non-canonical severity labels (critical/high/warning) are mapped onto the
// canonical set instead of being dropped, so a real blocker labeled "critical"
// still reaches the grounded-finding rail as a blocking finding.
func TestParseFindings_SeveritySynonymsNormalized(t *testing.T) {
	extra := jsonExtra(t, `{
		"findings": [
			{"severity":"critical","area":"scope","message":"crit -> blocker"},
			{"severity":"HIGH","area":"tests","message":"high -> major"},
			{"severity":"warning","area":"style","message":"warning -> minor"}
		]
	}`)
	got, warnings := ParseFindings(extra)
	if len(warnings) != 0 {
		t.Fatalf("synonyms must not warn/drop; got %d: %v", len(warnings), warnings)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 findings; got %d (%+v)", len(got), got)
	}
	if got[0].Severity != SeverityBlocker {
		t.Errorf("critical must map to blocker, got %q", got[0].Severity)
	}
	if got[1].Severity != SeverityMajor {
		t.Errorf("high must map to major, got %q", got[1].Severity)
	}
	if got[2].Severity != SeverityMinor {
		t.Errorf("warning must map to minor, got %q", got[2].Severity)
	}
}

func TestParseFindings_MalformedJSONDoesNotPanic(t *testing.T) {
	// A non-array under "findings": we expect a single warning, no
	// findings returned, no panic.
	extra := map[string]any{"findings": "this should be a list"}
	got, warnings := ParseFindings(extra)
	if got != nil {
		t.Errorf("expected nil findings on malformed input; got %v", got)
	}
	if len(warnings) == 0 {
		t.Errorf("expected a warning on malformed input; got none")
	}
}

func TestParseFindings_NegativeLineRejected(t *testing.T) {
	extra := jsonExtra(t, `{"findings":[{"severity":"major","area":"scope","message":"x","line":-5}]}`)
	got, warnings := ParseFindings(extra)
	if len(got) != 0 {
		t.Errorf("negative line should drop the finding; got %v", got)
	}
	if len(warnings) != 1 {
		t.Errorf("want 1 warning; got %d", len(warnings))
	}
}

func TestCountBySeverity(t *testing.T) {
	findings := []Finding{
		{Severity: SeverityBlocker, Area: AreaScope, Message: "x"},
		{Severity: SeverityMajor, Area: AreaTests, Message: "x"},
		{Severity: SeverityMajor, Area: AreaDocs, Message: "x"},
		{Severity: SeverityMinor, Area: AreaStyle, Message: "x"},
	}
	got := CountBySeverity(findings)
	if got[SeverityBlocker] != 1 || got[SeverityMajor] != 2 || got[SeverityMinor] != 1 {
		t.Errorf("counts: got %v", got)
	}
}

func TestHasBlockers(t *testing.T) {
	noBlockers := make([]Finding, 0, 3)
	noBlockers = append(noBlockers,
		Finding{Severity: SeverityMajor, Area: AreaTests, Message: "x"},
		Finding{Severity: SeverityMinor, Area: AreaStyle, Message: "x"},
	)
	if HasBlockers(noBlockers) {
		t.Error("HasBlockers: expected false on no-blocker list")
	}
	withBlocker := append(noBlockers, Finding{
		Severity: SeverityBlocker, Area: AreaScope, Message: "x",
	})
	if !HasBlockers(withBlocker) {
		t.Error("HasBlockers: expected true when a blocker is present")
	}
}

// TestParseFindings_AllAreasAccepted is a calibration test: every
// area constant the package defines must be acceptable, because the
// reviewer prompt's checklist references all of them. If we add an
// area to validAreas but forget to update the prompt, the diff alone
// won't catch it; if we remove one from validAreas without updating
// the prompt, this test fails.
func TestParseFindings_AllAreasAccepted(t *testing.T) {
	for area := range validAreas {
		t.Run(string(area), func(t *testing.T) {
			extra := map[string]any{
				"findings": []any{
					map[string]any{
						"severity": "major",
						"area":     string(area),
						"message":  "test",
					},
				},
			}
			got, warns := ParseFindings(extra)
			if len(got) != 1 {
				t.Errorf("area %q: want 1 finding; got %d (warns=%v)", area, len(got), warns)
			}
		})
	}
}

// TestParseFindings_TestProdFidelityArea covers the new Section I
// (real values, not placeholders) and Section J (wired-up, not
// inert) areas added in #788. These areas must parse successfully
// and carry the expected area constant.
func TestParseFindings_TestProdFidelityArea(t *testing.T) {
	extra := jsonExtra(t, `{
		"findings": [
			{
				"severity": "blocker",
				"area": "test-prod-fidelity",
				"message": "test uses placeholder runtime mlx-server; production default is llamacpp",
				"file": "pkg/agent/registry_test.go"
			},
			{
				"severity": "major",
				"area": "wired-up",
				"message": "llmkube_inference_ttft_seconds registered but never emitted in production",
				"file": "internal/metrics/metrics.go"
			}
		]
	}`)
	got, warnings := ParseFindings(extra)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(got) != 2 {
		t.Fatalf("got %d findings; want 2", len(got))
	}
	if got[0].Area != AreaTestProdFidelity {
		t.Errorf("first finding area: got %q; want %q", got[0].Area, AreaTestProdFidelity)
	}
	if got[1].Area != AreaWiredUp {
		t.Errorf("second finding area: got %q; want %q", got[1].Area, AreaWiredUp)
	}
}
