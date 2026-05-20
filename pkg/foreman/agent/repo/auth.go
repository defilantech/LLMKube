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

// Package repo holds the git-shelling helpers Foreman uses to clone a
// source repo, branch off it, commit the model's changes with a DCO
// sign-off, and push the branch to a fork. The package never directly
// invokes the bash tool; it owns its own bounded os/exec calls so the
// agent loop's transcript stays readable (one shell line, one tool
// result) instead of being dominated by git plumbing.
package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoToken means we could not locate a GitHub token in either the
// GITHUB_TOKEN env var or the local config file. Phase E's executor
// surfaces this as a clean "auth not configured" failure rather than
// the model trying to recover with bash tool calls.
var ErrNoToken = errors.New("repo: no GitHub token found (set GITHUB_TOKEN env or ~/.config/foreman/github-token)")

// Auth bundles a GitHub token and the GIT_ASKPASS scaffolding git needs
// to use it. Build one with NewAuth(); call Env() to compose the env
// for exec.Cmd; call Close() to remove the on-disk askpass helper when
// the run is done.
//
// The token never lands on disk: the askpass script just echoes the
// FOREMAN_GIT_TOKEN env var, which we set on the git child process.
// So the script's contents are static and reusable; only the env var
// carries the secret.
type Auth struct {
	Token string

	// askpassPath is a tmp script git invokes when prompted for a
	// credential. It reads FOREMAN_GIT_TOKEN and prints it on stdout.
	askpassPath string
}

// TokenFromEnvOrFile reads $GITHUB_TOKEN first, then falls back to
// ~/.config/foreman/github-token. Returns ErrNoToken if neither yields
// a non-empty value.
func TokenFromEnvOrFile() (string, error) {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("repo: home dir: %w", err)
	}
	path := filepath.Join(home, ".config", "foreman", "github-token")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: documented config-file path under $HOME
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNoToken
		}
		return "", fmt.Errorf("repo: read %s: %w", path, err)
	}
	t := strings.TrimSpace(string(raw))
	if t == "" {
		return "", ErrNoToken
	}
	return t, nil
}

// askpassScript is the tiny shell helper git invokes when prompted for
// credentials. It echoes FOREMAN_GIT_TOKEN so the secret is never on
// disk; only the static script is. gosec flags "credentials" on any
// constant containing "TOKEN"; this string is the *reader*, not the
// secret itself.
//
//nolint:gosec // G101: askpass reader script, not a hardcoded credential
const askpassScript = `#!/bin/sh
exec printf "%s" "$FOREMAN_GIT_TOKEN"
`

// NewAuth builds an Auth from the given token. The token is *not*
// validated against GitHub here; the first git operation that needs
// it will surface a clear error if the token is wrong or expired.
//
// Pass tokenSource="" to read the token from $GITHUB_TOKEN or
// ~/.config/foreman/github-token (the production / local-dev path).
func NewAuth(tokenSource string) (*Auth, error) {
	token := strings.TrimSpace(tokenSource)
	if token == "" {
		t, err := TokenFromEnvOrFile()
		if err != nil {
			return nil, err
		}
		token = t
	}

	f, err := os.CreateTemp("", "foreman-askpass-*.sh")
	if err != nil {
		return nil, fmt.Errorf("repo: create askpass: %w", err)
	}
	if _, err := f.WriteString(askpassScript); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("repo: write askpass: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("repo: close askpass: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o700); err != nil { //nolint:gosec // G302: askpass must be executable by owner
		_ = os.Remove(f.Name())
		return nil, fmt.Errorf("repo: chmod askpass: %w", err)
	}
	return &Auth{Token: token, askpassPath: f.Name()}, nil
}

// Env returns the env-var assignments needed for a git exec.Cmd to find
// the askpass helper and skip any terminal prompt fallback. The token
// rides in FOREMAN_GIT_TOKEN, which the askpass script reads.
func (a *Auth) Env() []string {
	return []string{
		"GIT_ASKPASS=" + a.askpassPath,
		"GIT_TERMINAL_PROMPT=0",
		"FOREMAN_GIT_TOKEN=" + a.Token,
	}
}

// Close removes the on-disk askpass helper. Safe to call multiple times.
func (a *Auth) Close() error {
	if a == nil || a.askpassPath == "" {
		return nil
	}
	err := os.Remove(a.askpassPath)
	a.askpassPath = ""
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
