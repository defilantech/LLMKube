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
	"errors"
	"fmt"
	"sort"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// ErrToolNotInWhitelist is returned by Dispatch when the model calls
// a tool that exists in the broader system but was excluded by the
// Agent's spec.tools whitelist (the Filter step). Distinct from a
// truly-unknown tool name so the failure taxonomy (v0.3 #559) can map
// this to ConstraintViolated instead of the typo-flavored
// ModelMisunderstood.
//
// Practical example: a reviewer-role Agent has
// tools=[read_file, grep, bash, submit_result]. If the model calls
// write_file (which exists in the broader system but is not on the
// reviewer's whitelist), Dispatch returns this sentinel. The loop
// surfaces it as a tool message whose content tells the model the
// call was blocked by role -- and the structured error lets the
// failure-taxonomy work split this from a typo. Closes #561.
var ErrToolNotInWhitelist = errors.New("tools: tool excluded by Agent.spec.tools whitelist")

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
	// filteredOut tracks tool names that exist in the broader system
	// (the pre-Filter registry) but were excluded by the whitelist.
	// Dispatch distinguishes these from truly-unknown names so the
	// failure-taxonomy work (v0.3 #559) can route ConstraintViolated
	// separately from ModelMisunderstood. Empty when the Registry was
	// constructed without Filter, in which case every dispatch failure
	// is "unknown tool" as before.
	filteredOut map[string]bool
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
//
// The returned Registry remembers the names that existed in the source
// Registry but were excluded by the whitelist; Dispatch uses this to
// emit ErrToolNotInWhitelist (distinct from "unknown tool") when the
// model calls one of those excluded names. The reviewer-role hardening
// in #561 relies on this distinction.
func (r *Registry) Filter(whitelist []string) (*Registry, error) {
	out := &Registry{
		tools:       make(map[string]Tool, len(whitelist)),
		filteredOut: make(map[string]bool, len(r.tools)),
	}
	allowed := make(map[string]bool, len(whitelist))
	for _, name := range whitelist {
		t, ok := r.tools[name]
		if !ok {
			return nil, fmt.Errorf("tools.Filter: unknown tool %q", name)
		}
		out.tools[name] = t
		allowed[name] = true
	}
	for name := range r.tools {
		if !allowed[name] {
			out.filteredOut[name] = true
		}
	}
	return out, nil
}

// Add inserts additional tools into an existing registry after
// construction (e.g. dynamically-discovered MCP tools that intentionally
// bypass the Agent.spec.tools whitelist because they carry their own
// per-server allowedTools gate). Nil tools are skipped. Duplicate names are
// skipped (not overwritten) and reported via the returned error, but every
// non-duplicate tool is still added -- so a single collision never drops the
// rest.
func (r *Registry) Add(tools ...Tool) error {
	var dups []string
	for _, t := range tools {
		if t == nil {
			continue
		}
		if _, ok := r.tools[t.Name()]; ok {
			dups = append(dups, t.Name())
			continue
		}
		r.tools[t.Name()] = t
	}
	if len(dups) > 0 {
		return fmt.Errorf("tools.Add: skipped duplicate tool(s): %v", dups)
	}
	return nil
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

// Dispatch executes one tool call. Three failure modes:
//
//  1. Name is in the registry (whitelist): execute and return.
//  2. Name is in the source registry but excluded by the Filter
//     whitelist: return a wrapped ErrToolNotInWhitelist. The loop
//     turns this into a tool message that tells the model the call
//     was blocked by role. v0.3 #559 will map this to
//     ConstraintViolated in the failure taxonomy.
//  3. Name is unknown to the system entirely (typo, hallucinated
//     tool): return a generic "unknown tool" error. The loop also
//     turns this into a tool message; the model usually self-corrects
//     on the next turn.
func (r *Registry) Dispatch(ctx context.Context, name string, args json.RawMessage) (*agent.ToolResult, error) {
	if t, ok := r.tools[name]; ok {
		return t.Execute(ctx, args)
	}
	if r.filteredOut[name] {
		return nil, fmt.Errorf("%w: %q", ErrToolNotInWhitelist, name)
	}
	return nil, fmt.Errorf("tools: unknown tool %q", name)
}
