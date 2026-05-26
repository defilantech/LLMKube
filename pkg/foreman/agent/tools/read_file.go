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
	"bufio"
	"bytes"
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
//
// We deliberately keep this tight (16 KiB) because each tool result
// stays in the OAI message history for every subsequent turn's
// prompt-eval. A 60 KiB file read on turn 4 makes every later turn's
// prompt-eval pay for those 60 KiB again -- on Carnice MoE on Metal,
// that compounds into multi-minute prompt-eval times by turn 20+.
//
// 16 KiB is enough to read a typical Go file or top-level config end
// to end (the LLMKube + Foreman codebase has very few sources past
// 8 KiB). For genuinely long files the model uses the offset / limit
// args to read a window.
const defaultReadMaxBytes = 16 * 1024 // 16 KiB

// ReadFileTool reads a workspace file and returns its contents (capped).
//
// Two reading modes via Execute args:
//
//  1. Default (no offset, no limit) :: read from the start of the file
//     up to MaxBytes; if the file is larger, set truncated=true.
//     The content field is the raw bytes verbatim.
//  2. Line-ranged (offset >= 1 or limit > 0) :: skip to line `offset`,
//     read at most `limit` lines (limit=0 means "to end of file"),
//     still bounded by MaxBytes. Line bookkeeping fields (start_line /
//     end_line / total_lines) reflect the actual window served.
type ReadFileTool struct {
	Workspace string
	MaxBytes  int // 0 falls back to defaultReadMaxBytes
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"` // 1-based line number to start at; default 1
	Limit  int    `json:"limit,omitempty"`  // max lines to read; default 0 = no line limit
}

// Name returns the tool name as advertised to the model.
func (t *ReadFileTool) Name() string { return "read_file" }

