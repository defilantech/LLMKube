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

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// StrReplaceTool performs an exact-match string replacement in a file.
// Semantics mirror Claude Code's str_replace_editor "string_replace"
// command: by default the old_string must occur exactly once. The
// optional expected_replacements field lets the model say "replace all
// N occurrences" and the tool enforces the count.
type StrReplaceTool struct {
	Workspace string
}

type strReplaceArgs struct {
	Path                 string `json:"path"`
	OldString            string `json:"old_string"`
	NewString            string `json:"new_string"`
	ExpectedReplacements int    `json:"expected_replacements"`
}

// Name returns the tool name as advertised to the model.
func (t *StrReplaceTool) Name() string { return "str_replace" }

// Schema returns the OAI schema advertisement.
func (t *StrReplaceTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "str_replace",
		Description: "Replace exact text in a file. Default: old_string must occur " +
			"exactly once. Set expected_replacements to N to require exactly N matches. " +
			"Copy old_string VERBATIM from a recent read_file; prefer the shortest " +
			"unique snippet (1-3 lines). Do not retype it from memory. If it does not " +
			"match, the error shows the file's actual current text near your edit; " +
			"copy that exactly and retry.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "path":                  {"type": "string"},
  "old_string":            {"type": "string"},
  "new_string":            {"type": "string"},
  "expected_replacements": {"type": "integer", "minimum": 1}
},
"required": ["path", "old_string", "new_string"]
}`),
	}
}

// Execute reads the file, asserts the expected count of old_string
// matches, replaces all of them, and writes the file back.
func (t *StrReplaceTool) Execute(_ context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a strReplaceArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("str_replace: bad args: %w", err)
	}
	if a.Path == "" || a.OldString == "" {
		return nil, fmt.Errorf("str_replace: path and old_string are required")
	}
	full, err := resolveInside(t.Workspace, a.Path)
	if err != nil {
		return nil, fmt.Errorf("str_replace: %w", err)
	}
	raw, err := os.ReadFile(full) //nolint:gosec // G304: path is resolveInside-validated
	if err != nil {
		return nil, fmt.Errorf("str_replace: read %q: %w", a.Path, err)
	}
	content := string(raw)
	want := a.ExpectedReplacements
	if want == 0 {
		want = 1
	}
	occurrences := strings.Count(content, a.OldString)
	if occurrences != want {
		// Recovery is only attempted for the default single-replace case where
		// the model's old_string did not match at all. This is the dominant
		// failure mode for models that retype old_string from memory instead of
		// copying it verbatim (whitespace drift, or a fabricated near-miss).
		if want == 1 && occurrences == 0 {
			if recovered, note, ok := t.applyWhitespaceMatch(content, a.OldString, a.NewString); ok {
				if err := os.WriteFile(full, []byte(recovered), 0o644); err != nil { //nolint:gosec // G306: workspace file
					return nil, fmt.Errorf("str_replace: write %q: %w", a.Path, err)
				}
				return &agent.ToolResult{
					Output: map[string]any{"path": a.Path, "replacements": 1, "note": note},
				}, nil
			}
			// Whitespace normalization only equalizes whitespace; a drifted
			// TOKEN (renamed identifier, paraphrased comment) falls through to
			// here. The fuzzy window recovers the unique near-miss case that
			// otherwise stalls local coder models into EditFreeStreak (#942).
			if fuzzyEnabled() {
				if recovered, note, ok := t.applyFuzzyMatch(content, a.OldString, a.NewString); ok {
					if err := os.WriteFile(full, []byte(recovered), 0o644); err != nil { //nolint:gosec // G306: workspace file
						return nil, fmt.Errorf("str_replace: write %q: %w", a.Path, err)
					}
					return &agent.ToolResult{
						Output: map[string]any{"path": a.Path, "replacements": 1, "note": note},
					}, nil
				}
			}
			// Could not safely locate the edit. Surface the file's ACTUAL current
			// text near the model's intended anchor so it can retry against
			// truth instead of re-hallucinating old_string.
			if actual, ok := anchorContext(content, a.OldString); ok {
				return nil, fmt.Errorf("str_replace: old_string not found in %q. "+
					"The file's actual current text near your edit is below - copy it "+
					"VERBATIM into old_string and retry:\n%s", a.Path, actual)
			}
		}
		return nil, fmt.Errorf("str_replace: old_string found %d times in %q, want %d", occurrences, a.Path, want)
	}
	next := strings.ReplaceAll(content, a.OldString, a.NewString)
	if err := os.WriteFile(full, []byte(next), 0o644); err != nil { //nolint:gosec // G306: workspace file
		return nil, fmt.Errorf("str_replace: write %q: %w", a.Path, err)
	}
	return &agent.ToolResult{
		Output: map[string]any{
			"path":         a.Path,
			"replacements": occurrences,
		},
	}, nil
}

// normWS collapses every run of horizontal whitespace to a single space and
// trims the ends, so tab-vs-space and trailing-space drift compare equal.
func normWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// applyWhitespaceMatch handles the common case where the model reproduced the
// right lines but with wrong indentation (spaces instead of the file's tabs) or
// trailing-whitespace drift. It matches old_string against the file line-by-line
// under whitespace normalization; if exactly one window matches, it replaces
// that real byte span with new_string. It refuses to act on an ambiguous
// (multi-window) match so it can never edit the wrong location.
func (t *StrReplaceTool) applyWhitespaceMatch(content, oldString, newString string) (result, note string, ok bool) {
	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldString, "\n")
	if len(oldLines) == 0 || len(oldLines) > len(contentLines) {
		return "", "", false
	}
	normOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		normOld[i] = normWS(l)
	}
	start, matches := -1, 0
	for i := 0; i+len(oldLines) <= len(contentLines); i++ {
		hit := true
		for j := range oldLines {
			if normWS(contentLines[i+j]) != normOld[j] {
				hit = false
				break
			}
		}
		if hit {
			start = i
			matches++
		}
	}
	if matches != 1 {
		return "", "", false
	}
	merged := make([]string, 0, len(contentLines))
	merged = append(merged, contentLines[:start]...)
	merged = append(merged, strings.Split(newString, "\n")...)
	merged = append(merged, contentLines[start+len(oldLines):]...)
	return strings.Join(merged, "\n"), "matched via whitespace-normalized fallback", true
}

// Fuzzy thresholds (see the #942 design doc). A candidate window must have
// every line within fuzzyPerLineThreshold normalized edit distance of the
// corresponding old_string line; windows of 2+ lines must additionally keep
// their aggregate drift under fuzzyWindowThreshold. The aggregate bound is
// deliberately NOT applied to single-line windows: there the aggregate ratio
// equals the per-line ratio, and stacking both would silently tighten the
// effective threshold to 0.15 and reject the primary drift case (one small
// token typo on one line).
const (
	fuzzyPerLineThreshold = 0.25
	fuzzyWindowThreshold  = 0.15
)

// fuzzyEnabled reports whether the fuzzy line-window fallback is active.
// FOREMAN_STRREPLACE_FUZZY=0 reverts str_replace to the pre-#942 behavior
// (whitespace fallback + anchor hint only), mirroring the per-check
// FOREMAN_<NAME>_GATE kill-switch convention.
func fuzzyEnabled() bool { return os.Getenv("FOREMAN_STRREPLACE_FUZZY") != "0" }

// applyFuzzyMatch handles the drift case applyWhitespaceMatch cannot: the
// model reproduced the right block but mutated a token (renamed identifier,
// paraphrased comment). It scores every old_string-sized line window under
// whitespace normalization plus bounded per-line edit distance, and applies
// only when exactly one window qualifies. Zero or 2+ candidates return
// ok=false so the caller falls through to the anchorContext truth hint; the
// unique-window requirement means this can never edit the wrong location.
func (t *StrReplaceTool) applyFuzzyMatch(content, oldString, newString string) (result, note string, ok bool) {
	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldString, "\n")
	if len(oldLines) == 0 || len(oldLines) > len(contentLines) {
		return "", "", false
	}
	normOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		normOld[i] = normWS(l)
	}
	start, matches := -1, 0
	for i := 0; i+len(oldLines) <= len(contentLines); i++ {
		if windowWithinFuzzyBudget(contentLines[i:i+len(oldLines)], normOld) {
			start = i
			matches++
			if matches > 1 {
				return "", "", false
			}
		}
	}
	if matches != 1 {
		return "", "", false
	}
	merged := make([]string, 0, len(contentLines))
	merged = append(merged, contentLines[:start]...)
	merged = append(merged, strings.Split(newString, "\n")...)
	merged = append(merged, contentLines[start+len(oldLines):]...)
	return strings.Join(merged, "\n"),
		"matched via fuzzy line-window fallback (old_string drifted from the file's actual text)",
		true
}

// windowWithinFuzzyBudget scores one candidate window against the
// whitespace-normalized old_string lines. Every line must clear the per-line
// budget; multi-line windows must also clear the aggregate budget so drift
// cannot accumulate across many near-threshold lines. An all-blank window is
// never a match (it would otherwise match everywhere).
func windowWithinFuzzyBudget(window, normOld []string) bool {
	totalDist, totalLen := 0, 0
	for j, ol := range normOld {
		cl := normWS(window[j])
		maxLen := len([]rune(cl))
		if l := len([]rune(ol)); l > maxLen {
			maxLen = l
		}
		if maxLen == 0 {
			continue // both blank after normalization: identical
		}
		budget := int(fuzzyPerLineThreshold * float64(maxLen))
		d := boundedLevenshtein(cl, ol, budget)
		if d > budget {
			return false
		}
		totalDist += d
		totalLen += maxLen
	}
	if totalLen == 0 {
		return false
	}
	if len(normOld) > 1 &&
		float64(totalDist)/float64(totalLen) > fuzzyWindowThreshold {
		return false
	}
	return true
}

// anchorContext finds the most distinctive line of old_string that appears
// verbatim (trimmed) exactly once in the file, and returns the real surrounding
// content so the model can copy the actual bytes. Returns ok=false when no
// unique anchor line exists.
func anchorContext(content, oldString string) (string, bool) {
	contentLines := strings.Split(content, "\n")
	anchorIdx, anchorLen := -1, 0
	for _, ol := range strings.Split(oldString, "\n") {
		trimmed := strings.TrimSpace(ol)
		if len(trimmed) < 8 { // skip trivial lines like "}" or "return"
			continue
		}
		count, idx := 0, -1
		for i, cl := range contentLines {
			if strings.TrimSpace(cl) == trimmed {
				count++
				idx = i
			}
		}
		if count == 1 && len(trimmed) > anchorLen {
			anchorLen = len(trimmed)
			anchorIdx = idx
		}
	}
	if anchorIdx < 0 {
		return "", false
	}
	span := strings.Count(oldString, "\n") + 1
	lo := anchorIdx - 2
	if lo < 0 {
		lo = 0
	}
	hi := anchorIdx + span + 2
	if hi > len(contentLines) {
		hi = len(contentLines)
	}
	return strings.Join(contentLines[lo:hi], "\n"), true
}
