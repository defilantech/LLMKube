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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
)

// plannerPrompt is the one-shot pinning planner. It decomposes an issue into
// disjoint-file slices and pins the exact cross-slice identifiers, so the union
// is interface-consistent. The two %s are the issue text and the repo map.
const plannerPrompt = `You are a planning agent. You are given a GitHub issue and a repository map.
Decompose the issue into 2 to 4 slices that can be implemented INDEPENDENTLY and
combined by concatenation.

Hard rules:
1. Disjoint files. Each slice owns a distinct set of files. No file may appear in
   two slices. If the issue cannot be split into disjoint-file slices, say so
   explicitly and stop: output only "UNSLICEABLE: <reason>".
2. Contract first. Before the slices, write a contract: the shared interfaces,
   signatures, config keys, and file boundaries all slices must agree on.
3. Each slice is small enough for a mid-size local model to finish in one pass.
4. Pin shared identifiers. For every identifier that crosses a slice boundary
   (metric name, config key, CRD field, function signature), pin the EXACT string
   in a shared_identifiers list with which slice defines it and which reference
   it. In each slice's task, name the exact identifiers that slice uses, VERBATIM.
5. Output VALID YAML in exactly this shape and nothing else:

issue: <number>
repo: <owner/name>
contract: |
  <shared interfaces and decisions>
shared_identifiers:
  - id: <exact string every slice must use verbatim>
    defined_by: <slice name>
    referenced_by: [<slice name>, ...]
slices:
  - name: <kebab-case>
    files:
      - <path>
    task: |
      <scoped instructions; name the exact shared_identifiers this slice uses>

Issue:
%s

Repository map:
%s
`

// PlannerCaller sends the planner prompt to a model and returns the completion.
// Defaults to an HTTP call against an OpenAI-compatible endpoint; tests inject
// a stub.
type PlannerCaller func(ctx context.Context, prompt string) (string, error)

// planIssue fetches the issue, builds the planner prompt, calls the planner
// model, and parses its SlicePlan. The CLI knows the real issue number and
// repo, so it overrides whatever the planner emitted for those (the planner
// routinely slips the header fields).
func planIssue(ctx context.Context, issue int32, opts *sliceOptions, call PlannerCaller) (slicePlan, error) {
	owner, repo, err := githubissue.ParseRepo(opts.repo)
	if err != nil {
		return slicePlan{}, fmt.Errorf("--repo: %w", err)
	}
	iss, err := githubissue.NewClient().Fetch(ctx, owner, repo, int(issue), os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return slicePlan{}, fmt.Errorf("fetch issue #%d: %w", issue, err)
	}
	repomap := ""
	if opts.repomapFile != "" {
		b, rerr := os.ReadFile(opts.repomapFile)
		if rerr != nil {
			return slicePlan{}, fmt.Errorf("read repomap %s: %w", opts.repomapFile, rerr)
		}
		repomap = string(b)
	}

	prompt := fmt.Sprintf(plannerPrompt, formatIssuePrompt(iss), repomap)
	raw, err := call(ctx, prompt)
	if err != nil {
		return slicePlan{}, fmt.Errorf("planner call: %w", err)
	}
	plan, err := parseSlicePlan(raw)
	if err != nil {
		return slicePlan{}, err
	}
	// The CLI is the source of truth for these; the planner slips them.
	plan.Issue = issue
	plan.Repo = opts.repo
	return plan, nil
}

var fenceRE = regexp.MustCompile("(?s)```(?:yaml)?\\s*(.*?)```")

// parseSlicePlan extracts the SlicePlan from a planner completion: it strips a
// code fence if present, surfaces an UNSLICEABLE refusal as an error, and
// unmarshals the YAML.
func parseSlicePlan(raw string) (slicePlan, error) {
	body := strings.TrimSpace(raw)
	if m := fenceRE.FindStringSubmatch(body); m != nil {
		body = strings.TrimSpace(m[1])
	}
	if strings.HasPrefix(strings.ToUpper(body), "UNSLICEABLE") {
		return slicePlan{}, fmt.Errorf("planner refused: %s", firstLine(body))
	}
	var p slicePlan
	if err := yaml.Unmarshal([]byte(body), &p); err != nil {
		return slicePlan{}, fmt.Errorf("parse planner output as a slice plan: %w", err)
	}
	if len(p.Slices) == 0 {
		return slicePlan{}, fmt.Errorf("planner output had no slices:\n%s", body)
	}
	return p, nil
}

// httpPlannerCall posts the prompt to an OpenAI-compatible /v1/chat/completions
// endpoint and returns the assistant message content.
func httpPlannerCall(url, model string) PlannerCaller {
	return func(ctx context.Context, prompt string) (string, error) {
		reqBody, _ := json.Marshal(map[string]any{
			"model":       model,
			"messages":    []map[string]string{{"role": "user", "content": prompt}},
			"temperature": 0.2,
			"max_tokens":  2400,
		})
		endpoint := strings.TrimSuffix(url, "/") + "/v1/chat/completions"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		client := &http.Client{Timeout: 10 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("planner endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}
		var parsed struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(b, &parsed); err != nil {
			return "", fmt.Errorf("decode planner response: %w", err)
		}
		if len(parsed.Choices) == 0 {
			return "", fmt.Errorf("planner response had no choices")
		}
		return parsed.Choices[0].Message.Content, nil
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
