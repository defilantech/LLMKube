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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathEscapesWorkspace is returned by resolveInside when the supplied
// path resolves to a location outside the workspace root, whether by
// absolute path, parent traversal, or symlink. Tools surface this error
// as the dispatch failure so the model sees a clear "do not do that".
var ErrPathEscapesWorkspace = errors.New("path escapes workspace")

// resolveInside returns the absolute path of p resolved against the
// workspace dir, after enforcing:
//
//  1. p is not absolute.
//  2. After Clean+Join, the result is still under the workspace.
//  3. Following symlinks (if the path exists) does not escape the
//     workspace.
//
// For paths that do not exist yet (e.g. write_file creating a new file),
// the Join-based containment check is authoritative; symlink resolution
// is skipped. The workspace itself is EvalSymlinks-resolved so a
// symlinked workspace is treated as its real path.
func resolveInside(workspace, p string) (string, error) {
	wsAbs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("workspace abs(%s): %w", workspace, err)
	}
	wsResolved, err := filepath.EvalSymlinks(wsAbs)
	if err != nil {
		return "", fmt.Errorf("workspace evalsymlinks(%s): %w", wsAbs, err)
	}

	// Accept absolute paths that live under the workspace root. Strip the
	// workspace prefix and continue with the same containment checks as
	// for a relative path. Absolute paths outside the workspace are still
	// rejected so the model learns the rule.
	if filepath.IsAbs(p) {
		cleaned := filepath.Clean(p)
		rel, relErr := filepath.Rel(wsResolved, cleaned)
		if relErr != nil {
			return "", fmt.Errorf("rel(%s,%s): %w", wsResolved, cleaned, relErr)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%w: absolute paths must be inside the workspace", ErrPathEscapesWorkspace)
		}
		p = rel
	}

	joined := filepath.Clean(filepath.Join(wsResolved, p))
	rel, err := filepath.Rel(wsResolved, joined)
	if err != nil {
		return "", fmt.Errorf("rel(%s,%s): %w", wsResolved, joined, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: paths must be workspace-relative", ErrPathEscapesWorkspace)
	}

	// Only check symlink escape if the path actually exists. Unborn paths
	// (write_file) are validated by the Join-based check above.
	if _, statErr := os.Lstat(joined); statErr == nil {
		resolved, evalErr := filepath.EvalSymlinks(joined)
		if evalErr != nil {
			return "", fmt.Errorf("evalsymlinks(%s): %w", joined, evalErr)
		}
		relResolved, relErr := filepath.Rel(wsResolved, resolved)
		if relErr != nil {
			return "", fmt.Errorf("rel-resolved(%s,%s): %w", wsResolved, resolved, relErr)
		}
		if relResolved == ".." || strings.HasPrefix(relResolved, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("%w: %q resolves outside via symlink", ErrPathEscapesWorkspace, p)
		}
	}
	return joined, nil
}
