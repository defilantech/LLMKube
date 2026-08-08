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

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// MaxSubmitSummaryLen is the cap on submit_result.summary length. It
// has to fit the AgenticTask status payload comfortably while still
// forcing a one-sentence outcome rather than a wall of text.
const MaxSubmitSummaryLen = 280

// truncateRuneSafe truncates s to at most maxLen bytes, appending an
// ellipsis if truncation occurred. It never splits a multi-byte rune:
// if the byte limit falls inside a rune, the rune is dropped entirely.
//
// When a sentence boundary survives in the tail of the budget, the cut is
// taken there instead of mid-word (#1411). A reviewer summary is rendered
// verbatim as the PR description, and a body that ends "... plus in
// `claim.…" reads as a corrupted artifact rather than a capped one.
// Backing off to the boundary costs a few characters and yields prose
// that ends where a sentence ends.
func truncateRuneSafe(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	const ellipsis = "…"
	// A boundary cut keeps its terminating punctuation, so the marker is
	// spaced off it: "First sentence. …" rather than "First sentence.…".
	const boundarySuffix = " …"
	if cut := runePrefix(s, maxLen-len(boundarySuffix)); cut != "" {
		// Only back off when the boundary keeps most of the budget;
		// otherwise a summary whose first sentence is short would lose
		// nearly all of its content to the sentence rule.
		if b := lastSentenceEnd(cut); b*5 >= len(cut)*3 {
			return cut[:b] + boundarySuffix
		}
	}
	return runePrefix(s, maxLen-len(ellipsis)) + ellipsis
}

// runePrefix returns the longest prefix of s that fits in maxBytes without
// splitting a rune.
func runePrefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	byteLen := 0
	for i, r := range s {
		runeBytes := len(string(r))
		if byteLen+runeBytes > maxBytes {
			return s[:i]
		}
		byteLen += runeBytes
	}
	return s
}

// lastSentenceEnd returns the index just past the last sentence-ending
// punctuation in s, or 0 when there is none. A terminator counts only when
// followed by whitespace or the end of the string, so decimals and dotted
// identifiers ("v1.2", "logger.info") are not mistaken for boundaries.
func lastSentenceEnd(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case '.', '!', '?':
			if i == len(s)-1 || s[i+1] == ' ' || s[i+1] == '\n' || s[i+1] == '\t' {
				return i + 1
			}
		}
	}
	return 0
}

// SubmitResultTool is the terminal tool. When the model calls it, the
// loop captures the envelope and exits. The fields map directly onto
// AgenticTaskStatus.Verdict + Status.Result + the commit message the
// Phase D repo helpers use to push the branch.
type SubmitResultTool struct{}

type submitResultArgs struct {
	Verdict       string         `json:"verdict"`
	Summary       string         `json:"summary"`
	CommitMessage string         `json:"commit_message"`
	Extra         map[string]any `json:"extra"`
}

// Name returns the tool name as advertised to the model.
func (SubmitResultTool) Name() string { return "submit_result" }

// Schema returns the OAI schema advertisement.
func (SubmitResultTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name:        "submit_result",
		Description: "Terminal tool. Submit the final outcome. The loop exits after this call.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "verdict":        {"type": "string", "enum": ["GO", "NO-GO", "ERROR"]},
  "summary":        {"type": "string", "description": "One-sentence outcome summary (1-280 chars)."},
  "commit_message": {"type": "string",
    "description": "Full commit message including subject, body, and Fixes #N if applicable."},
  "extra": {"type": "object",
    "description": "Structured extra fields the executor may surface in status.result.extra."}
},
"required": ["verdict", "summary"]
}`),
	}
}

// Execute validates the envelope and returns it as the terminal result.
// Validation is intentionally strict: a bad verdict here means the
// model is hallucinating outside the locked enum, and we surface that
// rather than papering over it.
func (SubmitResultTool) Execute(_ context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a submitResultArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("submit_result: bad args: %w", err)
	}
	switch a.Verdict {
	case "GO", "NO-GO", "ERROR":
	default:
		return nil, fmt.Errorf("submit_result: invalid verdict %q (must be GO, NO-GO, or ERROR)", a.Verdict)
	}
	if a.Summary == "" {
		return nil, fmt.Errorf("submit_result: summary is required")
	}
	if len(a.Summary) > MaxSubmitSummaryLen {
		a.Summary = truncateRuneSafe(a.Summary, MaxSubmitSummaryLen)
	}
	return &agent.ToolResult{
		Terminal:      true,
		Verdict:       a.Verdict,
		Summary:       a.Summary,
		CommitMessage: a.CommitMessage,
		Extra:         a.Extra,
		Output: map[string]any{
			"accepted": true,
			"verdict":  a.Verdict,
		},
	}, nil
}
