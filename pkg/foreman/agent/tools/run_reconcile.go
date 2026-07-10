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
	"os/exec"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
	"github.com/defilantech/llmkube/pkg/foreman/slicer"
)

// RunReconcileTool checks a sliced Workload's integrated union against the slice
// plan's pinned shared identifiers, catching cross-slice interface drift a
// build cannot see. It runs on the deterministic (no-LLM) path: the executor
// dispatches it and its Terminal verdict drives the AgenticTask status. Part of
// Sliced Workloads (#1033).
//
// v1 runs only the deterministic, authoritative pinned_check. The advisory
// llm_sweep (slicer.LLMSweep) needs a model endpoint the deterministic path
// does not resolve; wiring it is a follow-up.
type RunReconcileTool struct {
	// Workspace is the cloned working tree the integration branch is checked
	// out into so pinned_check can read the union's files.
	Workspace string
	// Token sources the git token for fetching the integration branch. A nil
	// Token skips auth (public repos, tests against local remotes).
	Token func() (string, error)
}

type reconcileSharedID struct {
	ID           string   `json:"id"`
	DefinedBy    string   `json:"definedBy"`
	ReferencedBy []string `json:"referencedBy"`
}

type reconcileSlice struct {
	Name  string   `json:"name"`
	Files []string `json:"files"`
}

type reconcileArgs struct {
	Branch            string              `json:"branch"`
	Slices            []reconcileSlice    `json:"slices"`
	SharedIdentifiers []reconcileSharedID `json:"sharedIdentifiers"`
}

func (RunReconcileTool) Name() string { return "run_reconcile" }

// Schema advertises the tool. The deterministic executor path never shows this
// to a model, but a future LLM-driven wrapper would, so it stays faithful.
func (RunReconcileTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "run_reconcile",
		Description: "Check the integrated union of a sliced workload against the pinned shared " +
			"identifiers. Returns GATE-PASS when every pinned identifier is present in the slices " +
			"that define or reference it, GATE-FAIL when one is missing (cross-slice interface " +
			"drift), or GATE-ERROR on a fetch failure.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "branch": {"type": "string", "description": "the integration branch to check"},
  "slices": {"type": "array", "items": {"type": "object", "properties": {
    "name":  {"type": "string"},
    "files": {"type": "array", "items": {"type": "string"}}
  }}},
  "sharedIdentifiers": {"type": "array", "items": {"type": "object", "properties": {
    "id":           {"type": "string"},
    "definedBy":    {"type": "string"},
    "referencedBy": {"type": "array", "items": {"type": "string"}}
  }}}
},
"required": ["branch"]
}`),
	}
}

// Execute fetches + checks out the integration branch and runs pinned_check
// against it. It never returns a non-nil error for a content/transport failure:
// those map to a Terminal verdict so the AgenticTask reaches a terminal state.
func (t *RunReconcileTool) Execute(ctx context.Context, raw json.RawMessage) (*agent.ToolResult, error) {
	var a reconcileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return t.result(VerdictGateError, "bad args: "+err.Error(), nil), nil
	}
	if !refSafe(a.Branch) {
		return t.result(VerdictGateError, fmt.Sprintf("unsafe or empty integration branch %q", a.Branch), nil), nil
	}

	var authEnv []string
	if t.Token != nil {
		if tok, err := t.Token(); err == nil && tok != "" {
			if auth, aerr := repo.NewAuth(tok); aerr == nil {
				authEnv = auth.Env()
			}
		}
	}

	// Fetch the integration branch (pushed by the integrate step) and check it
	// out so pinned_check reads the union's files from the workspace.
	if err := t.runGit(ctx, authEnv, "fetch", "--end-of-options", "origin", a.Branch); err != nil {
		return t.result(VerdictGateError, "fetch integration branch: "+err.Error(), nil), nil
	}
	if err := t.runGit(ctx, nil, "checkout", "--force", "--end-of-options", "FETCH_HEAD"); err != nil {
		return t.result(VerdictGateError, "checkout integration branch: "+err.Error(), nil), nil
	}

	sliceFiles := make(map[string][]string, len(a.Slices))
	for _, s := range a.Slices {
		sliceFiles[s.Name] = s.Files
	}
	ids := make([]slicer.SharedIdentifier, 0, len(a.SharedIdentifiers))
	for _, si := range a.SharedIdentifiers {
		ids = append(ids, slicer.SharedIdentifier{
			ID:           si.ID,
			DefinedBy:    si.DefinedBy,
			ReferencedBy: si.ReferencedBy,
		})
	}

	drifts := slicer.PinnedCheck(ids, t.Workspace, sliceFiles)
	if len(drifts) == 0 {
		return t.result(VerdictGatePass,
			fmt.Sprintf("union interface-consistent: %d pinned identifiers present", len(ids)), nil), nil
	}
	return t.result(VerdictGateFail,
		fmt.Sprintf("interface drift: %d pinned identifier(s) missing from the slices that must carry them", len(drifts)),
		drifts), nil
}

// runGit shells one git subcommand in the workspace with the extra env.
func (t *RunReconcileTool) runGit(ctx context.Context, extraEnv []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = t.Workspace
	cmd.Env = append(cmd.Environ(), extraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *RunReconcileTool) result(verdict, summary string, drifts []slicer.Drift) *agent.ToolResult {
	out := map[string]any{"summary": summary}
	if drifts != nil {
		out["drifts"] = drifts
	}
	return &agent.ToolResult{Terminal: true, Verdict: verdict, Summary: summary, Output: out}
}
