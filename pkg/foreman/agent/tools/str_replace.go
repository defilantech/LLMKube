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
	"errors"
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

	// failures counts consecutive str_replace match-failures per file path
	// within one run (StrReplaceTool is instantiated per workspace). Once a
	// file crosses strReplaceEscalateAfter, the failure result escalates to
	// dumping the full file and ordering a write_file, so a model that keeps
	// re-hallucinating old_string gets a deterministic escape instead of
	// looping into RepeatedToolCall (#1025). Reset on a successful edit.
	failures map[string]int
}

const (
	// strReplaceEscalateAfter is the consecutive per-file failure count at
	// which str_replace stops handing back an anchor snippet and instead
	// dumps the whole file with a write_file directive.
	strReplaceEscalateAfter = 2
	// strReplaceEscalateMaxLines caps how large a file we will inline in the
	// escalated hint; above it, the model is told to write_file from a fresh
	// read rather than dumping the whole thing into the transcript.
	strReplaceEscalateMaxLines = 400
)

// noteFailure records a match-failure for path and returns the new consecutive
// count. clearFailures resets it after any successful edit to that path.
func (t *StrReplaceTool) noteFailure(path string) int {
	if t.failures == nil {
		t.failures = map[string]int{}
	}
	t.failures[path]++
	return t.failures[path]
}

func (t *StrReplaceTool) clearFailures(path string) { delete(t.failures, path) }

// failureResult records a failure for path and returns either the caller's
// normal hint (early failures) or, once the file has failed
// strReplaceEscalateAfter times in a row, the escalated write_file hint.
func (t *StrReplaceTool) failureResult(path, content, normalHint string) error {
	if n := t.noteFailure(path); n >= strReplaceEscalateAfter {
		return errors.New(escalatedEditHint(path, content, n))
	}
	return errors.New(normalHint)
}

// escalatedEditHint tells the model to stop retrying str_replace on a file it
// cannot match and to rewrite it wholesale, inlining the full numbered content
// for small files so write_file needs no further reads.
func escalatedEditHint(path, content string, n int) string {
	head := fmt.Sprintf(
		"str_replace has failed %d times in a row on %q: your old_string is not matching the "+
			"actual bytes. STOP calling str_replace on this file. ", n, path)
	if strings.Count(content, "\n")+1 <= strReplaceEscalateMaxLines {
		return head + fmt.Sprintf(
			"Call write_file on %q with the full corrected file. Its complete current content is "+
				"below, line-numbered for reference (do NOT include the numbers in write_file):\n%s",
			path, numberLines(content))
	}
	return head + "The file is large: do a fresh read_file of the exact region and copy the bytes " +
		"VERBATIM, or call write_file with the full corrected content. Do not retype old_string from memory."
}

// numberLines prefixes each line of content with its 1-based number and a tab.
func numberLines(content string) string {
	var b strings.Builder
	for i, l := range strings.Split(content, "\n") {
		fmt.Fprintf(&b, "%d\t%s\n", i+1, l)
	}
	return b.String()
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
			"copy that exactly and retry. For a NEW file or a full-file rewrite, use " +
			"write_file instead: str_replace requires old_string to already exist in " +
			"the file.",
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
	// A no-op edit (old_string == new_string) changes nothing, but the match
	// logic below would still "replace" the string with itself and report
	// replacements:1 -- a phantom success. A model that sees success on an
	// unchanged file re-issues the identical call and gets killed by the
	// RepeatedToolCall stuck-loop detector (#968). Reject it explicitly so the
	// model corrects new_string or moves on.
	if a.OldString == a.NewString {
		return nil, fmt.Errorf("str_replace: old_string and new_string are identical, " +
			"so this edit would change nothing. If the file already contains the text " +
			"you want, move on to the next step; otherwise set new_string to the " +
			"replacement text you intend")
	}
	full, err := resolveInside(t.Workspace, a.Path)
	if err != nil {
		return nil, fmt.Errorf("str_replace: %w", err)
	}
	raw, err := os.ReadFile(full) //nolint:gosec // G304: path is resolveInside-validated
	if err != nil {
		// The most literal wrong-tool case: editing a file that does not
		// exist. Seen live burning a coder's whole restricted-edit budget on
		// bare ENOENT retries against a hallucinated path; steer to
		// write_file instead (#942 Part B).
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("str_replace: %q does not exist. "+
				"To create a new file, call write_file with the full contents instead", a.Path)
		}
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
				t.clearFailures(a.Path)
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
					t.clearFailures(a.Path)
					return &agent.ToolResult{
						Output: map[string]any{"path": a.Path, "replacements": 1, "note": note},
					}, nil
				}
			}
			// Could not safely locate the edit. Surface the file's ACTUAL current
			// text near the model's intended anchor so it can retry against
			// truth instead of re-hallucinating old_string.
			if actual, ok := anchorContext(content, a.OldString); ok {
				return nil, t.failureResult(a.Path, content, fmt.Sprintf(
					"str_replace: old_string not found in %q. "+
						"The file's actual current text near your edit is below - copy it "+
						"VERBATIM into old_string and retry:\n%s%s", a.Path, actual, writeFileHint(content)))
			}
			// No unique verbatim anchor exists. Fall back to the closest line
			// match so the model always gets real file content to re-anchor on
			// instead of the bare count error (#944).
			if closest := closestLineContext(content, a.OldString); closest != "" {
				return nil, t.failureResult(a.Path, content, fmt.Sprintf(
					"str_replace: old_string not found in %q. "+
						"No unique anchor line exists; the closest approximate match "+
						"in the file is below (not verbatim). Copy the actual bytes "+
						"VERBATIM into old_string and retry:\n%s%s", a.Path, closest, writeFileHint(content)))
			}
		}
		return nil, t.failureResult(a.Path, content, fmt.Sprintf(
			"str_replace: old_string found %d times in %q, want %d%s",
			occurrences, a.Path, want, writeFileHint(content)))
	}
	next := strings.ReplaceAll(content, a.OldString, a.NewString)
	if err := os.WriteFile(full, []byte(next), 0o644); err != nil { //nolint:gosec // G306: workspace file
		return nil, fmt.Errorf("str_replace: write %q: %w", a.Path, err)
	}
	t.clearFailures(a.Path)
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

