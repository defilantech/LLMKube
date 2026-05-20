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

package repo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// CloneOptions configures Clone.
type CloneOptions struct {
	// RemoteURL is the source URL to clone from. HTTPS preferred for
	// the GIT_ASKPASS auth path; SSH works too but expects the
	// foreman-agent's host key to be configured.
	RemoteURL string

	// Dest is the local directory to clone into. Must not exist yet,
	// or must be empty.
	Dest string

	// Ref is the branch, tag, or SHA to check out after cloning. Empty
	// uses the remote's default branch (HEAD).
	Ref string

	// Auth, when non-nil, provides the GIT_ASKPASS scaffolding. Public
	// clones can leave it nil.
	Auth *Auth
}

// Clone clones RemoteURL into Dest. The repository is cloned in full
// (no --depth) because the coder agent's grep tool may need history;
// shallow cloning is a v0.2 optimization once the cost surfaces.
//
// After clone, if Ref is set, the helper checks it out. The clone
// completes with HEAD pointing at the requested ref.
func Clone(ctx context.Context, opts CloneOptions) error {
	if opts.RemoteURL == "" {
		return fmt.Errorf("Clone: RemoteURL is required")
	}
	if opts.Dest == "" {
		return fmt.Errorf("Clone: Dest is required")
	}

	// Refuse to clone into an existing non-empty dir; this protects
	// the foreman-agent from being pointed at $HOME by mistake.
	if entries, err := os.ReadDir(opts.Dest); err == nil && len(entries) > 0 {
		return fmt.Errorf("Clone: dest %q is not empty", opts.Dest)
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Clone: stat dest %q: %w", opts.Dest, err)
	}

	parent := filepath.Dir(opts.Dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("Clone: mkdir parent %q: %w", parent, err)
	}

	env := []string{
		"HOME=" + envOr("HOME", "/tmp"),
	}
	if opts.Auth != nil {
		env = append(env, opts.Auth.Env()...)
	}

	if _, err := runGit(ctx, "", env, "clone", opts.RemoteURL, opts.Dest); err != nil {
		return err
	}
	if opts.Ref != "" {
		if _, err := runGit(ctx, opts.Dest, env, "checkout", opts.Ref); err != nil {
			return fmt.Errorf("Clone: checkout %q: %w", opts.Ref, err)
		}
	}
	return nil
}
