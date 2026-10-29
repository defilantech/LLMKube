/*
Copyright 2026.

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

package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubpr"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubprfetch"
)

func TestInfer(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r.git":              "github",
		"https://GitHub.com/o/r":                  "github",
		"git@github.com:o/r.git":                  "github",
		"ssh://git@gitlab.com:2222/group/sub/p":   "gitlab",
		"https://codeberg.org/o/r":                "gitea",
		"https://git.example.internal/group/proj": "",
		"git@git.example.internal:group/proj.git": "",
		"":           "",
		"not a url":  "",
		"/local/dir": "",
	}
	for remote, want := range cases {
		if got := Infer(remote); got != want {
			t.Errorf("Infer(%q) = %q, want %q", remote, got, want)
		}
	}
}

func TestResolveSelection(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		want    string
		wantErr string
	}{
		{name: "default for an unknown host", cfg: Config{GitRemoteURL: "https://git.example.internal/o/r"}, want: "github"},
		{name: "default with no remote", cfg: Config{}, want: "github"},
		{name: "inferred from github.com", cfg: Config{GitRemoteURL: "git@github.com:o/r.git"}, want: "github"},
		{
			name: "explicit wins over the host",
			cfg:  Config{Name: "github", GitRemoteURL: "https://gitlab.com/o/r"},
			want: "github",
		},
		{
			name:    "explicit but not built in",
			cfg:     Config{Name: "gitea"},
			wantErr: `forge "gitea" (configured) is not built into this agent (available: github)`,
		},
		{
			name:    "inferred but not built in names the host and the override",
			cfg:     Config{GitRemoteURL: "https://gitlab.com/group/p"},
			wantErr: "inferred from the git remote host gitlab.com",
		},
		{
			name:    "relative API base",
			cfg:     Config{APIBaseURL: "ghe.example.com/api/v3"},
			wantErr: "must be an absolute http(s) URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := Resolve(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Resolve err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if p.Name != tc.want || p.CodeHost == nil || p.WorkItems == nil || p.PullRequests == nil {
				t.Errorf("Resolve = %+v, want provider %q with every seam", p, tc.want)
			}
		})
	}
}

// An API base points every GitHub client at a GitHub Enterprise Server, and
// clone URLs at that server's host, so a self-hosted instance is reachable
// at all (the clients' BaseURL had no production setter).
func TestGitHubEnterpriseBaseURL(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.URL.Path, "/api/v3/") {
			t.Errorf("request path %q is not under the configured /api/v3", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":7,"title":"from ghe"}`))
	}))
	defer srv.Close()

	p, err := Resolve(Config{Name: "github", APIBaseURL: srv.URL + "/api/v3/"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := p.CodeHost.ResolveCloneURL("org/repo"), srv.URL+"/org/repo.git"; got != want {
		t.Errorf("clone URL = %q, want %q on the enterprise host", got, want)
	}
	item, err := p.WorkItems.Get(context.Background(), "org/repo", "7")
	if err != nil || item.Title != "from ghe" {
		t.Fatalf("WorkItems.Get = %+v, %v; want the enterprise server's issue", item, err)
	}
	if hits.Load() == 0 {
		t.Fatal("the enterprise server was never called")
	}
	for name, base := range map[string]string{
		"githubpr":      p.CodeHost.(*codehost.GitHubCodeHost).Ensurer.(*githubpr.Client).BaseURL,
		"githubprfetch": p.PullRequests.(*githubprfetch.Client).BaseURL,
	} {
		if base != srv.URL+"/api/v3" {
			t.Errorf("%s BaseURL = %q, want %q", name, base, srv.URL+"/api/v3")
		}
	}
}

func TestGitHubPublicDefaults(t *testing.T) {
	p, err := Resolve(Config{Name: "github"})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.CodeHost.ResolveCloneURL("o/r"); got != "https://github.com/o/r.git" {
		t.Errorf("clone URL = %q, want github.com", got)
	}
	if base := p.PullRequests.(*githubprfetch.Client).BaseURL; base != "" {
		t.Errorf("PR client BaseURL = %q, want the public default", base)
	}
}
