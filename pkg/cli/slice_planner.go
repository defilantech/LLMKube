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

Reason SILENTLY. Do ALL of the analysis below in your own head. Your entire
reply is the single YAML document specified at the end and NOTHING else: no
preamble, no prose, no premise analysis, no "Now I'll create the slices",
nothing before the first YAML key or after the last line.

First, classify the premises (silently). A premise is any fact the slices'
correctness depends on. Classify each load-bearing premise:
- settled-in-repo: verifiable from the repository map or the issue itself. Fine.
- repo-verifiable: the answer lives in the repo or a vendored dependency (does a
  function exist, what is an API's real signature). Do not assume it: instruct the
  owning slice to verify it first, then implement against what it finds.
- externally-empirical: correctness depends on facts outside the repo that only
  running code or hardware can settle (whether a named tool or image works on the
  target hardware, the exact metrics a component emits at runtime, a live system's
  behavior). Treat issue hedging ("e.g.", "realistic path", "should be
  hands-verified", "likely", "should work") as a signal that a premise is
  externally-empirical, NOT settled fact.

Act on the classification:
- If the ENTIRE issue hinges on an externally-empirical premise, output only
  "UNSLICEABLE: <premise> requires empirical verification (<how>)" and stop.
- Otherwise, name each externally-empirical premise in the contract, and in the
  task of every slice whose correctness depends on it, instruct the slice to
  ground the premise from a real source or return NO-GO with outcome
  NEEDS-VERIFICATION rather than guessing. Never pin (below) an identifier whose
  real value is externally-empirical and unknown to you: a value you invented and
  pinned is worse than no pin.

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
   The reconcile check matches pins as WHOLE TOKENS (bounded on both sides by a
   non-identifier character). A pin like "rocm_smi_" will NEVER match anything
   because the trailing underscore is an identifier-continuation byte: the text
   "rocm_smi_sensor_temperature" contains the prefix as a substring but never as
   a whole token, so the pin is unsatisfiable and will always produce a
   pinned-missing GATE-FAIL. Never pin a prefix, a partial token, or anything
   that ends on an identifier-continuation byte (letter, digit, underscore).
   Pin only complete, standalone identifiers.
5. Keep the contract field as plain prose lines only: no markdown headers,
   bold, or nested bullets, so the YAML block scalar always parses cleanly.
6. Your whole reply is ONLY this YAML document, nothing before the first key
   or after the last line, in exactly this shape:

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
	// A chatty local planner can wrap the YAML in prose despite the prompt
	// (premise analysis, a "Now I'll create the slices:" lead-in, etc).
	// Surface an UNSLICEABLE refusal on whatever line it lands, then strip any
	// preamble before the first top-level `issue:` key so the YAML parses.
	if line, ok := unsliceableLine(body); ok {
		return slicePlan{}, fmt.Errorf("planner refused: %s", line)
	}
	body = stripToYAMLStart(body)
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

// unsliceableLine returns the first line whose trimmed text begins with
// "UNSLICEABLE" (case-insensitive), so the planner's refusal is surfaced even
// when a chatty model prefixes it with prose.
func unsliceableLine(body string) (string, bool) {
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(strings.ToUpper(t), "UNSLICEABLE") {
			return t, true
		}
	}
	return "", false
}

// stripToYAMLStart drops any preamble before the first line that begins the
// plan document (a top-level `issue:` key). If the body already starts there,
// or no such line exists, it is returned unchanged.
func stripToYAMLStart(body string) string {
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "issue:") {
			return strings.Join(lines[i:], "\n")
		}
	}
	return body
}
