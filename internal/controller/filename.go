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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
)

// modelFileExt is the on-disk extension used for downloaded model files.
const modelFileExt = ".gguf"

// legacyModelFilename is the basename historical versions of the controller
// wrote downloaded files as. New downloads prefer a metadata-derived name;
// legacy files are migrated in place on next reconcile.
const legacyModelFilename = "model" + modelFileExt

// fallbackModelFilename is the slug used when neither GGUF metadata nor
// Model.metadata.name yields a non-empty sanitized name.
const fallbackModelFilename = "model"

// sanitizeModelFilename converts an arbitrary string into a slug safe to use
// as a filename basename. The rule set:
//   - Characters outside [A-Za-z0-9._-] are replaced with '-'
//   - Runs of '-' are collapsed
//   - Leading and trailing '-', '.', and '_' are trimmed (no path-traversal)
//   - Returns "" if the input is empty after sanitization
func sanitizeModelFilename(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := b.String()
	out = strings.Trim(out, "-._")
	// Collapse any remaining double-dash runs that survived the trim.
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return out
}

// canonicalModelBasename returns the desired GGUF filename basename (without
// directory) for a Model, in priority order:
//  1. sanitized Model.Status.GGUF.ModelName
//  2. sanitized Model.Name
//  3. fallbackModelFilename
//
// Always includes the .gguf extension.
func canonicalModelBasename(model *inferencev1alpha1.Model) string {
	var name string
	if model.Status.GGUF != nil {
		name = sanitizeModelFilename(model.Status.GGUF.ModelName)
	}
	if name == "" {
		name = sanitizeModelFilename(model.Name)
	}
	if name == "" {
		name = fallbackModelFilename
	}
	return name + modelFileExt
}

// canonicalModelPath joins the model cache directory with the canonical
// basename for a Model.
func canonicalModelPath(modelDir string, model *inferencev1alpha1.Model) string {
	return filepath.Join(modelDir, canonicalModelBasename(model))
}

// findCachedModelFile looks for a single .gguf file in modelDir and returns
// (path, fileInfo, true) if found. If the canonical filename and the legacy
// "model.gguf" both exist, it prefers the canonical one. If multiple .gguf
// files exist for any other reason, it returns the lexicographically first.
func findCachedModelFile(modelDir string) (string, os.FileInfo, bool) {
	matches, err := filepath.Glob(filepath.Join(modelDir, "*"+modelFileExt))
	if err != nil || len(matches) == 0 {
		return "", nil, false
	}
	sort.Strings(matches)
	pick := matches[0]
	for _, m := range matches {
		if filepath.Base(m) != legacyModelFilename {
			pick = m
			break
		}
	}
	info, err := os.Stat(pick)
	if err != nil {
		return "", nil, false
	}
	return pick, info, true
}

// migrateModelFilename renames currentPath to the canonical path for the given
// model, returning the path the file resides at after the operation. If the
// file is already at the canonical path, currentPath is returned unchanged.
// Caller should populate model.Status.GGUF before calling so the metadata-
// derived name is preferred. If the canonical path already exists alongside
// currentPath (shouldn't happen, but be safe), currentPath is returned and a
// warning is logged.
func (r *ModelReconciler) migrateModelFilename(currentPath, modelDir string, model *inferencev1alpha1.Model) (string, error) {
	logger := log.FromContext(context.Background())
	desired := canonicalModelPath(modelDir, model)
	if currentPath == desired {
		return currentPath, nil
	}
	if _, err := os.Stat(desired); err == nil {
		logger.Info("Canonical model path already exists; not overwriting", "current", currentPath, "canonical", desired)
		return currentPath, nil
	}
	if err := os.Rename(currentPath, desired); err != nil {
		return currentPath, fmt.Errorf("rename %s -> %s: %w", currentPath, desired, err)
	}
	logger.Info("Migrated model file to canonical filename", "from", currentPath, "to", desired)
	return desired, nil
}
