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

package v1alpha1

import "testing"

func TestOllamaModelTag(t *testing.T) {
	cases := []struct {
		name, modelName, source string
		wantTag                 string
		wantHailo, wantErr      bool
	}{
		{name: "catalog name is mapped", modelName: "phi-4-mini", source: "hf://org/x", wantTag: "phi4-mini:latest"},
		{name: "unmapped name passes through", modelName: "my-model", source: "hf://org/x", wantTag: "my-model"},
		{name: "empty source uses the name", modelName: "llama-3.1-8b", wantTag: "llama3.1:8b"},
		{
			name: "hailo source wins over the name", modelName: "phi-4-mini", source: "hailo://qwen2.5-instruct:1.5b",
			wantTag: "qwen2.5-instruct:1.5b", wantHailo: true,
		},
		{
			name: "hailo scheme is case-insensitive", modelName: "m", source: "HAILO://llama3.2:3b",
			wantTag: "llama3.2:3b", wantHailo: true,
		},
		{
			name: "hailo tag without a :tag is an error", modelName: "m", source: "hailo://qwen2.5",
			wantTag: "qwen2.5", wantHailo: true, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tag, hailo, err := OllamaModelTag(tc.modelName, tc.source)
			if tag != tc.wantTag || hailo != tc.wantHailo || (err != nil) != tc.wantErr {
				t.Fatalf("OllamaModelTag(%q, %q) = %q, %v, %v; want %q, %v, err=%v",
					tc.modelName, tc.source, tag, hailo, err, tc.wantTag, tc.wantHailo, tc.wantErr)
			}
		})
	}
}