// fuzzyPerLineMaxDist caps the per-line budget in absolute terms. The
// percentage threshold alone lets long lines (120+ runes) absorb 30+ edits,
// enough to alias two genuinely different logger.Info calls; real old_string
// drift is a token or two, never dozens of runes.
const fuzzyPerLineMaxDist = 6

// fuzzyMaxOldLines bounds the window scan. The tool's schema tells models to
// use 1-3 line snippets; a huge old_string against a large file is O(minutes)
// of CPU in a single tool call and is never legitimate drift recovery.
const fuzzyMaxOldLines = 40

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
// ok=false so the caller falls through to the anchorContext truth hint. The
// unique-window requirement guarantees it never edits an AMBIGUOUS location;
// a unique near-miss can still be an unintended target (a semantic twin
// within threshold), which is why the note reports exactly what was replaced
// and where, so the model and transcript reader can catch a wrong-target
// edit.
func (t *StrReplaceTool) applyFuzzyMatch(content, oldString, newString string) (result, note string, ok bool) {
	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldString, "\n")
	if len(oldLines) == 0 || len(oldLines) > fuzzyMaxOldLines || len(oldLines) > len(contentLines) {
		return "", "", false
	}
	normOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		normOld[i] = normWS(l)
	}
	normContent := make([]string, len(contentLines))
	for i, l := range contentLines {
		normContent[i] = normWS(l)
	}
	start, matches := -1, 0
	for i := 0; i+len(oldLines) <= len(contentLines); i++ {
		if windowWithinFuzzyBudget(normContent[i:i+len(oldLines)], normOld) {
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
	original := strings.Join(contentLines[start:start+len(oldLines)], "\n")
	note = fmt.Sprintf(
		"matched via fuzzy line-window fallback at line %d (old_string drifted from the file's actual text); replaced: %q",
		start+1, original)
	return strings.Join(merged, "\n"), note, true
}

// windowWithinFuzzyBudget scores one candidate window (already
// whitespace-normalized by the caller) against the whitespace-normalized
// old_string lines. Every line must clear the per-line budget; multi-line
// windows must also clear the aggregate budget so drift cannot accumulate
// across many near-threshold lines. An all-blank window is never a match (it
// would otherwise match everywhere).
func windowWithinFuzzyBudget(normWindow, normOld []string) bool {
	totalDist, totalLen := 0, 0
	for j, ol := range normOld {
		cl := normWindow[j]
		maxLen := len([]rune(cl))
		if l := len([]rune(ol)); l > maxLen {
			maxLen = l
		}
		if maxLen == 0 {
			continue // both blank after normalization: identical
		}
		budget := int(fuzzyPerLineThreshold * float64(maxLen))
		if budget > fuzzyPerLineMaxDist {
			budget = fuzzyPerLineMaxDist
		}
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

// writeFileHintMaxLines bounds the "did you mean write_file?" steering hint
// to small files. On a large file a failed match means old_string drift and
// the anchor hint is the right feedback; on a small (often brand-new or stub)
// file it frequently means the model picked the wrong tool for creating or
// rewriting the file outright (#478).
const writeFileHintMaxLines = 40

// writeFileHint returns the steering line appended to a failed-recovery
// error for small files, or "" for larger ones.
func writeFileHint(content string) string {
	if strings.Count(content, "\n")+1 <= writeFileHintMaxLines {
		return "\nIf you are creating or rewriting this file, call write_file with the full contents instead."
	}
	return ""
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

// closestLineContext is the best-effort fallback when anchorContext finds no
// unique verbatim anchor. It scores every file line against the most
// distinctive old_string line (longest trimmed line, skipping trivial lines)
// using boundedLevenshtein and returns the surrounding real lines of the
// single lowest-distance hit, clearly labeled as approximate. This gives the
// model actual file bytes to re-anchor on instead of the bare count error.
func closestLineContext(content, oldString string) string {
	contentLines := strings.Split(content, "\n")
	oldLines := strings.Split(oldString, "\n")

	// Pick the most distinctive old_string line (longest trimmed, skip trivial).
	bestIdx, bestLen := -1, 0
	for i, ol := range oldLines {
		trimmed := strings.TrimSpace(ol)
		if len(trimmed) < 8 {
			continue
		}
		if len(trimmed) > bestLen {
			bestLen = len(trimmed)
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return ""
	}
	probe := strings.TrimSpace(oldLines[bestIdx])

	// Score every content line against the probe; keep the closest hit.
	minDist := len(probe) + 1 // worse than any real distance
	closestIdx := 0
	for i, cl := range contentLines {
		d := boundedLevenshtein(strings.TrimSpace(cl), probe, minDist)
		if d < minDist {
			minDist = d
			closestIdx = i
		}
	}

	span := strings.Count(oldString, "\n") + 1
	lo := closestIdx - 2
	if lo < 0 {
		lo = 0
	}
	hi := closestIdx + span + 2
	if hi > len(contentLines) {
		hi = len(contentLines)
	}
	return strings.Join(contentLines[lo:hi], "\n")
}
