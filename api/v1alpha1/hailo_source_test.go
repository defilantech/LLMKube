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

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHailoSourceTag(t *testing.T) {
	for _, tc := range []struct {
		source, tag string
		ok, err     bool
	}{
		{source: "hailo://qwen2.5-instruct:1.5b", tag: "qwen2.5-instruct:1.5b", ok: true},
		{source: "HAILO://llama3.2:3b", tag: "llama3.2:3b", ok: true},
		{source: "Hailo://deepseek_r1_distill_qwen:1.5b", tag: "deepseek_r1_distill_qwen:1.5b", ok: true},
		{source: "hailo://qwen3", ok: true, err: true},
		{source: "hailo://qwen3:", ok: true, err: true},
		{source: "hailo://qwen:..", ok: true, err: true},
		{source: "hailo://qwen:.", ok: true, err: true},
		{source: "hailo://..:1", ok: true, err: true},
		{source: "hailo://.:1", ok: true, err: true},
		{source: "hailo://attacker.example.com/x:latest", ok: true, err: true},
		{source: "hailo://someone/model", ok: true, err: true},
		{source: "hailo://", ok: true, err: true},
		{source: "hailo://Qwen:1.5b", ok: true, err: true},
		{source: "https://example.com/m.gguf"},
		{source: "hf://Qwen/Qwen2.5-1.5B"},
		{source: ""},
	} {
		tag, ok, err := HailoSourceTag(tc.source)
		if ok != tc.ok || (err != nil) != tc.err || (!tc.err && tag != tc.tag) {
			t.Errorf("HailoSourceTag(%q) = %q, %v, %v; want %q, ok %v, err %v", tc.source, tag, ok, err, tc.tag, tc.ok, tc.err)
		}
	}
}

func TestHailoSourceRuntimeError(t *testing.T) {
	model := func(source string) *Model {
		return &Model{ObjectMeta: metav1.ObjectMeta{Name: "m"}, Spec: ModelSpec{Source: source}}
	}
	hailo := model("hailo://qwen2.5-instruct:1.5b")
	if err := HailoSourceRuntimeError(hailo, RuntimeOllama); err != nil {
		t.Errorf("hailo source on ollama: %v", err)
	}
	for _, rt := range []string{"llamacpp", "llama-server", RuntimeOMLX, ""} {
		err := HailoSourceRuntimeError(hailo, rt)
		if err == nil || !strings.Contains(err.Error(), "hailo://") || !strings.Contains(err.Error(), "ollama") {
			t.Errorf("hailo source on %q: err = %v, want a refusal naming hailo:// and ollama", rt, err)
		}
	}
	if err := HailoSourceRuntimeError(model("https://x/m.gguf"), "llamacpp"); err != nil {
		t.Errorf("http source on llamacpp: %v", err)
	}
	if err := HailoSourceRuntimeError(nil, "llamacpp"); err != nil {
		t.Errorf("nil Model: %v", err)
	}
}
