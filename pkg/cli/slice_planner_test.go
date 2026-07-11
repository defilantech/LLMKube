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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const planYAMLBody = `issue: 0
repo: wrong/repo
contract: |
  agree on names
shared_identifiers:
  - id: foo_metric
    defined_by: a
    referenced_by: [b]
slices:
  - name: a
    files: [x.yaml]
    task: define foo_metric
  - name: b
    files: [y.json]
    task: use foo_metric
`

func TestParseSlicePlan(t *testing.T) {
	t.Run("bare yaml", func(t *testing.T) {
		p, err := parseSlicePlan(planYAMLBody)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(p.Slices) != 2 || p.SharedIdentifiers[0].ID != "foo_metric" {
			t.Fatalf("parsed wrong: %+v", p)
		}
	})

	t.Run("fenced yaml", func(t *testing.T) {
		p, err := parseSlicePlan("here you go:\n```yaml\n" + planYAMLBody + "```\n")
		if err != nil {
			t.Fatalf("parse fenced: %v", err)
		}
		if len(p.Slices) != 2 {
			t.Fatalf("fenced parse wrong: %+v", p)
		}
	})

	t.Run("unsliceable", func(t *testing.T) {
		_, err := parseSlicePlan("UNSLICEABLE: the change is one tangled file")
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("want refusal error, got %v", err)
		}
	})

	t.Run("prose preamble before yaml", func(t *testing.T) {
		// A chatty local planner prints its reasoning before the document.
		chatty := "Looking at this issue, let me classify the premises first.\n\n" +
			"**Premises:**\n1. foo is settled-in-repo\n\nNow I'll create the slices:\n\n" +
			planYAMLBody
		p, err := parseSlicePlan(chatty)
		if err != nil {
			t.Fatalf("parse with preamble: %v", err)
		}
		if len(p.Slices) != 2 {
			t.Fatalf("preamble parse wrong: %+v", p)
		}
	})

	t.Run("unsliceable behind preamble", func(t *testing.T) {
		_, err := parseSlicePlan("Let me analyze.\n\nUNSLICEABLE: hardware verification required")
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("want refusal error surfaced from a non-prefix line, got %v", err)
		}
	})

	t.Run("no slices", func(t *testing.T) {
		if _, err := parseSlicePlan("issue: 1\nrepo: a/b\n"); err == nil {
			t.Fatal("want error for a plan with no slices")
		}
	})
}

func TestHTTPPlannerCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s, want /v1/chat/completions", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": "```yaml\n" + planYAMLBody + "```"}},
			},
		})
	}))
	defer srv.Close()

	out, err := httpPlannerCall(srv.URL, "planner")(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(out, "foo_metric") {
		t.Fatalf("content missing plan: %q", out)
	}

	// non-200 surfaces an error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()
	if _, err := httpPlannerCall(bad.URL, "m")(context.Background(), "p"); err == nil {
		t.Fatal("want error on non-200")
	}
}

func TestResolveSlicePlan_RequiresInput(t *testing.T) {
	// no --plan and no issue arg -> error.
	if _, err := resolveSlicePlan(context.Background(), nil, &sliceOptions{}); err == nil {
		t.Fatal("want error when neither --plan nor ISSUE is given")
	}
	// issue arg without --repo/--planner-url -> error.
	if _, err := resolveSlicePlan(context.Background(), []string{"700"}, &sliceOptions{}); err == nil {
		t.Fatal("want error for issue without --repo/--planner-url")
	}
	// non-numeric issue -> error.
	badOpts := &sliceOptions{repo: "a/b", plannerURL: "u"}
	if _, err := resolveSlicePlan(context.Background(), []string{"abc"}, badOpts); err == nil {
		t.Fatal("want error for non-numeric issue")
	}
}
