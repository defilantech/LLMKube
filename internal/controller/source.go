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
	"strings"
)

// isPVCSource returns true if the source uses the pvc:// scheme.
func isPVCSource(source string) bool {
	return strings.HasPrefix(source, "pvc://")
}

// parsePVCSource extracts the PVC claim name and file path from a pvc:// source.
// Format: pvc://claim-name/path/to/model.gguf
func parsePVCSource(source string) (claimName, path string, err error) {
	if !isPVCSource(source) {
		return "", "", fmt.Errorf("not a PVC source: %s", source)
	}

	// Strip the pvc:// prefix
	rest := strings.TrimPrefix(source, "pvc://")
	if rest == "" {
		return "", "", fmt.Errorf("empty PVC source: %s", source)
	}

	// Split into claim name and path
	slashIdx := strings.Index(rest, "/")
	if slashIdx < 0 {
		return "", "", fmt.Errorf("PVC source must include a file path: %s (expected pvc://claim-name/path/to/model.gguf)", source)
	}

	claimName = rest[:slashIdx]
	path = rest[slashIdx+1:]

	if claimName == "" {
		return "", "", fmt.Errorf("PVC source has empty claim name: %s", source)
	}
	if path == "" {
		return "", "", fmt.Errorf("PVC source has empty file path: %s", source)
	}

	return claimName, path, nil
}

// isLocalSource returns true if the source is a local file (file:// URL or absolute path).
func isLocalSource(source string) bool {
	return strings.HasPrefix(source, "file://") || strings.HasPrefix(source, "/")
}

// getLocalPath extracts the filesystem path from a local source.
func getLocalPath(source string) string {
	if strings.HasPrefix(source, "file://") {
		return strings.TrimPrefix(source, "file://")
	}
	return source
}

// isRemoteHTTPSource reports whether source is an http:// or https:// URL.
// These sources are downloaded by the inference Pod's init container into the
// per-namespace model cache PVC, not by the Model controller. Downloading in
// the controller's pod writes to the operator-namespace PVC, which is not
// visible to Pods in user namespaces (PVCs cannot be cross-namespace mounted),
// so the controller defers the actual fetch to the workload.
func isRemoteHTTPSource(source string) bool {
	return strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://")
}

// isHFRepoSource reports whether source looks like a HuggingFace repo ID
// (e.g., "TinyLlama/TinyLlama-1.1B-Chat-v1.0", "Qwen/Qwen3.6-35B-A3B").
// These sources are downloaded by the runtime (vLLM) at startup, not by
// the Model controller.
//
// Criteria:
//
//	Not a URL (no "://" scheme)
//	Not an absolute path (doesn't start with "/")
//	Not a PVC source (handled separately)
//	Contains at least one "/" separator (HF convention: owner/repo)
//	Matches Hugging Face's permitted character set
func isHFRepoSource(source string) bool {
	if source == "" {
		return false
	}
	if isPVCSource(source) {
		return false
	}
	if isLocalSource(source) {
		return false
	}
	if strings.Contains(source, "://") {
		return false
	}
	if !strings.Contains(source, "/") {
		return false
	}
	// Match HF's permitted character set: alphanumeric, hyphens, underscores,
	// dots, and forward slashes. Must start with alphanumeric.
	for i, c := range source {
		if i == 0 {
			if !isAlphaNum(c) {
				return false
			}
			continue
		}
		if !isAlphaNum(c) && c != '-' && c != '_' && c != '.' && c != '/' {
			return false
		}
	}
	return true
}

func isAlphaNum(c rune) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	}
	return false
}
