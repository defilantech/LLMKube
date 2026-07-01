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

package agent

import (
	"context"
	"strings"
	"testing"
)

// fakeMdRunner returns a commandRunner for artifact_gate tests.
// It handles two commands:
//   - git diff --name-only --diff-filter=d HEAD -- *.md : returns changed md paths (newline-joined)
//   - git show :<path> : returns the content for that path from the contents map
func fakeMdRunner(changedMd []string, contents map[string]string) commandRunner {
	return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, error) {
		if name != "git" {
			return "", nil
		}
		// git diff --name-only --diff-filter=d HEAD -- *.md
		if len(args) >= 5 && args[0] == "diff" && args[1] == "--name-only" {
			return strings.Join(changedMd, "\n"), nil
		}
		// git show :<path>
		if len(args) == 2 && args[0] == "show" && strings.HasPrefix(args[1], ":") {
			path := strings.TrimPrefix(args[1], ":")
			if contents != nil {
				return contents[path], nil
			}
			return "", nil
		}
		return "", nil
	}
}

func TestCheckEmbeddedArtifacts_BrokenYAMLFails(t *testing.T) {
	run := fakeMdRunner(
		[]string{"docs/site/guides/x.md"},
		map[string]string{
			"docs/site/guides/x.md": "intro text\n\n```yaml\nfoo: {bar\n```\n\nmore text\n",
		})
	failed, out := checkEmbeddedArtifacts(context.Background(), "/ws", run)
	if !failed || !strings.Contains(out, "docs/site/guides/x.md") {
		t.Fatalf("want YAML-parse failure naming the file, got failed=%v out=%q", failed, out)
	}
}

func TestCheckEmbeddedArtifacts_ValidYAMLPasses(t *testing.T) {
	run := fakeMdRunner(
		[]string{"docs/x.md"},
		map[string]string{
			"docs/x.md": "```yaml\nfoo: bar\nlist:\n  - a\n  - b\n```\n",
		})
	failed, _ := checkEmbeddedArtifacts(context.Background(), "/ws", run)
	if failed {
		t.Fatalf("valid YAML should pass")
	}
}

func TestCheckEmbeddedArtifacts_NoMarkdownChangesPasses(t *testing.T) {
	run := fakeMdRunner(nil, nil)
	failed, _ := checkEmbeddedArtifacts(context.Background(), "/ws", run)
	if failed {
		t.Fatal("no changed markdown -> pass")
	}
}
