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
	"net/url"
	"strings"

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubpr"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubprfetch"
	"github.com/defilantech/llmkube/pkg/foreman/agent/worktracker"
)

func init() { Register("github", newGitHub) }

// newGitHub builds the GitHub provider. An APIBaseURL points every client at
// a GitHub Enterprise Server, and clone URLs at that server's host (its API
// lives at https://<host>/api/v3).
func newGitHub(cfg Config) (Provider, error) {
	pr := githubpr.NewClient()
	issues := githubissue.NewClient()
	pulls := githubprfetch.NewClient()
	cloneBase := ""
	if cfg.APIBaseURL != "" {
		base := strings.TrimRight(cfg.APIBaseURL, "/")
		pr.BaseURL, issues.BaseURL, pulls.BaseURL = base, base, base
		cloneBase = gitHubCloneBase(base)
	}
	return Provider{
		CodeHost:     &codehost.GitHubCodeHost{Ensurer: pr, TokenSource: cfg.TokenSource, CloneBaseURL: cloneBase},
		WorkItems:    &worktracker.GitHubWorkItems{Fetcher: issues, TokenSource: cfg.TokenSource},
		PullRequests: pulls,
	}, nil
}

// gitHubCloneBase is the web root for an API base: https://github.com for the
// public API, else the API's scheme and host.
func gitHubCloneBase(apiBase string) string {
	u, err := url.Parse(apiBase)
	if err != nil || strings.EqualFold(u.Hostname(), "api.github.com") {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
