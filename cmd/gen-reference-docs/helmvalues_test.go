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

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const fixtureValues = `# Chart header, not a key's description.

# Namespace to deploy into
namespace: llmkube-system

# Controller settings
controller:
  # Image to run | with a pipe
  image: ghcr.io/x/y
  undocumented: 3
  tag: ""
  quoted: "60s"
  # The canary, which documents the keys under it
  canary:
    interval: 60s
  annotations: {}
  drop:
    - "ALL"
  long:
    - aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    - bbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
    - cccccccccccccccccccccccccccccc
  podLabels:
    app.kubernetes.io/name: llmkube
  nothing:
  inline: 5 # trailing comment
  script: |
    line one
    line two
# Extra objects
extraObjects: []
#  - commented-out: example
#    that yaml.v3 hangs on the key before as a foot comment

last: 1
`

func writeCharts(t *testing.T, llmkube, foreman string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range map[string]string{llmkubeValues: llmkube, foremanValues: foreman} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestRenderHelmValuesRows(t *testing.T) {
	root := writeCharts(t, fixtureValues, "# Foreman knob\nknob: true\n")
	out, err := renderHelmValues(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{
		"| `namespace` | `llmkube-system` | Namespace to deploy into |",
		"| `controller.image` | `ghcr.io/x/y` | Image to run \\| with a pipe |",
		"| `controller.undocumented` | `3` |  |",
		"| `controller.tag` | `\"\"` |  |",
		"| `controller.quoted` | `\"60s\"` |  |",
		"| `controller.canary` |  | The canary, which documents the keys under it |",
		"| `controller.canary.interval` | `60s` |  |",
		"| `controller.annotations` | `{}` |  |",
		"| `controller.drop` | `[\"ALL\"]` |  |",
		"| `controller.long` | list of 3 items |  |",
		"| `controller.podLabels.app\\.kubernetes\\.io/name` | `llmkube` |  |",
		"| `controller.nothing` | `null` |  |",
		"| `controller.inline` | `5` | trailing comment |",
		"| `controller.script` | multi-line string, 2 lines |  |",
		"| `extraObjects` | `[]` | Extra objects |",
		"| `knob` | `true` | Foreman knob |",
	} {
		if !strings.Contains(out, row+"\n") {
			t.Errorf("missing row %s", row)
		}
	}
	for _, want := range []string{
		"## controller\n\nController settings\n\n", "## Foreman chart\n", "### Foreman: knob\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, absent := range []string{"Chart header", "commented-out"} {
		if strings.Contains(out, absent) {
			t.Errorf("%q is not a key's description but was rendered", absent)
		}
	}
}

// Every leaf of both real charts has a row. The leaves come from a plain map
// decode, not the yaml.Node walk the page is rendered with.
func TestRenderHelmValuesCoversEveryLeaf(t *testing.T) {
	out, err := renderHelmValues("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{llmkubeValues, foremanValues} {
		data, err := os.ReadFile(filepath.Join("../..", file))
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		if err := yaml.Unmarshal(data, &values); err != nil {
			t.Fatal(err)
		}
		leaves := flattenLeaves(values, "")
		if len(leaves) < 50 {
			t.Fatalf("%s: only %d leaves; the fixture path is wrong", file, len(leaves))
		}
		for _, leaf := range leaves {
			if !strings.Contains(out, "| "+codeSpan(leaf)+" | ") {
				t.Errorf("%s: no row for %s", file, leaf)
			}
		}
	}
}

func flattenLeaves(m map[string]any, prefix string) []string {
	var out []string
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		path := strings.ReplaceAll(k, ".", `\.`)
		if prefix != "" {
			path = prefix + "." + path
		}
		if sub, ok := m[k].(map[string]any); ok && len(sub) > 0 {
			out = append(out, flattenLeaves(sub, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}
