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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
)

// fakeFetcher lets a test inject a canned outcome without spinning up
// an httptest server. Used for the failure-shape and arg-validation
// cases; the body-truncation and authorization-header cases go through
// a real Client + httptest server below.
type fakeFetcher struct {
	gotOwner  string
	gotRepo   string
	gotNumber int
	gotToken  string
	issue     *githubissue.Issue
	err       error
}

func (f *fakeFetcher) Fetch(
	_ context.Context, owner, repo string, number int, token string,
) (*githubissue.Issue, error) {
	f.gotOwner, f.gotRepo, f.gotNumber, f.gotToken = owner, repo, number, token
	return f.issue, f.err
}

func staticToken(t string) TokenSource {
	return func() (string, error) { return t, nil }
}

func errToken(msg string) TokenSource {
	return func() (string, error) { return "", errors.New(msg) }
}

// TestFetchIssueTool_HappyPath verifies the tool round-trips a parsed
// issue through a real githubissue.Client backed by httptest, and that
// the Authorization header carries the token the TokenSource returns.
func TestFetchIssueTool_HappyPath(t *testing.T) {
	body := strings.Repeat("body line\n", 5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t-happy" {
			t.Errorf("auth header: want 'Bearer t-happy' got %q", got)
		}
		if r.URL.Path != "/repos/defilantech/LLMKube/issues/526" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
		  "number": 526,
		  "title": "[BUG] host-ip auto-detect picks unreachable virtual interface",
		  "body": %q,
		  "state": "open",
		  "labels": [{"name":"bug"},{"name":"foreman"}]
		}`, body)
	}))
	defer srv.Close()

	tool := &FetchIssueTool{
		Fetcher: &githubissue.Client{BaseURL: srv.URL},
		Token:   staticToken("t-happy"),
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"defilantech/LLMKube","number":526}`))
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	out, _ := res.Output.(map[string]any)
	if out["number"] != 526 {
		t.Errorf("number: want 526 got %v", out["number"])
	}
	if !strings.Contains(out["title"].(string), "host-ip") {
		t.Errorf("title: %v", out["title"])
	}
	if !strings.Contains(out["body"].(string), "body line") {
		t.Errorf("body: %v", out["body"])
	}
	labels, _ := out["labels"].([]string)
	if len(labels) != 2 || labels[0] != "bug" || labels[1] != "foreman" {
		t.Errorf("labels: %v", labels)
	}
}

func TestFetchIssueTool_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	tool := &FetchIssueTool{
		Fetcher: &githubissue.Client{BaseURL: srv.URL},
		Token:   staticToken("t"),
	}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"o/r","number":99999}`))
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("404 error should name 'not found': %v", err)
	}
}

func TestFetchIssueTool_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	tool := &FetchIssueTool{
		Fetcher: &githubissue.Client{BaseURL: srv.URL},
		Token:   staticToken("bad-token"),
	}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"o/r","number":1}`))
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("401 error should name 'unauthorized': %v", err)
	}
}

// TestFetchIssueTool_ArgValidation exercises the four arg-shape failures
// in one table: bad JSON, missing repo, non-positive number, malformed
// repo string. The fake fetcher is never called on these paths.
func TestFetchIssueTool_ArgValidation(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"bad-json", `not-json`, "bad args"},
		{"missing-repo", `{"number":1}`, "repo is required"},
		{"zero-number", `{"repo":"o/r","number":0}`, "number must be a positive integer"},
		{"negative-number", `{"repo":"o/r","number":-3}`, "number must be a positive integer"},
		{"malformed-repo", `{"repo":"no-slash","number":1}`, "owner/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ff := &fakeFetcher{}
			tool := &FetchIssueTool{Fetcher: ff, Token: staticToken("t")}
			_, err := tool.Execute(context.Background(), json.RawMessage(tc.args))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error: want substring %q, got %v", tc.want, err)
			}
			if ff.gotNumber != 0 {
				t.Errorf("fetcher should not have been called on arg-validation failure; called with number=%d", ff.gotNumber)
			}
		})
	}
}

func TestFetchIssueTool_TokenResolutionFailure(t *testing.T) {
	ff := &fakeFetcher{}
	tool := &FetchIssueTool{Fetcher: ff, Token: errToken("askpass not configured")}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"o/r","number":1}`))
	if err == nil {
		t.Fatal("expected error on token resolution failure")
	}
	if !strings.Contains(err.Error(), "no GitHub token") {
		t.Errorf("token error should name 'no GitHub token': %v", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("token error should hint at GITHUB_TOKEN env var: %v", err)
	}
	if ff.gotNumber != 0 {
		t.Errorf("fetcher should not have been called when token resolution fails; called with number=%d", ff.gotNumber)
	}
}

func TestFetchIssueTool_NilDepsFailLoud(t *testing.T) {
	cases := []struct {
		name string
		tool *FetchIssueTool
		want string
	}{
		{"nil-fetcher", &FetchIssueTool{Fetcher: nil, Token: staticToken("t")}, "Fetcher is nil"},
		{"nil-token", &FetchIssueTool{Fetcher: &fakeFetcher{}, Token: nil}, "Token resolver is nil"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.tool.Execute(context.Background(), json.RawMessage(`{"repo":"o/r","number":1}`))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error: want substring %q, got %v", tc.want, err)
			}
		})
	}
}

// TestFetchIssueTool_Schema verifies the OAI advertisement is wellformed
// JSONSchema and names the two required args. The loop sends this
// schema to the model verbatim; a typo here would be invisible until
// the model decided not to call the tool at all.
func TestFetchIssueTool_Schema(t *testing.T) {
	tool := &FetchIssueTool{Fetcher: &fakeFetcher{}, Token: staticToken("t")}
	if tool.Name() != "fetch_issue" {
		t.Errorf("Name(): %q", tool.Name())
	}
	schema := tool.Schema()
	if schema.Name != "fetch_issue" {
		t.Errorf("schema.Name: %q", schema.Name)
	}
	var parsed map[string]any
	if err := json.Unmarshal(schema.Parameters, &parsed); err != nil {
		t.Fatalf("schema parameters not valid JSON: %v", err)
	}
	required, _ := parsed["required"].([]any)
	if len(required) != 2 {
		t.Fatalf("required should have 2 fields, got %v", required)
	}
}

// TestFetchIssueTool_FetcherReceivesParsedRepo confirms ParseRepo's
// owner+name split flows through to the Fetcher call. A regression
// where the tool passed the full "owner/name" string as owner is the
// kind of bug that only surfaces against a real GitHub API; this test
// pins the contract early.
func TestFetchIssueTool_FetcherReceivesParsedRepo(t *testing.T) {
	ff := &fakeFetcher{
		issue: &githubissue.Issue{Number: 42, Title: "t", Body: "b", State: "open"},
	}
	tool := &FetchIssueTool{Fetcher: ff, Token: staticToken("tok")}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"defilantech/LLMKube","number":42}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if ff.gotOwner != "defilantech" || ff.gotRepo != "LLMKube" || ff.gotNumber != 42 || ff.gotToken != "tok" {
		t.Errorf("fetcher args: owner=%q repo=%q number=%d token=%q",
			ff.gotOwner, ff.gotRepo, ff.gotNumber, ff.gotToken)
	}
}
