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

	"github.com/defilantech/llmkube/pkg/foreman/agent"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
	"github.com/defilantech/llmkube/pkg/foreman/agent/oai"
)

// TokenSource resolves a GitHub PAT at FetchIssueTool.Execute time.
// Production wires repo.TokenFromEnvOrFile; tests pass a closure that
// returns a fixed string (or an error to exercise the fallback path).
//
// We resolve per call rather than caching the token on the tool struct
// so:
//
//  1. The credential never sits in the tool's memory between calls.
//  2. Rotating ~/.config/foreman/github-token while the foreman-agent
//     is running takes effect on the next tool call.
//  3. The tool stays a clean read-only surface even if its instance
//     leaks into a debugger or transcript.
type TokenSource func() (string, error)

// FetchIssueTool is the reviewer's read-only path to a GitHub issue.
// It wraps the same githubissue.Client the executor already uses for
// the coder-side pre-fetch (PR #572) and exposes it as a first-class
// tool so the reviewer never needs a separately-authed `gh` CLI on
// its FleetNode.
//
// Why this exists (issue #580): the v0.4 reviewer prompt mandated
// `bash("gh issue view {issue} --repo {repo}")` as the scope anchor.
// On any FleetNode where `gh` was not authed (e.g. the Mac Studio
// reviewer node during rerun-10), that call returned an auth-error
// stub via stderr/exit-4. The reviewer kept going with empty issue
// context, fell through to "the diff looks fine in isolation", and
// emitted APPROVE verdicts on diffs whose scope it had never actually
// checked against the issue body. The fix is to stop subshelling `gh`
// and instead expose one bounded GitHub surface (this tool) using the
// same token the foreman-agent already loads at startup.
//
// Security shape vs the GH_TOKEN-env alternative:
//
//   - Read-only by construction: exactly one
//     GET /repos/{owner}/{repo}/issues/{number} per Execute. The
//     model has no path to gh pr create / gh repo delete / generic
//     authenticated REST writes through this tool.
//   - The token lives behind a TokenSource closure; the struct
//     stores no credentials between calls.
//   - Containerised foreman-agents do not need `gh` installed in
//     their image; pure Go HTTP suffices.
//   - Token rotation surface is one file (~/.config/foreman/github-token)
//     instead of also a per-user `gh` config or per-process env var
//     inherited by every bash subprocess.
type FetchIssueTool struct {
	// Fetcher is the GitHub client. Required. Production wires
	// githubissue.NewClient(); tests pass a fake or an httptest-backed
	// Client.
	Fetcher githubissue.Fetcher
	// Token resolves the GitHub PAT at Execute time. Required.
	Token TokenSource
}

type fetchIssueArgs struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// Name returns the tool name as advertised to the model.
func (t *FetchIssueTool) Name() string { return "fetch_issue" }

// Schema returns the OAI schema advertisement.
func (t *FetchIssueTool) Schema() oai.ToolSchemaDef {
	return oai.ToolSchemaDef{
		Name: "fetch_issue",
		Description: "Read a GitHub issue's title, body, state, and labels. " +
			"Use this as the scope anchor before reviewing a diff: a " +
			"verbatim sentence from the issue body is the right citation " +
			"for whether the diff addresses what was asked. Returns " +
			"title, body (truncated to ~16 KiB if needed), state, labels.",
		Parameters: json.RawMessage(`{
"type": "object",
"properties": {
  "repo":   {"type": "string", "description": "owner/name, e.g. \"defilantech/LLMKube\"."},
  "number": {"type": "integer", "minimum": 1, "description": "Issue number."}
},
"required": ["repo", "number"]
}`),
	}
}

// Execute reads the GitHub issue at args.repo / args.number. All failure
// modes are surfaced via the returned error, which the loop emits as
// the tool-result content the model sees on its next turn.
//
//   - bad args (missing repo, non-positive number, malformed repo
//     string): wrapped fmt.Errorf naming the field.
//   - token resolution failure: wrapped error citing the env var /
//     file the operator is expected to populate; this points at the
//     foreman-agent host, not the model.
//   - 404: "issue not found" so the model can choose to ERROR out
//     rather than guess.
//   - 401 / 403: "unauthorized" with a hint that the token is wrong
//     or out of scope.
//   - 5xx / network: generic transient failure; the model can retry
//     once or call submit_result with verdict=ERROR.
func (t *FetchIssueTool) Execute(ctx context.Context, args json.RawMessage) (*agent.ToolResult, error) {
	if t.Fetcher == nil {
		return nil, fmt.Errorf("fetch_issue: tool not configured (Fetcher is nil)")
	}
	if t.Token == nil {
		return nil, fmt.Errorf("fetch_issue: tool not configured (Token resolver is nil)")
	}
	var a fetchIssueArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, fmt.Errorf("fetch_issue: bad args: %w", err)
	}
	if a.Repo == "" {
		return nil, fmt.Errorf("fetch_issue: repo is required (owner/name)")
	}
	if a.Number <= 0 {
		return nil, fmt.Errorf("fetch_issue: number must be a positive integer (got %d)", a.Number)
	}
	owner, name, err := githubissue.ParseRepo(a.Repo)
	if err != nil {
		return nil, fmt.Errorf("fetch_issue: %w", err)
	}
	token, err := t.Token()
	if err != nil {
		return nil, fmt.Errorf(
			"fetch_issue: no GitHub token "+
				"(set GITHUB_TOKEN env or populate "+
				"~/.config/foreman/github-token on the FleetNode): %w", err)
	}
	issue, err := t.Fetcher.Fetch(ctx, owner, name, a.Number, token)
	if err != nil {
		var herr *githubissue.HTTPError
		if errors.As(err, &herr) {
			switch {
			case herr.IsNotFound():
				return nil, fmt.Errorf("fetch_issue: issue %s#%d not found", a.Repo, a.Number)
			case herr.IsUnauthorized():
				return nil, fmt.Errorf(
					"fetch_issue: unauthorized for %s#%d; "+
						"foreman-agent's GitHub token may be missing the repo scope, "+
						"or the issue is in a private repo not visible to it",
					a.Repo, a.Number)
			}
		}
		return nil, fmt.Errorf("fetch_issue: %w", err)
	}
	return &agent.ToolResult{
		Output: map[string]any{
			"repo":   a.Repo,
			"number": issue.Number,
			"title":  issue.Title,
			"body":   issue.Body,
			"state":  issue.State,
			"labels": issue.Labels,
		},
	}, nil
}
