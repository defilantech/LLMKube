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
	"os/exec"
	"regexp"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
	"github.com/defilantech/llmkube/pkg/foreman/agent/repo"
	"github.com/defilantech/llmkube/pkg/foreman/slicer"
)

// RunIntegrateTool unions a sliced Workload's disjoint slice branches onto the
// current base on a fresh integration branch and pushes it, so a downstream
// reconcile task can check the union for cross-slice interface drift. It runs
// on the deterministic (no-LLM) path: the executor dispatches it directly and
// its Terminal verdict drives the AgenticTask status. Part of Sliced Workloads
// (#1033).
//
// The slice branches live on origin (the fork the coders pushed to); the base
// lives on the upstream project (the #813 base resolution). So the tool fetches
// from two remotes into local refs before the local union, then pushes the
// integration branch back to origin.
type RunIntegrateTool struct {
	// Workspace is the cloned fork working tree (origin = the fork). The union
	// and the fetches happen here.
	Workspace string
	// Token sources the git token for the fetches + push. Production wires
	// repo.TokenFromEnvOrFile; a nil Token skips auth (public repos, tests
	// against local remotes).
	Token func() (string, error)
	// Run overrides the git transport for the local union (slicer.Integrate).
	// Nil uses slicer's exec default. Tests inject a runner; the fetch/push the
	// tool does itself always shell real git in Workspace.
	Run slicer.GitRunner
}

// integrateSlice is one slice's dispatch view: its pushed branch and the files
// it owns (files are unused by integrate but travel in the same payload shape
// the reconcile step consumes).
type integrateSlice struct {
	Name   string   `json:"name"`
	Branch string   `json:"branch"`
	Files  []string `json:"files"`
}

type integrateArgs struct {
	BaseBranch  string           `json:"baseBranch"`
	Branch      string           `json:"branch"`
	UpstreamURL string           `json:"upstreamURL"`
	Slices      []integrateSlice `json:"slices"`
}

func (RunIntegrateTool) Name() string { return "run_integrate" }

