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

	"sigs.k8s.io/yaml"
)

const fixtureCRD = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.%[1]s
spec:
  group: %[1]s
  names:
    kind: %[2]s
    listKind: %[2]sList
    plural: widgets
    shortNames: [wd]
    categories: [all]
    singular: widget
  scope: Namespaced
  versions:
  - name: v1alpha1
    served: true
    storage: true
    additionalPrinterColumns:
    - jsonPath: .status.phase
      name: Phase
      type: string
    - jsonPath: .spec.size
      name: Size
      type: integer
      priority: 1
      description: How big | really
    schema:
      openAPIV3Schema:
        properties:
          spec:
            description: The desired widget.
            required: [size]
            properties:
              size:
                type: integer
                format: int32
                minimum: 1
                maximum: 8
                default: 2
                description: |-
                  Size of the widget, a map of {key,value} pairs.

                  A second paragraph the table leaves out.
              mode:
                type: string
                enum: [fast, slow]
                pattern: ^[a-z]+$
              ports:
                type: array
                items:
                  type: object
                  properties:
                    name:
                      type: string
                    deep:
                      type: object
                      properties:
                        deeper:
                          type: object
                          properties:
                            deepest:
                              type: string
                            alsoDeepest:
                              type: string
              labels:
                type: object
                additionalProperties:
                  type: string
              port:
                x-kubernetes-int-or-string: true
          status:
            properties:
              phase:
                type: string
        type: object
`

func writeCRDs(t *testing.T, crds map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, crdDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range crds {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func fixture(group, kind string) string {
	return strings.NewReplacer("%[1]s", group, "%[2]s", kind).Replace(fixtureCRD)
}

func TestRenderCRDsFields(t *testing.T) {
	root := writeCRDs(t, map[string]string{"widgets.yaml": fixture("inference.llmkube.dev", "Widget")})
	out, err := renderCRDs(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []string{
		"| `spec.size` | integer (int32) | yes | `2` | min 1; max 8 | Size of the widget, a map of `{key,value}` pairs. |",
		"| `spec.mode` | string |  |  | one of `\"fast\"`, `\"slow\"`; pattern `^[a-z]+$` |  |",
		"| `spec.ports` | array of object |  |  |  |  |",
		"| `spec.ports[].name` | string |  |  |  |  |",
		"| `spec.ports[].deep.deeper` | object |  |  |  | Has 2 nested fields; see the YAML. |",
		"| `spec.labels` | map of string |  |  |  |  |",
		"| `spec.port` | int or string |  |  |  |  |",
		"| `status.phase` | string |  |  |  |  |",
		"| Size | `integer` | `.spec.size` | 1 | How big \\| really |",
		"| Short names | `wd` |",
		"| Categories | `all` |",
	} {
		if !strings.Contains(out, row+"\n") {
			t.Errorf("missing row %s", row)
		}
	}
	if strings.Contains(out, "deeper.deepest") || strings.Contains(out, "alsoDeepest") {
		t.Errorf("a field below the depth limit of %d was rendered", crdFieldDepth)
	}
	if strings.Contains(out, "second paragraph") {
		t.Error("a description's second paragraph was rendered")
	}
}

func TestRenderCRDsGroupOrder(t *testing.T) {
	root := writeCRDs(t, map[string]string{
		"a.yaml": fixture("aaa.example.com", "Zeta"),
		"b.yaml": fixture("federation.llmkube.dev", "Fed"),
		"c.yaml": fixture("foreman.llmkube.dev", "Job"),
		"d.yaml": fixture("inference.llmkube.dev", "Beta"),
		"e.yaml": fixture("inference.llmkube.dev", "Alpha"),
	})
	out, err := renderCRDs(root)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		"## inference.llmkube.dev", "### Alpha", "### Beta",
		"## foreman.llmkube.dev", "## federation.llmkube.dev", "## aaa.example.com",
	}
	last := -1
	for _, h := range order {
		i := strings.Index(out, "\n"+h+"\n")
		if i < 0 || i < last {
			t.Fatalf("heading %q missing or out of order (want %v)", h, order)
		}
		last = i
	}
}

// Every top-level spec and status field of every real CRD has a row. The
// field names come from a plain map decode, not the typed schema the page is
// rendered from.
func TestRenderCRDsCoversTopLevelFields(t *testing.T) {
	out, err := renderCRDs("../..")
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join("../..", crdDir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no CRDs found: %v", err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd map[string]any
		if err := yaml.Unmarshal(data, &crd); err != nil {
			t.Fatal(err)
		}
		kind := dig(crd, "spec", "names", "kind").(string)
		if !strings.Contains(out, "\n### "+kind+"\n") {
			t.Errorf("no section for %s", kind)
		}
		for _, v := range dig(crd, "spec", "versions").([]any) {
			props, _ := dig(v.(map[string]any), "schema", "openAPIV3Schema", "properties").(map[string]any)
			for _, part := range []string{"spec", "status"} {
				fields, _ := dig(props, part, "properties").(map[string]any)
				names := make([]string, 0, len(fields))
				for name := range fields {
					names = append(names, name)
				}
				sort.Strings(names)
				for _, name := range names {
					if !strings.Contains(out, "| `"+part+"."+name+"` |") {
						t.Errorf("%s: no row for %s.%s", kind, part, name)
					}
				}
			}
		}
	}
}

func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}
