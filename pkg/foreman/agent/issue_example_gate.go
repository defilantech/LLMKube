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

// issue_example_gate.go implements the issue-example advisory gate check.
//
// When an issue body contains a concrete example (a fenced code block near a
// heading or line that uses marker words such as "example", "repro",
// "reproduce", or "expected"), the check surfaces that block as an advisory to
// the reviewer so they can verify the coder's diff satisfies it.
//
// This is ADVISORY and LANGUAGE-AGNOSTIC: it does not fail the gate loop and
// it operates solely on the issue text, requiring no workspace access or
// command execution.
package agent

import (
	"context"
	"strings"
)

// maxExampleBytes caps the harvested example at ~2 KB so the advisory stays
// readable in a reviewer prompt.
const maxExampleBytes = 2 * 1024

// exampleMarkers is the case-insensitive set of words that signal a concrete
// example in issue prose. A fenced block is preferentially chosen when it
// appears near (within a few lines of) a heading or line containing one of
// these words.
var exampleMarkers = []string{"example", "repro", "reproduce", "expected"}

// harvestIssueExample extracts the best concrete-example code block from an
// issue body. Conservative selection rules:
//
//  1. If the body contains any marker word AND a fenced code block whose
//     preceding heading/line (up to 5 lines above) also contains a marker
//     word, return that block's content.
//  2. If the body contains any marker word but no block has a marked heading,
//     fall back to the FIRST fenced block in the body.
//  3. If the body contains NO marker word at all, return "" — we do not
//     surface arbitrary code blocks.
//
// The returned string is the content between the fences (fences stripped),
// capped at maxExampleBytes.
func harvestIssueExample(body string) string {
	if !containsMarker(body) {
		return ""
	}

	lines := strings.Split(body, "\n")
	type block struct {
		content     string
		markedAbove bool
	}
	var blocks []block

	i := 0
	for i < len(lines) {
		line := lines[i]
		if strings.HasPrefix(line, "```") {
			// Opening fence found; collect until the closing fence. Any info
			// string on the opening fence is ignored; the closing fence is any
			// later line that also starts with "```".
			var buf []string
			i++
			for i < len(lines) && !strings.HasPrefix(lines[i], "```") {
				buf = append(buf, lines[i])
				i++
			}
			// i now points at the closing fence (or end-of-lines).
			content := strings.Join(buf, "\n")

			// Check if any of the preceding up-to-5 lines contain a marker.
			marked := false
			fenceStart := i - len(buf) - 1 // index of opening fence
			for lookback := 1; lookback <= 5 && fenceStart-lookback >= 0; lookback++ {
				if containsMarker(lines[fenceStart-lookback]) {
					marked = true
					break
				}
			}
			blocks = append(blocks, block{content: content, markedAbove: marked})
		}
		i++
	}

	if len(blocks) == 0 {
		return ""
	}

	// Prefer the first block with a marked heading.
	for _, b := range blocks {
		if b.markedAbove {
			return capBytes(b.content)
		}
	}

	// Fall back: body has a marker somewhere, return the first block.
	return capBytes(blocks[0].content)
}

// containsMarker reports whether s (case-insensitive) contains any of the
// example marker words.
func containsMarker(s string) bool {
	lower := strings.ToLower(s)
	for _, m := range exampleMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// capBytes caps s at maxExampleBytes, keeping the leading portion.
func capBytes(s string) string {
	if len(s) <= maxExampleBytes {
		return s
	}
	return s[:maxExampleBytes]
}

// checkIssueExample returns a gate-check closure that harvests a concrete
// example from issueText and, when one is found, emits an advisory asking the
// reviewer to verify the diff satisfies it. The closure ignores its workspace
// and run arguments — the check is purely text-based.
func checkIssueExample(issueText string) func(ctx context.Context, workspace string, run commandRunner) (bool, string) {
	return func(_ context.Context, _ string, _ commandRunner) (bool, string) {
		example := harvestIssueExample(issueText)
		if example == "" {
			return false, ""
		}
		out := "Advisory: the issue provides a concrete example the change should satisfy. " +
			"Verify the diff handles it:\n" + example
		return true, out
	}
}