// Schema advertises the tool. The deterministic executor path never shows this
// to a model, but a future LLM-driven wrapper would, so it stays faithful.
func (RunIntegrateTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "run_integrate",
		Description: "Union the disjoint slice branches of a sliced workload onto the current base " +
			"on a fresh integration branch and push it. Returns GATE-PASS on a clean union, " +
			"GATE-FAIL when slices overlap or a slice does not apply onto the base, or " +
			"GATE-ERROR on a fetch/push failure.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "branch":      {"type": "string", "description": "integration branch to create and push"},
  "baseBranch":  {"type": "string", "description": "base ref to union onto (default main)"},
  "upstreamURL": {"type": "string", "description": "git URL the base ref is fetched from"},
  "slices": {"type": "array", "items": {"type": "object", "properties": {
    "name":   {"type": "string"},
    "branch": {"type": "string", "description": "the slice's pushed branch on origin"},
    "files":  {"type": "array", "items": {"type": "string"}}
  }}, "description": "the disjoint slices to union"}
},
"required": ["branch", "slices"]
}`),
	}
}

var localRefUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

var refAllowed = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// refSafe reports whether s is safe to pass to git as a ref/positional
// argument: non-empty, not an option (no leading '-'), no ".." traversal, and
// only git-ref-safe characters. Mirrors repo.gitRefSafe so a value beginning
// with '-' cannot be smuggled in as a git flag (argv injection, e.g.
// --upload-pack=<cmd>). Shared with run_reconcile.
func refSafe(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") &&
		!strings.Contains(s, "..") && refAllowed.MatchString(s)
}

// gitURLSafe reports whether s is a safe git remote URL: not an option and a
// known scheme, so it cannot be read as a git flag.
func gitURLSafe(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://", "git@", "file://", "/"} {
		if strings.HasPrefix(s, scheme) {
			return true
		}
	}
	return false
}

// Execute fetches the base + each slice into local refs, unions them via
// slicer.Integrate, and pushes the integration branch. It never returns a
// non-nil error for a content/transport failure: those map to a Terminal
// GATE-FAIL/GATE-ERROR verdict so the AgenticTask reaches a terminal state.
func (t *RunIntegrateTool) Execute(ctx context.Context, raw json.RawMessage) (*agent.ToolResult, error) {
	var a integrateArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return t.gateError("bad args: " + err.Error()), nil
	}
	if a.Branch == "" || len(a.Slices) == 0 {
		return t.gateError("run_integrate requires branch and at least one slice"), nil
	}
	// Validate every user-influenced value before it reaches git argv, so a
	// value beginning with '-' cannot be smuggled in as a git flag.
	if !refSafe(a.Branch) {
		return t.gateError(fmt.Sprintf("unsafe integration branch %q", a.Branch)), nil
	}
	base := a.BaseBranch
	if base == "" {
		base = "main"
	}
	if !refSafe(base) {
		return t.gateError(fmt.Sprintf("unsafe base branch %q", base)), nil
	}
	if a.UpstreamURL != "" && !gitURLSafe(a.UpstreamURL) {
		return t.gateError(fmt.Sprintf("unsafe upstream url %q", a.UpstreamURL)), nil
	}

	var authEnv []string
	if t.Token != nil {
		if tok, err := t.Token(); err == nil && tok != "" {
			if auth, aerr := repo.NewAuth(tok); aerr == nil {
				authEnv = auth.Env()
			}
		}
	}

	// 1. Fetch the base from the upstream project into a local ref (origin is
	// the fork; the base lives upstream). An empty upstreamURL means the base
	// is already a local ref (tests, or a same-repo flow).
	const baseLocal = "_slicer_base"
	if a.UpstreamURL != "" {
		if v := t.fetchToLocal(ctx, authEnv, a.UpstreamURL, base, baseLocal); v != nil {
			return v, nil
		}
		base = baseLocal
	}

	// 2. Fetch each slice branch from origin into a local ref.
	sliceRefs := make([]string, 0, len(a.Slices))
	for _, s := range a.Slices {
		if !refSafe(s.Branch) {
			return t.gateError(fmt.Sprintf("slice %q has an unsafe or empty branch %q", s.Name, s.Branch)), nil
		}
		local := "_slicer_slice_" + localRefUnsafe.ReplaceAllString(s.Name, "-")
		if v := t.fetchToLocal(ctx, authEnv, "origin", s.Branch, local); v != nil {
			return v, nil
		}
		sliceRefs = append(sliceRefs, local)
	}

	// 3. Union onto the base. slicer.Integrate proves disjointness first, then
	// applies each slice's diff.
	res, err := slicer.Integrate(ctx, slicer.IntegrateOptions{
		RepoDir: t.Workspace,
		Base:    base,
		Branch:  a.Branch,
		Slices:  sliceRefs,
		Run:     t.Run,
	})
	if err != nil {
		var overlap *slicer.OverlapError
		var apply *slicer.ApplyError
		var empty *slicer.EmptyUnionError
		if errors.As(err, &overlap) || errors.As(err, &apply) || errors.As(err, &empty) {
			// A slice violated its file scope, was cut from a stale base, or the
			// union is empty: a real content failure the coder/planner must fix.
			return t.gateFail(err.Error()), nil
		}
		// Anything else is a git/transport fault: retryable infrastructure.
		return t.gateError("integrate: " + err.Error()), nil
	}

	// 4. Push the integration branch to origin for the reconcile step + PR.
	if err := t.runGit(ctx, authEnv, "push", "--force-with-lease", "--end-of-options", "origin", a.Branch); err != nil {
		return t.gateError("push integration branch: " + err.Error()), nil
	}

	return &agent.ToolResult{
		Terminal: true,
		Verdict:  VerdictGatePass,
		Summary:  fmt.Sprintf("integrated %d slices into %s", len(sliceRefs), a.Branch),
		Output: map[string]any{
			"branch": res.Branch,
			"owners": res.Owners,
		},
	}, nil
}

// fetchToLocal fetches remoteRef from remote and pins it to a local branch.
// Returns a non-nil GATE-ERROR ToolResult on failure, nil on success.
func (t *RunIntegrateTool) fetchToLocal(
	ctx context.Context, env []string, remote, remoteRef, localBranch string,
) *agent.ToolResult {
	// --end-of-options is defense in depth on top of the refSafe/gitURLSafe
	// validation in Execute: even a validated value is treated as positional.
	if err := t.runGit(ctx, env, "fetch", "--end-of-options", remote, remoteRef); err != nil {
		return t.gateError(fmt.Sprintf("fetch %s %s: %s", remote, remoteRef, err.Error()))
	}
	if err := t.runGit(ctx, env, "branch", "-f", localBranch, "FETCH_HEAD"); err != nil {
		return t.gateError(fmt.Sprintf("pin %s: %s", localBranch, err.Error()))
	}
	return nil
}

// runGit shells one git subcommand in the workspace with the extra env.
func (t *RunIntegrateTool) runGit(ctx context.Context, extraEnv []string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = t.Workspace
	cmd.Env = append(cmd.Environ(), extraEnv...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (t *RunIntegrateTool) gateFail(msg string) *agent.ToolResult {
	return &agent.ToolResult{Terminal: true, Verdict: VerdictGateFail, Summary: msg,
		Output: map[string]any{"error": msg}}
}

func (t *RunIntegrateTool) gateError(msg string) *agent.ToolResult {
	return &agent.ToolResult{Terminal: true, Verdict: VerdictGateError, Summary: msg,
		Output: map[string]any{"error": msg}}
}
