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

// checkEmbeddedArtifacts validates fenced yaml/yml code blocks in changed
// Markdown files. For each changed *.md file it:
//  1. Parses every ```yaml / ```yml fenced block.
//  2. Validates each block body with sigs.k8s.io/yaml (unmarshal into
//     map[string]any or []any; only a dual-failure is a real parse error).
//  3. For blocks that are Kubernetes manifests (have both "apiVersion" and
//     "kind"), runs `kubectl --dry-run=client -f <tmpfile>` if kubectl is
//     on PATH.
//
// The check is conservative: only yaml/yml-fenced blocks are examined; no
// other content is judged. It is fail-open (a git or read error skips the
// file rather than blocking the coder).
package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	sigsyaml "sigs.k8s.io/yaml"
)

// checkEmbeddedArtifacts is the gate check function registered in
// gateCheckRegistry. It validates embedded YAML/manifest blocks in changed
// Markdown files and returns (true, output) on parse or validation failures.
func checkEmbeddedArtifacts(ctx context.Context, workspace string, run commandRunner) (failed bool, output string) {
	changed, err := changedMarkdownFiles(ctx, workspace, run)
	if err != nil || len(changed) == 0 {
		return false, ""
	}

	var findings []string
	for _, path := range changed {
		content, err := run(ctx, workspace, nil, "git", "show", ":"+path)
		if err != nil {
			continue // fail-open: skip files we cannot read
		}
		findings = append(findings, validateMarkdownYAML(ctx, workspace, path, content, run)...)
	}

	if len(findings) == 0 {
		return false, ""
	}
	return true, strings.Join(findings, "\n") + "\n"
}

// changedMarkdownFiles returns the workspace-relative paths of changed *.md
// files using `git diff --name-only --diff-filter=d HEAD -- *.md`. The
// --diff-filter=d excludes deleted files (their content is gone; no point
// parsing them). Returns nil on git error (fail-open).
func changedMarkdownFiles(ctx context.Context, workspace string, run commandRunner) ([]string, error) {
	out, err := run(ctx, workspace, nil, "git", "diff", "--name-only", "--diff-filter=d", "HEAD", "--", "*.md")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && strings.HasSuffix(line, ".md") {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// validateMarkdownYAML extracts and validates every yaml/yml fenced block in
// content. Each finding is a "file:line - reason" string.
func validateMarkdownYAML(ctx context.Context, workspace, path, content string, run commandRunner) []string {
	var findings []string

	// Track the current line as we scan through the content.
	lines := strings.Split(content, "\n")

	// We need to find fenced blocks AND record their start line numbers.
	// Walk line-by-line to capture start positions accurately.
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		infoString := ""
		if strings.HasPrefix(trimmed, "```yaml") {
			infoString = "yaml"
		} else if strings.HasPrefix(trimmed, "```yml") {
			infoString = "yml"
		}
		if infoString == "" {
			continue
		}
		// Found an opening fence. Collect body until closing ```.
		startLine := i + 1 // 1-based line number of the opening fence
		i++
		var bodyLines []string
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "```" {
				break
			}
			bodyLines = append(bodyLines, lines[i])
			i++
		}
		body := strings.Join(bodyLines, "\n")

		// Validate the YAML body. Split on explicit document separators first
		// (multi-doc support: split on lines that are exactly "---").
		docs := splitYAMLDocs(body)
		for _, doc := range docs {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}
			if parseErr := validateYAMLDoc(doc); parseErr != nil {
				findings = append(findings, fmt.Sprintf(
					"%s:%d - invalid YAML: %s", path, startLine, parseErr.Error(),
				))
				continue
			}
			// For k8s manifests: if both "apiVersion" and "kind" are present,
			// run kubectl --dry-run=client if kubectl is available.
			if isK8sManifest(doc) {
				if kubectlPath, err := exec.LookPath("kubectl"); err == nil && kubectlPath != "" {
					findings = append(findings, kubectlDryRun(ctx, workspace, path, startLine, doc, run)...)
				}
			}
		}
	}

	return findings
}

// splitYAMLDocs splits a YAML body on lines that are exactly "---"
// (YAML document separator). Empty segments are kept so callers can skip them.
func splitYAMLDocs(body string) []string {
	var docs []string
	var cur []string
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
		} else {
			cur = append(cur, line)
		}
	}
	docs = append(docs, strings.Join(cur, "\n"))
	return docs
}

// validateYAMLDoc tries to unmarshal a YAML document. It first tries
// map[string]any (most common), then []any (list at the top level). Only
// when BOTH fail is it a real parse error (avoids false positives on
// list-rooted documents).
func validateYAMLDoc(doc string) error {
	var m map[string]any
	if err := sigsyaml.Unmarshal([]byte(doc), &m); err == nil {
		return nil
	}
	var l []any
	if err := sigsyaml.Unmarshal([]byte(doc), &l); err != nil {
		return err
	}
	return nil
}

// isK8sManifest reports whether the YAML document (already validated) looks
// like a Kubernetes manifest by checking for both "apiVersion" and "kind"
// keys at the top level.
func isK8sManifest(doc string) bool {
	var m map[string]any
	if err := sigsyaml.Unmarshal([]byte(doc), &m); err != nil {
		return false
	}
	_, hasAPI := m["apiVersion"]
	_, hasKind := m["kind"]
	return hasAPI && hasKind
}

// kubectlDryRun writes the YAML block to a temp file and runs
// `kubectl --dry-run=client -f <file>`. Returns any findings.
func kubectlDryRun(ctx context.Context, workspace, path string, startLine int, doc string, run commandRunner) []string {
	tmp, err := os.CreateTemp(os.TempDir(), "artifact-gate-*.yaml")
	if err != nil {
		return nil // fail-open
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.WriteString(doc); err != nil {
		_ = tmp.Close()
		return nil
	}
	_ = tmp.Close()

	out, err := run(ctx, workspace, nil, "kubectl", "--dry-run=client", "-f", tmpPath)
	if err != nil {
		return []string{fmt.Sprintf(
			"%s:%d - kubectl client validation failed: %s", path, startLine, strings.TrimSpace(out),
		)}
	}
	return nil
}
