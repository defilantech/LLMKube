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

// Package tools is the concrete implementation of agent.ToolRegistry.
// Each tool advertises an OAI schema and runs work against a workspace
// directory. The native loop holds an opaque agent.ToolRegistry; the
// foreman-agent (or test code) builds a *tools.Registry and passes it
// in as the seam between the loop and the filesystem / shell.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// Tool is one callable function the model can invoke. Each tool
// advertises its OAI schema and runs the work via Execute. Tools are
// constructed with their workspace dir; every file-path argument is
// validated against that dir via resolveInside() before any filesystem
// access.
type Tool interface {
	Name() string
	Schema() oai.ToolSchemaDef
	Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error)
}

// Registry is the agent.ToolRegistry implementation. Build one with
// New(); call Filter(whitelist) to narrow the surface to the tools an
// Agent.spec.tools whitelist names.
type Registry struct {
	tools map[string]Tool
}

// New constructs a Registry holding the given tools. Each tool's Name()
// must be unique; duplicates are a programmer error and surface here
// rather than as a silent overwrite.
func New(tools ...Tool) (*Registry, error) {
	r := &Registry{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		if _, ok := r.tools[t.Name()]; ok {
			return nil, fmt.Errorf("tools.New: duplicate tool %q", t.Name())
		}
		r.tools[t.Name()] = t
	}
	return r, nil
}

// Filter returns a new Registry containing only the tools whose names
// appear in whitelist. Unknown names return an error so a typo in an
// Agent CR fails loud rather than silently disabling a tool.
func (r *Registry) Filter(whitelist []string) (*Registry, error) {
	out := &Registry{tools: make(map[string]Tool, len(whitelist))}
	for _, name := range whitelist {
		t, ok := r.tools[name]
		if !ok {
			return nil, fmt.Errorf("tools.Filter: unknown tool %q", name)
		}
		out.tools[name] = t
	}
	return out, nil
}

// Names returns the registered tool names in sorted order. Useful for
// log lines and for asserting against a whitelist in tests.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Schemas returns the OAI Tool advertisements in deterministic order so
// the wire format is stable across runs (easier to diff transcripts).
func (r *Registry) Schemas() []oai.Tool {
	names := r.Names()
	out := make([]oai.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, oai.Tool{
			Type:     "function",
			Function: r.tools[n].Schema(),
		})
	}
	return out
}

// Dispatch executes one tool call. Unknown tool names return an error
// that the loop turns into a tool message so the model can recover on
// the next turn.
func (r *Registry) Dispatch(ctx context.Context, name string, args json.RawMessage) (*agent.ToolResult, error) {
	t, ok := r.tools[name]
	if !ok {
		return nil, fmt.Errorf("tools: unknown tool %q", name)
	}
	return t.Execute(ctx, args)
}
