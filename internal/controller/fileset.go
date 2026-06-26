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

package controller

import (
	"fmt"
	"path"
	"strings"
)

// StagingPlan describes the resolved set of model files to stage from a source.
type StagingPlan struct {
	// Primary is the first file, passed to the runtime as the model path.
	Primary string
	// Files is the ordered, deduplicated list of all files to stage.
	Files []string
	// Mmproj is the multimodal projector file, if set.
	Mmproj string
}

// ResolveFileSet turns declared spec.files and spec.mmproj into a staging plan.
// It validates the primary file resolves to exactly one match, expands globs
// against repoFileList, deduplicates entries, and appends mmproj if present.
func ResolveFileSet(files []string, mmproj string, repoFileList []string) (*StagingPlan, error) {
	if len(files) == 0 && mmproj == "" {
		return nil, nil
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("spec.files must include a primary model file when spec.mmproj is set")
	}

	primaryMatches, err := expandFilePattern(files[0], repoFileList)
	if err != nil {
		return nil, fmt.Errorf("resolve primary file %q: %w", files[0], err)
	}
	if len(primaryMatches) != 1 {
		return nil, fmt.Errorf("primary file %q must resolve to exactly one file, matched %d", files[0], len(primaryMatches))
	}

	seen := map[string]struct{}{}
	resolved := make([]string, 0, len(files)+1)
	for _, entry := range files {
		matches, err := expandFilePattern(entry, repoFileList)
		if err != nil {
			return nil, fmt.Errorf("resolve file %q: %w", entry, err)
		}
		for _, match := range matches {
			if _, ok := seen[match]; ok {
				continue
			}
			seen[match] = struct{}{}
			resolved = append(resolved, match)
		}
	}

	if mmproj != "" {
		if err := validateRepoRelativePath(mmproj); err != nil {
			return nil, fmt.Errorf("spec.mmproj: %w", err)
		}
		if hasGlob(mmproj) {
			return nil, fmt.Errorf("spec.mmproj must be a concrete repo-relative path, got glob %q", mmproj)
		}
		if len(repoFileList) > 0 && !containsString(repoFileList, mmproj) {
			return nil, fmt.Errorf("spec.mmproj %q was not found in repository file list", mmproj)
		}
		if _, ok := seen[mmproj]; !ok {
			resolved = append(resolved, mmproj)
		}
	}

	return &StagingPlan{Primary: primaryMatches[0], Files: resolved, Mmproj: mmproj}, nil
}

// validateRepoRelativePath rejects empty, absolute, and path-traversal paths.
func validateRepoRelativePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
		return fmt.Errorf("invalid repo-relative path %q", p)
	}
	return nil
}

func expandFilePattern(entry string, repoFileList []string) ([]string, error) {
	if err := validateRepoRelativePath(entry); err != nil {
		return nil, err
	}
	if !hasGlob(entry) {
		// When repoFileList is nil or empty we skip existence validation.
		// The controller may call ResolveFileSet before HF listing completes.
		if len(repoFileList) > 0 && !containsString(repoFileList, entry) {
			return nil, fmt.Errorf("file %q was not found in repository file list", entry)
		}
		return []string{entry}, nil
	}

	if repoFileList == nil {
		return nil, fmt.Errorf("glob pattern %q requires repository file listing, which is unavailable in production: use explicit file paths", entry)
	}

	matches := make([]string, 0)
	for _, candidate := range repoFileList {
		matched, err := path.Match(entry, candidate)
		if err != nil {
			return nil, fmt.Errorf("glob %q: %w", entry, err)
		}
		if matched {
			matches = append(matches, candidate)
		}
	}
	// A secondary glob that matches zero files is not an error when a repository
	// file listing is available. Without a listing, globs are rejected above so
	// production reconcile does not silently skip typoed patterns.
	return matches, nil
}

// stagedCachePath returns the absolute cache path for a repo-relative file.
func stagedCachePath(modelDir string, repoRelative string) string {
	return path.Join(modelDir, repoRelative)
}

// hasGlob reports whether value contains path.Match wildcard characters.
// The set covers the three patterns recognized by path.Match: * (match any
// sequence), ? (match any single character), and [ (start character class).
func hasGlob(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
