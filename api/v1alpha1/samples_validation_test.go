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

package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// splitYAMLDocs splits a multi-document YAML file on lines that are exactly
// "---", tolerating trailing whitespace. Simpler than a full YAML reader and
// sufficient for the hand-authored sample manifests.
func splitYAMLDocs(data []byte) []string {
	var docs []string
	var cur []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimRight(line, " \t") == "---" {
			docs = append(docs, strings.Join(cur, "\n"))
			cur = nil
			continue
		}
		cur = append(cur, line)
	}
	return append(docs, strings.Join(cur, "\n"))
}

// objForKind returns a fresh typed object for the LLMKube CRD kinds this repo
// owns, or nil for foreign kinds (KEDA HTTPScaledObject, core Secret/Service)
// that some samples also carry and which we do not validate here.
func objForKind(kind string) any {
	switch kind {
	case "Model":
		return &Model{}
	case "InferenceService":
		return &InferenceService{}
	case "ModelRouter":
		return &ModelRouter{}
	default:
		return nil
	}
}

// TestConfigSamplesDecodeStrict guards config/samples/*.yaml against
// hallucinated or mistyped CRD fields. Every Model / InferenceService /
// ModelRouter document must strict-decode into its typed struct, so an
// unknown field (e.g. a coder inventing spec.url or spec.parameters) or a
// wrong type (e.g. modelRef as an object when the field is a string) fails
// here instead of shipping a sample that kubectl apply would reject.
//
// Motivated by #699/#1021: a coder rewrote a sample with url/parameters/
// modelRef-object/runtime-object/service and it GATE-PASSed, because the
// Go-only gate never validated the YAML against the schema.
func TestConfigSamplesDecodeStrict(t *testing.T) {
	files, err := filepath.Glob("../../config/samples/*.yaml")
	if err != nil {
		t.Fatalf("glob samples: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no config/samples/*.yaml found; check the relative path from api/v1alpha1")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, doc := range splitYAMLDocs(data) {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			var head struct {
				Kind string `json:"kind"`
			}
			if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
				t.Errorf("%s doc %d: could not read kind: %v", filepath.Base(f), i, err)
				continue
			}
			obj := objForKind(head.Kind)
			if obj == nil {
				continue
			}
			if err := yaml.UnmarshalStrict([]byte(doc), obj); err != nil {
				t.Errorf("%s doc %d (%s): strict decode failed (unknown or mistyped field?): %v",
					filepath.Base(f), i, head.Kind, err)
			}
		}
	}
}

// TestConfigSamplesDecodeStrict_CatchesBadFields proves the guard actually
// rejects the #699 failure class: unknown fields and a map where a string is
// expected. If strict decoding ever silently accepted these, the samples test
// above would be worthless.
func TestConfigSamplesDecodeStrict_CatchesBadFields(t *testing.T) {
	cases := map[string]string{
		"unknown Model field (url instead of source)": `
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: {name: x}
spec:
  url: https://example.com/m.gguf
  format: gguf
`,
		"unknown Model block (parameters)": `
apiVersion: inference.llmkube.dev/v1alpha1
kind: Model
metadata: {name: x}
spec:
  source: https://example.com/m.gguf
  parameters: {contextSize: 4096}
`,
		"modelRef as object when field is a string": `
apiVersion: inference.llmkube.dev/v1alpha1
kind: InferenceService
metadata: {name: x}
spec:
  modelRef: {name: x}
`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			var head struct {
				Kind string `json:"kind"`
			}
			if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
				t.Fatalf("read kind: %v", err)
			}
			obj := objForKind(head.Kind)
			if obj == nil {
				t.Fatalf("no type for kind %q", head.Kind)
			}
			if err := yaml.UnmarshalStrict([]byte(doc), obj); err == nil {
				t.Errorf("strict decode should have rejected %q, but it passed", name)
			}
		})
	}
}
