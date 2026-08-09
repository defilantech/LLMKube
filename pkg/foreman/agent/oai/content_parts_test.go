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

package oai

import (
	"encoding/json"
	"testing"
)

// Image input (#1466). A message carries EITHER a plain string content or an
// array of content parts. The parts form is what carries an image, and it has
// to coexist with the existing string form without disturbing it: the
// non-assistant always-emit-content rule from #556 is load-bearing against
// strict backends and must survive.

func TestMessage_PartsSerializeAsContentArray(t *testing.T) {
	m := Message{Role: RoleUser, Parts: []ContentPart{
		{Type: ContentPartText, Text: "what is wrong with this render?"},
		{Type: ContentPartImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,AAAA"}},
	}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	parts, ok := got["content"].([]any)
	if !ok {
		t.Fatalf("content is not an array: %s", b)
	}
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d: %s", len(parts), b)
	}
	first := parts[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "what is wrong with this render?" {
		t.Errorf("text part wrong: %v", first)
	}
	second := parts[1].(map[string]any)
	if second["type"] != "image_url" {
		t.Errorf("image part type wrong: %v", second)
	}
	iu, ok := second["image_url"].(map[string]any)
	if !ok || iu["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image_url wrong: %v", second)
	}
	// The string field must not also appear; two content keys is invalid.
	if _, dup := first["content"]; dup {
		t.Error("nested content key leaked into a part")
	}
}

// The #556 rule: non-assistant roles always emit content, even empty. Adding
// Parts must not regress it for messages that do not use parts.
func TestMessage_StringContentUnchanged(t *testing.T) {
	for _, role := range []Role{RoleSystem, RoleUser, RoleTool} {
		b, err := json.Marshal(Message{Role: role, Content: ""})
		if err != nil {
			t.Fatalf("marshal %s: %v", role, err)
		}
		var got map[string]any
		_ = json.Unmarshal(b, &got)
		c, present := got["content"]
		if !present {
			t.Errorf("%s dropped the content key; strict backends reject that (#556): %s", role, b)
		}
		if c != "" {
			t.Errorf("%s content = %v, want empty string", role, c)
		}
	}
}

// Assistant keeps omitempty so {"role":"assistant","tool_calls":[...]} stays
// valid rather than carrying an awkward empty content alongside tool calls.
func TestMessage_AssistantOmitsEmptyContent(t *testing.T) {
	b, err := json.Marshal(Message{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "1", Type: "function"}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	if _, present := got["content"]; present {
		t.Errorf("assistant emitted content alongside tool_calls: %s", b)
	}
}

// Parts win over Content when both are set, rather than emitting both keys
// (which is invalid) or silently dropping the image.
func TestMessage_PartsWinOverStringContent(t *testing.T) {
	m := Message{Role: RoleUser, Content: "ignored", Parts: []ContentPart{
		{Type: ContentPartText, Text: "used"},
	}}
	b, _ := json.Marshal(m)
	var got map[string]any
	_ = json.Unmarshal(b, &got)
	arr, ok := got["content"].([]any)
	if !ok {
		t.Fatalf("content should be the parts array when Parts is set: %s", b)
	}
	if arr[0].(map[string]any)["text"] != "used" {
		t.Errorf("wrong part content: %s", b)
	}
}
