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

package controller

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// StagingPlan describes the resolved set of model files to stage from a source.
type StagingPlan struct {
	// Primary is the first file, passed to the runtime as the model path.
	Primary string
	// Files is the ordered, deduplicated list of all files to stage.
	Files []string
	// Sources maps cache-relative destinations to explicitly supplied URLs.
	Sources map[string]string
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

	seen := map[string]string{}
	var sources map[string]string
	resolved := make([]string, 0, len(files)+1)
	for _, entry := range files {
		matches, err := expandFilePattern(entry, repoFileList)
		if err != nil {
			return nil, fmt.Errorf("resolve file %q: %w", entry, err)
		}
		for _, match := range matches {
			name, err := fileEntryPath(match)
			if err != nil {
				return nil, err
			}
			if previous, ok := seen[path.Clean(name)]; ok {
				if previous != match {
					return nil, fmt.Errorf("files %q and %q stage to the same path %q", previous, match, name)
				}
				continue
			}
			seen[path.Clean(name)] = match
			if name != match {
				if sources == nil {
					sources = make(map[string]string)
				}
				sources[name] = match
			}
			resolved = append(resolved, name)
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
		if original, ok := seen[path.Clean(mmproj)]; ok && original != mmproj {
			return nil, fmt.Errorf("spec.mmproj conflicts with file %q", original)
		} else if !ok {
			resolved = append(resolved, mmproj)
		}
	}

	return &StagingPlan{Primary: resolved[0], Files: resolved, Sources: sources, Mmproj: mmproj}, nil
}

// validateRepoRelativePath rejects empty, absolute, and path-traversal paths.
func validateRepoRelativePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.ContainsAny(p, "\t\r\n") {
		return fmt.Errorf("invalid repo-relative path %q", p)
	}
	return nil
}

func expandFilePattern(entry string, repoFileList []string) ([]string, error) {
	if strings.Contains(entry, "://") {
		if _, err := fileEntryPath(entry); err != nil {
			return nil, err
		}
		return []string{entry}, nil
	}
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
		return nil, fmt.Errorf("glob pattern %q requires a repository listing, which only an hf://, huggingface.co, or HF_ENDPOINT mirror repo source provides: use explicit file paths", entry)
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

// fileEntryPath preserves HF repository paths and S3 keys; other HTTP files
// use their basename. URLs never become filesystem paths themselves.
func fileEntryPath(entry string) (string, error) {
	if !strings.Contains(entry, "://") {
		return entry, validateRepoRelativePath(entry)
	}
	u, err := url.Parse(entry)
	if err != nil {
		return "", fmt.Errorf("invalid file URL %q: %w", entry, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "s3") || u.Host == "" || u.User != nil || u.Fragment != "" || strings.ContainsAny(entry, "\t\r\n") {
		return "", fmt.Errorf("file entry must be an http(s) or s3 object URL: %q", entry)
	}
	name := path.Base(u.Path)
	if u.Scheme == "s3" {
		name = strings.TrimPrefix(u.Path, "/")
	} else if isHuggingFaceURL(entry) {
		parts := strings.SplitN(strings.TrimPrefix(u.Path, "/"), "/", 5)
		if len(parts) != 5 || parts[2] != "resolve" {
			return "", fmt.Errorf("HuggingFace file URL must name a /resolve/revision/file: %q", entry)
		}
		name = parts[4]
	}
	if strings.HasSuffix(u.Path, "/") || name == "." || hasGlob(name) {
		return "", fmt.Errorf("file URL must name a concrete file: %q", entry)
	}
	if err := validateRepoRelativePath(name); err != nil {
		return "", err
	}
	return name, nil
}

func (p *StagingPlan) entries() []string {
	entries := append([]string(nil), p.Files...)
	for i, name := range entries {
		if source, ok := p.Sources[name]; ok {
			entries[i] = source
		}
	}
	return entries
}