// Schema returns the OAI schema advertisement.
func (t *ReadFileTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "read_file",
		Description: "Read a file from the workspace and return its contents. " +
			"Output is capped at 16 KiB by default; for longer files pass offset " +
			"and limit to read a window of the file. The result includes " +
			"total_lines so subsequent calls can fetch the rest. Paths are " +
			"relative to the repository root.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "path":   {"type": "string", "description": "Workspace-relative path to the file."},
  "offset": {"type": "integer", "minimum": 1,
             "description": "1-based starting line for a ranged read. Defaults to 1."},
  "limit":  {"type": "integer", "minimum": 0,
             "description": "Lines to return for a ranged read. 0 (default) reads to EOF or until the byte cap."}
},
"required": ["path"]
}`),
	}
}

// Execute reads the file at args.path and returns the contents capped
// at MaxBytes. With args.offset / args.limit unset (the default mode),
// the tool returns raw bytes verbatim. With either set, the tool
// returns a line-aligned window. Paths are containment-checked;
// absolute paths and paths that resolve outside the workspace return an
// error.
func (t *ReadFileTool) Execute(_ context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	var a readFileArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("read_file: bad args: %w", err)
	}
	if a.Path == "" {
		return nil, fmt.Errorf("read_file: path is required")
	}
	if a.Offset < 0 {
		return nil, fmt.Errorf("read_file: offset must be >= 1 (or 0/unset for start of file)")
	}
	if a.Limit < 0 {
		return nil, fmt.Errorf("read_file: limit must be >= 0 (0 means no line limit)")
	}

	full, err := resolveInside(t.Workspace, a.Path)
	if err != nil {
		return nil, fmt.Errorf("read_file: %w", err)
	}

	cap := t.MaxBytes
	if cap <= 0 {
		cap = defaultReadMaxBytes
	}

	if a.Offset == 0 && a.Limit == 0 {
		return t.readWhole(a.Path, full, cap)
	}
	return t.readRange(a.Path, full, cap, a.Offset, a.Limit)
}

// readWhole is the default-mode path: raw bytes from the start of the
// file up to cap. Preserves the byte-for-byte fidelity callers expect
// when not requesting a ranged window. Includes total_lines so the
// model knows to switch to a ranged read on the next call when there
// is more content beyond the cap.
func (t *ReadFileTool) readWhole(path, full string, cap int) (*agent.ToolResult, error) {
	f, err := os.Open(full) //nolint:gosec // G304: full is resolveInside-validated by the caller
	if err != nil {
		return nil, fmt.Errorf("read_file: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// Read one extra byte to detect truncation cleanly.
	buf, err := io.ReadAll(io.LimitReader(f, int64(cap)+1))
	if err != nil {
		return nil, fmt.Errorf("read_file: read %q: %w", path, err)
	}
	truncated := len(buf) > cap
	if truncated {
		buf = buf[:cap]
	}

	// Count lines in the returned slice. If truncated, also count the
	// remaining lines in the rest of the file so total_lines stays
	// accurate without buffering the whole file.
	emittedLines := countLines(buf)
	totalLines := emittedLines
	if truncated {
		remaining, err := countRemainingLines(f)
		if err != nil {
			return nil, fmt.Errorf("read_file: count tail %q: %w", path, err)
		}
		totalLines += remaining
		// If the truncation point fell mid-line, the next line is
		// already half-counted in emittedLines; the tail count starts
		// from the next newline so the sum is correct.
	}

	endLine := emittedLines
	if endLine == 0 && len(buf) > 0 {
		endLine = 1 // file with no newlines at all is still 1 line of content
	}

	return &agent.ToolResult{
		Output: map[string]any{
			"path":        path,
			"content":     string(buf),
			"bytes":       len(buf),
			"truncated":   truncated,
			"start_line":  1,
			"end_line":    endLine,
			"total_lines": totalLines,
		},
	}, nil
}

// readRange is the line-ranged path. Skips to line `offset`, reads up
// to `limit` lines, all bounded by cap bytes. The model uses this to
// walk large files in windows without re-embedding the whole file in
// every later turn.
func (t *ReadFileTool) readRange(path, full string, cap, offset, limit int) (*agent.ToolResult, error) {
	f, err := os.Open(full) //nolint:gosec // G304: full is resolveInside-validated by the caller
	if err != nil {
		return nil, fmt.Errorf("read_file: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	startLine := offset
	if startLine < 1 {
		startLine = 1
	}

	scanner := bufio.NewScanner(f)
	// Allow long single lines (generated CRDs, minified JS, etc.).
	// 8 MiB ceiling is far above any reasonable source file and well
	// under runaway-memory territory.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var (
		out         bytes.Buffer
		lineNum     = 0
		emittedEnd  = 0 // last 1-based line number included in output
		truncated   bool
		emittedDone bool // once true, we stop accumulating but keep counting lines for total_lines
	)

	for scanner.Scan() {
		lineNum++

		// Skip until offset.
		if lineNum < startLine {
			continue
		}

		// Past the limit or past the byte cap: keep scanning so
		// total_lines stays accurate, but stop accumulating output.
		if emittedDone {
			continue
		}

		// Stop emitting after `limit` lines (when limit > 0).
		if limit > 0 && (lineNum-startLine) >= limit {
			emittedDone = true
			continue
		}

		line := scanner.Bytes()
		needed := len(line) + 1 // include the newline we add back
		if out.Len()+needed > cap {
			// One more byte would push past the cap. Write what fits,
			// mark truncated, switch to count-only mode for the rest
			// of the file. The model can re-call with a later offset
			// or a smaller limit.
			remaining := cap - out.Len()
			if remaining > 0 {
				if remaining > len(line) {
					out.Write(line)
					out.WriteByte('\n')
				} else {
					out.Write(line[:remaining])
				}
			}
			truncated = true
			emittedDone = true
			emittedEnd = lineNum
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
		emittedEnd = lineNum
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read_file: scan %q: %w", path, err)
	}

	if startLine > lineNum && lineNum > 0 {
		// offset past EOF -- return empty content with truthful counts.
		emittedEnd = startLine - 1
	}

	return &agent.ToolResult{
		Output: map[string]any{
			"path":        path,
			"content":     out.String(),
			"bytes":       out.Len(),
			"truncated":   truncated,
			"start_line":  startLine,
			"end_line":    emittedEnd,
			"total_lines": lineNum,
		},
	}, nil
}

// countLines returns the number of \n-terminated lines in b. A trailing
// non-empty fragment without a final \n counts as one additional line
// (matches the intuitive "lines in this content" expectation).
func countLines(b []byte) int {
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// countRemainingLines reads f to EOF and returns the number of newlines
// encountered. Used after a truncated read to finish counting
// total_lines without buffering the file's tail in memory.
func countRemainingLines(f *os.File) (int, error) {
	buf := make([]byte, 64*1024)
	count := 0
	lastByte := byte(0)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			count += bytes.Count(buf[:n], []byte{'\n'})
			lastByte = buf[n-1] //nolint:gosec // G602: n > 0 + Read contract n <= len(buf)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	// A final non-newline-terminated fragment of tail is its own line.
	if lastByte != 0 && lastByte != '\n' {
		count++
	}
	return count, nil
}
