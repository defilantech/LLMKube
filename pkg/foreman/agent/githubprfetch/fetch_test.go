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

package githubprfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// isPRDetailsPath returns true when the path is the PR details endpoint
// (not reviews, comments, or check-runs sub-paths).
func isPRDetailsPath(path string) bool {
	return strings.Contains(path, "/pulls/") &&
		!strings.Contains(path, "/reviews") &&
		!strings.Contains(path, "/comments") &&
		!strings.Contains(path, "/check-runs")
}

func TestClient_Fetch_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t-happy" {
			t.Errorf("auth header: want 'Bearer t-happy' got %q", got)
		}
		switch {
		case isPRDetailsPath(r.URL.Path):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
		  "number": 42,
		  "title": "fix: something",
		  "body": "PR body text",
		  "state": "open",
		  "merged": false,
		  "head_ref": "fix/something",
		  "base_ref": "main"
		}`)
		case strings.Contains(r.URL.Path, "/reviews"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[{"user":{"login":"reviewer1"},`+
				`"state":"APPROVED","body":"LGTM",`+
				`"submitted_at":"2025-01-01T00:00:00Z"}]`)
		case strings.Contains(r.URL.Path, "/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[{"user":{"login":"reviewer1"},`+
				`"path":"pkg/foo.go","line":10,`+
				`"original_line":10,"body":"fix this"}]`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"check_runs":[{"name":"lint",`+
				`"conclusion":"success",`+
				`"details_url":"https://example.com"}]}`)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	pr, err := c.Fetch(context.Background(), "defilantech", "LLMKube", 42, "t-happy")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("Number: want 42 got %d", pr.Number)
	}
	if pr.Title != "fix: something" {
		t.Errorf("Title: want 'fix: something' got %q", pr.Title)
	}
	if pr.Body != "PR body text" {
		t.Errorf("Body: want 'PR body text' got %q", pr.Body)
	}
	if pr.State != "open" {
		t.Errorf("State: want 'open' got %q", pr.State)
	}
	if pr.Merged {
		t.Errorf("Merged: want false got true")
	}
	if pr.HeadRef != "fix/something" {
		t.Errorf("HeadRef: want 'fix/something' got %q", pr.HeadRef)
	}
	if pr.BaseRef != "main" {
		t.Errorf("BaseRef: want 'main' got %q", pr.BaseRef)
	}
	if len(pr.Reviews) != 1 || pr.Reviews[0].Author != "reviewer1" {
		t.Errorf("Reviews: %v", pr.Reviews)
	}
	if len(pr.ReviewComments) != 1 || pr.ReviewComments[0].Path != "pkg/foo.go" {
		t.Errorf("ReviewComments: %v", pr.ReviewComments)
	}
	if len(pr.CheckRuns) != 1 || pr.CheckRuns[0].Name != "lint" {
		t.Errorf("CheckRuns: %v", pr.CheckRuns)
	}
}

func TestClient_Fetch_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	_, err := c.Fetch(context.Background(), "o", "r", 99999, "t")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Errorf("error should name HTTP 404: %v", err)
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if !herr.IsNotFound() {
		t.Errorf("IsNotFound should be true")
	}
}

func TestClient_Fetch_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	_, err := c.Fetch(context.Background(), "o", "r", 1, "bad-token")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError, got %T", err)
	}
	if !herr.IsUnauthorized() {
		t.Errorf("IsUnauthorized should be true")
	}
}

func TestClient_Fetch_BestEffortSubFetches(t *testing.T) {
	// PR details succeed, but reviews endpoint returns 500.
	// The overall fetch should still succeed with empty reviews.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isPRDetailsPath(r.URL.Path):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
		  "number": 42,
		  "title": "fix: something",
		  "body": "PR body text",
		  "state": "open",
		  "merged": false,
		  "head_ref": "fix/something",
		  "base_ref": "main"
		}`)
		case strings.Contains(r.URL.Path, "/reviews"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.Contains(r.URL.Path, "/comments"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[]`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"check_runs":[]}`)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	pr, err := c.Fetch(context.Background(), "o", "r", 42, "t")
	if err != nil {
		t.Fatalf("Fetch should succeed even when sub-fetches fail: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("Number: want 42 got %d", pr.Number)
	}
	if len(pr.Reviews) != 0 {
		t.Errorf("Reviews should be empty on sub-fetch failure: %v", pr.Reviews)
	}
}

func TestClient_Fetch_Validation(t *testing.T) {
	c := NewClient()
	_, err := c.Fetch(context.Background(), "", "r", 1, "t")
	if err == nil {
		t.Fatal("expected error on empty owner")
	}
	_, err = c.Fetch(context.Background(), "o", "", 1, "t")
	if err == nil {
		t.Fatal("expected error on empty repo")
	}
	_, err = c.Fetch(context.Background(), "o", "r", 0, "t")
	if err == nil {
		t.Fatal("expected error on zero number")
	}
	_, err = c.Fetch(context.Background(), "o", "r", -1, "t")
	if err == nil {
		t.Fatal("expected error on negative number")
	}
}

func TestHTTPError_IsNotFound(t *testing.T) {
	e := &HTTPError{StatusCode: http.StatusNotFound}
	if !e.IsNotFound() {
		t.Error("IsNotFound should be true for 404")
	}
	e2 := &HTTPError{StatusCode: http.StatusOK}
	if e2.IsNotFound() {
		t.Error("IsNotFound should be false for 200")
	}
}

func TestHTTPError_IsUnauthorized(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		e := &HTTPError{StatusCode: code}
		if !e.IsUnauthorized() {
			t.Errorf("IsUnauthorized should be true for %d", code)
		}
	}
	e2 := &HTTPError{StatusCode: http.StatusOK}
	if e2.IsUnauthorized() {
		t.Error("IsUnauthorized should be false for 200")
	}
}

func TestParseRepo(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"owner/name", "defilantech/LLMKube", "defilantech", "LLMKube", false},
		{"group/subgroup/project", "group/subgroup/project", "group/subgroup", "project", false},
		{"no-slash", "noslash", "", "", true},
		{"empty-segment", "owner/", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, err := ParseRepo(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if owner != tc.wantOwner || repo != tc.wantRepo {
				t.Errorf("ParseRepo(%q) = %q, %q; want %q, %q",
					tc.input, owner, repo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestTruncateBody(t *testing.T) {
	// Short body: no truncation.
	short := "short body"
	if got := truncateBody(short, 1024); got != short {
		t.Errorf("short body: got %q, want %q", got, short)
	}
	// Long body: truncated with marker.
	long := strings.Repeat("x", 2048)
	got := truncateBody(long, 1024)
	if len(got) > 1024 {
		t.Errorf("truncated body too long: %d bytes", len(got))
	}
	if !strings.Contains(got, "[PR body truncated") {
		t.Errorf("truncated body missing marker: %q", got)
	}
	// Zero cap: uses default.
	got2 := truncateBody(long, 0)
	if len(got2) > DefaultBodyCap {
		t.Errorf("zero cap body too long: %d bytes", len(got2))
	}
}
