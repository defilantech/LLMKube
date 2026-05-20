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
	"io"
	"os"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// defaultReadMaxBytes caps a single read_file call so a confused model
// asking for a 50 MB binary blob does not blow the transcript budget.
const defaultReadMaxBytes = 256 * 1024 // 256 KiB

// ReadFileTool reads a workspace file and returns its contents (capped).
type ReadFileTool struct {
	Workspace string
	MaxBytes  int // 0 falls back to defaultReadMaxBytes
}

type readFileArgs struct {
	Path string `json:"path"`
}

// Name returns the tool name as advertised to the model.
func (t *ReadFileTool) Name() string { return "read_file" }

// Schema returns the OAI schema advertisement.
func (t *ReadFileTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name:        "read_file",
		Description: "Read a file from the workspace and return its contents. Paths are relative to the repository root.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "path": {"type": "string", "description": "Workspace-relative path to the file."}
},
"required": ["path"]
}`),
	}
}

// Execute reads the file at args.path and returns the contents capped at
// MaxBytes. Paths are containment-checked; absolute paths and any path
// that resolves outside the workspace return an error.
func (t *ReadFileTool) Execute(_ context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a readFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("read_file: bad args: %w", err)
	}
	if a.Path == "" {
		return nil, fmt.Errorf("read_file: path is required")
	}
	full, err := resolveInside(t.Workspace, a.Path)
	if err != nil {
		return nil, fmt.Errorf("read_file: %w", err)
	}
	f, err := os.Open(full) //nolint:gosec // G304: path is resolveInside-validated
	if err != nil {
		return nil, fmt.Errorf("read_file: open %q: %w", a.Path, err)
	}
	defer func() { _ = f.Close() }()

	limit := t.MaxBytes
	if limit <= 0 {
		limit = defaultReadMaxBytes
	}
	// Read one extra byte to detect truncation cleanly.
	buf, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("read_file: read %q: %w", a.Path, err)
	}
	truncated := len(buf) > limit
	if truncated {
		buf = buf[:limit]
	}
	return &agent.ToolResult{
		Output: map[string]any{
			"path":      a.Path,
			"content":   string(buf),
			"truncated": truncated,
			"bytes":     len(buf),
		},
	}, nil
}
