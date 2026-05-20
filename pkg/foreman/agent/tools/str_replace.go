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
			"exactly once. Set expected_replacements to N to require exactly N matches.",
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
