// sync-reviewer-prompts reads config/foreman/system-prompts/reviewer.md and
// writes its contents into spec.systemPrompt of every reviewer Agent manifest
// under config/foreman/agents/*-reviewer.yaml.
//
// Usage:
//
//	go run ./cmd/sync-reviewer-prompts          // sync (write)
//	go run ./cmd/sync-reviewer-prompts --check  // drift-check (exit 1 on mismatch)
//
// The tool uses gopkg.in/yaml.v3 to parse and emit YAML so the ~16 KB
// multi-line markdown is never corrupted by sed or shell quoting.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	promptPath   = "config/foreman/system-prompts/reviewer.md"
	agentsDir    = "config/foreman/agents"
	reviewerGlob = "*-reviewer.yaml"
)

func main() {
	checkMode := false
	for _, arg := range os.Args[1:] {
		if arg == "--check" {
			checkMode = true
			break
		}
	}

	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ reading %s: %v\n", promptPath, err)
		os.Exit(1)
	}
	promptStr := string(prompt)

	// Ensure the prompt ends with a single newline so the YAML block scalar
	// round-trips cleanly.
	promptStr = strings.TrimRight(promptStr, "\n") + "\n"

	pattern := filepath.Join(agentsDir, reviewerGlob)
	files, err := filepath.Glob(pattern)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ glob %s: %v\n", pattern, err)
		os.Exit(1)
	}

	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "❌ no reviewer manifests found matching %s\n", pattern)
		os.Exit(1)
	}

	var drifted []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ reading %s: %v\n", f, err)
			os.Exit(1)
		}

		var node yaml.Node
		if err := yaml.Unmarshal(data, &node); err != nil {
			fmt.Fprintf(os.Stderr, "❌ parsing %s: %v\n", f, err)
			os.Exit(1)
		}

		// Descend into the document root if needed.
		root := &node
		if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
			root = root.Content[0]
		}

		// Find spec.role to confirm this is a reviewer.
		role, err := findString(root, "spec", "role")
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ reading spec.role from %s: %v\n", f, err)
			os.Exit(1)
		}
		if role != "reviewer" {
			continue
		}

		if checkMode {
			current, err := findString(root, "spec", "systemPrompt")
			if err != nil {
				fmt.Fprintf(os.Stderr, "❌ reading spec.systemPrompt from %s: %v\n", f, err)
				os.Exit(1)
			}
			if current != promptStr {
				drifted = append(drifted, f)
			}
			continue
		}

		// Update spec.systemPrompt in the node tree.
		if err := setString(root, []string{"spec", "systemPrompt"}, promptStr); err != nil {
			fmt.Fprintf(os.Stderr, "❌ updating spec.systemPrompt in %s: %v\n", f, err)
			os.Exit(1)
		}

		out, err := yaml.Marshal(&node)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ marshaling %s: %v\n", f, err)
			os.Exit(1)
		}

		if err := os.WriteFile(f, out, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "❌ writing %s: %v\n", f, err)
			os.Exit(1)
		}
		fmt.Printf("✅ Synced %s\n", f)
	}

	if checkMode {
		if len(drifted) > 0 {
			fmt.Fprintf(os.Stderr, "❌ reviewer prompt drift detected in:\n")
			for _, f := range drifted {
				fmt.Fprintf(os.Stderr, "     %s\n", f)
			}
			fmt.Fprintf(os.Stderr, "\nRun 'make sync-reviewer-prompts' to fix.\n")
			os.Exit(1)
		}
		fmt.Println("✅ All reviewer Agent systemPrompts match reviewer.md")
	}
}

// findString walks the YAML node tree to find the value at the given
// key path (e.g. "spec", "role").
func findString(root *yaml.Node, keys ...string) (string, error) {
	cur := root
	for _, k := range keys {
		if cur.Kind != yaml.MappingNode {
			return "", fmt.Errorf("expected mapping node, got %v", cur.Kind)
		}
		found := false
		for i := 0; i < len(cur.Content); i += 2 {
			if cur.Content[i].Value == k {
				cur = cur.Content[i+1]
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("key %q not found", k)
		}
	}
	return cur.Value, nil
}

// setString walks the YAML node tree to set the value at the given
// key path (e.g. ["spec", "systemPrompt"]) to the provided value.
func setString(root *yaml.Node, path []string, value string) error {
	cur := root
	for i, k := range path {
		if i < len(path)-1 {
			// Navigate to the next level.
			if cur.Kind != yaml.MappingNode {
				return fmt.Errorf("expected mapping node, got %v", cur.Kind)
			}
			found := false
			for j := 0; j < len(cur.Content); j += 2 {
				if cur.Content[j].Value == k {
					cur = cur.Content[j+1]
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("key %q not found", k)
			}
		} else {
			// Last path key: set the value.
			if cur.Kind != yaml.MappingNode {
				return fmt.Errorf("expected mapping node, got %v", cur.Kind)
			}
			found := false
			for j := 0; j < len(cur.Content); j += 2 {
				if cur.Content[j].Value == k {
					// Replace the value node with a new scalar.
					cur.Content[j+1] = &yaml.Node{
						Kind:  yaml.ScalarNode,
						Tag:   "!!str",
						Value: value,
					}
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("key %q not found", k)
			}
		}
	}
	return nil
}
