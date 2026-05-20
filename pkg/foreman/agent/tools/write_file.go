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
	"path/filepath"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// WriteFileTool creates or overwrites a workspace file. mkdir -p
// behavior for missing parent dirs so a new package directory does not
// require a separate tool call to set up.
type WriteFileTool struct {
	Workspace string
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Name returns the tool name as advertised to the model.
func (t *WriteFileTool) Name() string { return "write_file" }

// Schema returns the OAI schema advertisement.
func (t *WriteFileTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name:        "write_file",
		Description: "Create or overwrite a file in the workspace. Creates missing parent directories.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "path":    {"type": "string", "description": "Workspace-relative path to write."},
  "content": {"type": "string", "description": "File contents (UTF-8)."}
},
"required": ["path", "content"]
}`),
	}
}

// Execute writes the file contents, creating parent dirs as needed.
func (t *WriteFileTool) Execute(_ context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a writeFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("write_file: bad args: %w", err)
	}
	if a.Path == "" {
		return nil, fmt.Errorf("write_file: path is required")
	}
	full, err := resolveInside(t.Workspace, a.Path)
	if err != nil {
		return nil, fmt.Errorf("write_file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, fmt.Errorf("write_file: mkdir parent: %w", err)
	}
	// 0o644 because the workspace lives under the agent's $HOME and the
	// git operations need to read these files in subsequent tool calls.
	if err := os.WriteFile(full, []byte(a.Content), 0o644); err != nil { //nolint:gosec // G306: workspace file, not secret
		return nil, fmt.Errorf("write_file: write %q: %w", a.Path, err)
	}
	return &agent.ToolResult{
		Output: map[string]any{
			"path":  a.Path,
			"bytes": len(a.Content),
		},
	}, nil
}
