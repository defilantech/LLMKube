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

package githubissue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRepo(t *testing.T) {
	cases := []struct {
		in        string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"defilantech/LLMKube", "defilantech", "LLMKube", false},
		{"owner/repo", "owner", "repo", false},
		{"", "", "", true},
		{"single", "", "", true},
		{"a/b/c", "", "", true},
		{"/empty-owner", "", "", true},
		{"empty-repo/", "", "", true},
	}
	for _, tc := range cases {
		o, r, err := ParseRepo(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: wantErr=%v got err=%v", tc.in, tc.wantErr, err)
		}
		if o != tc.wantOwner || r != tc.wantRepo {
			t.Errorf("%q: want (%q,%q) got (%q,%q)", tc.in, tc.wantOwner, tc.wantRepo, o, r)
		}
	}
}

// TestClient_Fetch_HappyPath wires the Client at an httptest server
// serving the canonical GitHub issue payload. Verifies the parsed
// fields, the Authorization header propagation, and the API headers.
func TestClient_Fetch_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header: want 'Bearer test-token' got %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Errorf("accept header: want vnd.github+json got %q", got)
		}
		if got := r.URL.Path; got != "/repos/defilantech/LLMKube/issues/510" {
			t.Errorf("path: want /repos/.../510 got %q", got)
		}
		_, _ = w.Write([]byte(`{
			"number": 510,
			"title": "[FEATURE] Mention make lint-all in AGENTS.md",
			"body": "After PR #508 lands the make lint-all target, point contributors at it from AGENTS.md.",
			"state": "open",
			"labels": [
				{"name": "documentation"},
				{"name": "enhancement"}
			]
		}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	iss, err := c.Fetch(context.Background(), "defilantech", "LLMKube", 510, "test-token")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if iss.Number != 510 {
		t.Errorf("number: got %d", iss.Number)
	}
	if !strings.Contains(iss.Title, "Mention make lint-all") {
		t.Errorf("title: got %q", iss.Title)
	}
	if !strings.Contains(iss.Body, "AGENTS.md") {
		t.Errorf("body: got %q", iss.Body)
	}
	if iss.State != "open" {
		t.Errorf("state: got %q", iss.State)
	}
	if len(iss.Labels) != 2 || iss.Labels[0] != "documentation" || iss.Labels[1] != "enhancement" {
		t.Errorf("labels: got %v", iss.Labels)
	}
}

// TestClient_Fetch_NoToken confirms an unauthenticated request omits
// the Authorization header entirely. The empty token is the documented
// "public repo, low rate-limit" path.
func TestClient_Fetch_NoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("auth header: want empty (anonymous) got %q", got)
		}
		_, _ = w.Write([]byte(`{"number": 1, "title": "t", "body": "b", "state": "open"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	if _, err := c.Fetch(context.Background(), "o", "r", 1, ""); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
}

// TestClient_Fetch_NotFound surfaces the 404 as an HTTPError. The
// executor uses IsNotFound to distinguish "issue is gone" from "token
// is wrong" and log appropriately.
func TestClient_Fetch_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL}
	_, err := c.Fetch(context.Background(), "o", "r", 999999, "")
	if err == nil {
		t.Fatal("expected error on 404")
	}
	var herr *HTTPError
	if !errors.As(err, &herr) {
		t.Fatalf("expected *HTTPError; got %T (%v)", err, err)
	}
	if !herr.IsNotFound() {
		t.Errorf("IsNotFound: got false")
	}
	if herr.IsUnauthorized() {
		t.Errorf("IsUnauthorized: should be false for a 404")
	}
}

// TestClient_Fetch_Unauthorized covers both 401 and 403; IsUnauthorized
// returns true for either so the executor's log line can suggest a
// token check.
func TestClient_Fetch_Unauthorized(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
		}))
		c := &Client{BaseURL: srv.URL}
		_, err := c.Fetch(context.Background(), "o", "r", 1, "bad")
		srv.Close()
		var herr *HTTPError
		if !errors.As(err, &herr) {
			t.Fatalf("status %d: expected *HTTPError; got %v", code, err)
		}
		if !herr.IsUnauthorized() {
			t.Errorf("status %d: IsUnauthorized should be true", code)
		}
	}
}

// TestClient_Fetch_TruncatesLargeBody caps a body bigger than BodyCap
// and appends the documented marker so the model knows there is more.
func TestClient_Fetch_TruncatesLargeBody(t *testing.T) {
	big := strings.Repeat("x", 20*1024) // 20 KiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"number":1,"title":"t","body":"` + big + `","state":"open"}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, BodyCap: 1024}
	iss, err := c.Fetch(context.Background(), "o", "r", 1, "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(iss.Body) > 1024 {
		t.Errorf("body should be truncated to ~1024; got %d", len(iss.Body))
	}
	if !strings.Contains(iss.Body, "truncated") {
		t.Errorf("truncation marker missing; got: %q", iss.Body[len(iss.Body)-100:])
	}
}

// TestClient_Fetch_RejectsBadArgs guards against zero / empty inputs.
func TestClient_Fetch_RejectsBadArgs(t *testing.T) {
	c := NewClient()
	for _, tc := range []struct {
		name   string
		owner  string
		repo   string
		number int
	}{
		{"empty-owner", "", "r", 1},
		{"empty-repo", "o", "", 1},
		{"zero-number", "o", "r", 0},
		{"negative-number", "o", "r", -1},
	} {
		_, err := c.Fetch(context.Background(), tc.owner, tc.repo, tc.number, "")
		if err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}
