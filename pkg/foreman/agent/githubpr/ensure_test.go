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

package githubpr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// prServer scripts the two endpoints EnsurePR touches: the head-filtered
// list and the create. existing controls the list response; createStatus
// and createBody script the POST.
func prServer(t *testing.T, existing []string, createStatus int, createBody string) (*Client, *[]string) {
	t.Helper()
	var posts []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head") != "o:foreman/wl-x/issue-7" {
			t.Errorf("head filter: got %q", r.URL.Query().Get("head"))
		}
		items := make([]map[string]string, 0, len(existing))
		for _, u := range existing {
			items = append(items, map[string]string{"html_url": u})
		}
		_ = json.NewEncoder(w).Encode(items)
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		b, _ := json.Marshal(req)
		posts = append(posts, string(b))
		w.WriteHeader(createStatus)
		_, _ = w.Write([]byte(createBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}, &posts
}

func TestEnsurePR_CreatesWhenAbsent(t *testing.T) {
	c, posts := prServer(t, nil, http.StatusCreated, `{"html_url":"https://github.com/o/r/pull/9"}`)

	res, err := c.EnsurePR(context.Background(), "o", "r",
		"foreman/wl-x/issue-7", "main", "Fix the thing", "Fixes #7", "tok")
	if err != nil {
		t.Fatalf("EnsurePR: %v", err)
	}
	if !res.Created || res.URL != "https://github.com/o/r/pull/9" {
		t.Fatalf("want created PR 9, got %+v", res)
	}
	if len(*posts) != 1 {
		t.Fatalf("want 1 create POST, got %d", len(*posts))
	}
	var req map[string]string
	_ = json.Unmarshal([]byte((*posts)[0]), &req)
	if req["head"] != "foreman/wl-x/issue-7" || req["base"] != "main" || req["title"] != "Fix the thing" {
		t.Errorf("create payload wrong: %v", req)
	}
}

func TestEnsurePR_ReusesExisting(t *testing.T) {
	c, posts := prServer(t, []string{"https://github.com/o/r/pull/4"}, http.StatusCreated, `{}`)

	res, err := c.EnsurePR(context.Background(), "o", "r",
		"foreman/wl-x/issue-7", "main", "t", "b", "tok")
	if err != nil {
		t.Fatalf("EnsurePR: %v", err)
	}
	if res.Created || res.URL != "https://github.com/o/r/pull/4" {
		t.Fatalf("want existing PR 4 reused, got %+v", res)
	}
	if len(*posts) != 0 {
		t.Fatalf("must not POST when a PR exists; got %d posts", len(*posts))
	}
}

func TestEnsurePR_ResolvesCreateRace(t *testing.T) {
	// List says absent, create 422s "already exists" (a concurrent
	// reviewer won), second list finds the winner.
	var listCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		listCalls++
		if listCalls == 1 {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_, _ = w.Write([]byte(`[{"html_url":"https://github.com/o/r/pull/11"}]`))
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"errors":[{"message":"A pull request already exists for o:foreman/wl-x/issue-7."}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}

	res, err := c.EnsurePR(context.Background(), "o", "r",
		"foreman/wl-x/issue-7", "main", "t", "b", "tok")
	if err != nil {
		t.Fatalf("EnsurePR after race: %v", err)
	}
	if res.Created || res.URL != "https://github.com/o/r/pull/11" {
		t.Fatalf("want race resolved to PR 11, got %+v", res)
	}
}

func TestEnsurePR_SurfacesAuthFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}

	_, err := c.EnsurePR(context.Background(), "o", "r", "h", "main", "t", "b", "tok")
	if err == nil {
		t.Fatal("want error on 403")
	}
}

func TestHeadCommitSubject(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/commits/foreman/wl-x/issue-7", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"commit":{"message":"feat: add the thing\n\nLonger body.\nFixes #7"}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}

	// Note: the ref is path-escaped by the client; httptest's mux
	// matches the unescaped path.
	got := c.HeadCommitSubject(context.Background(), "o", "r", "foreman/wl-x/issue-7", "tok")
	if got != "feat: add the thing" {
		t.Fatalf("subject: got %q", got)
	}
}

func TestHeadCommitSubject_EmptyOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := &Client{HTTPClient: srv.Client(), BaseURL: srv.URL}
	if got := c.HeadCommitSubject(context.Background(), "o", "r", "b", "t"); got != "" {
		t.Fatalf("want empty on 404, got %q", got)
	}
}
