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

// Package forge selects the code-hosting provider (GitHub, Gitea, GitLab,
// ...) the Foreman agent works against and builds its seams (#1300).
// Providers register a constructor under a name; Resolve picks one from
// explicit configuration, else the git remote's host, else GitHub.
package forge

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubprfetch"
	"github.com/defilantech/llmkube/pkg/foreman/agent/worktracker"
)

// DefaultName is the provider used when nothing selects one, so an install
// that predates provider selection keeps working unchanged.
const DefaultName = "github"

// Config selects and configures a provider.
type Config struct {
	// Name is the explicitly configured provider. Empty infers it.
	Name string
	// APIBaseURL is the provider's API root, for a self-hosted instance
	// (for example https://ghe.example.com/api/v3). Empty means the
	// provider's public default.
	APIBaseURL string
	// GitRemoteURL is the agent's --git-remote-url, used to infer the
	// provider from its host when Name is empty.
	GitRemoteURL string
	// TokenSource resolves the provider credential on every call.
	TokenSource func() (string, error)
}

// Provider is the set of seams one forge provides.
type Provider struct {
	// Name is the resolved provider name.
	Name      string
	CodeHost  codehost.CodeHost
	WorkItems worktracker.WorkItems
	// PullRequests reads a pull request with its reviews, review comments
	// and check runs, for the fetch_pull_request tool. Nil when the provider
	// cannot; the tool then reports codehost.ErrNotSupported.
	PullRequests githubprfetch.Fetcher
}

// Constructor builds a provider's seams from its configuration.
type Constructor func(Config) (Provider, error)

var (
	mu       sync.RWMutex
	registry = map[string]Constructor{}
)

// Register adds a provider under name. It panics on an empty or duplicate
// name: both are programming errors in the agent binary.
func Register(name string, c Constructor) {
	mu.Lock()
	defer mu.Unlock()
	if name == "" || c == nil {
		panic("forge: Register needs a name and a constructor")
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("forge: provider %q registered twice", name))
	}
	registry[name] = c
}

// Names lists the registered providers, sorted.
func Names() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// knownHosts maps public forge hosts to their provider, so a remote on one
// of them needs no configuration. A self-hosted instance's hostname carries
// no signal and must be named explicitly.
var knownHosts = map[string]string{
	"github.com":   "github",
	"gitlab.com":   "gitlab",
	"codeberg.org": "gitea",
}

// Infer returns the provider a git remote URL's host implies, or "" when the
// host is not a known public forge (or the URL does not parse).
func Infer(gitRemoteURL string) string {
	return knownHosts[strings.ToLower(remoteHost(gitRemoteURL))]
}

// Resolve picks the provider for cfg and builds it: an explicit Name wins,
// then the git remote's host, then DefaultName. A name that is configured or
// inferred but not built into this agent is an error rather than a silent
// GitHub fallback, which would push to and open pull requests on the wrong
// forge.
func Resolve(cfg Config) (Provider, error) {
	name, how := cfg.Name, "configured"
	if name == "" {
		if name = Infer(cfg.GitRemoteURL); name != "" {
			how = "inferred from the git remote host " + remoteHost(cfg.GitRemoteURL)
		} else {
			name, how = DefaultName, "default"
		}
	}
	if cfg.APIBaseURL != "" {
		if err := validateBaseURL(cfg.APIBaseURL); err != nil {
			return Provider{}, err
		}
	}
	mu.RLock()
	c, ok := registry[name]
	mu.RUnlock()
	if !ok {
		hint := ""
		if how != "configured" {
			hint = "; set --forge to the provider this remote is on"
		}
		return Provider{}, fmt.Errorf("forge %q (%s) is not built into this agent (available: %s)%s",
			name, how, strings.Join(Names(), ", "), hint)
	}
	p, err := c(cfg)
	if err != nil {
		return Provider{}, fmt.Errorf("forge %q: %w", name, err)
	}
	p.Name = name
	return p, nil
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("forge API base URL %q must be an absolute http(s) URL", raw)
	}
	return nil
}

// remoteHost is the host of a git remote: https://host/..., ssh://user@host:port/...,
// or the scp-like user@host:path. Empty when none parses.
func remoteHost(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		return u.Hostname()
	}
	// scp-like: [user@]host:path
	hostPart, _, ok := strings.Cut(remote, ":")
	if !ok {
		return ""
	}
	if _, h, at := strings.Cut(hostPart, "@"); at {
		hostPart = h
	}
	if strings.Contains(hostPart, "/") {
		return ""
	}
	return hostPart
}
