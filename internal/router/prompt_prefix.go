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

package router

import (
	"bytes"
	"encoding/json"
)

// promptStream returns the parts of a request that a chat template renders
// ahead of generation, in render order, as one byte stream for prefix
// hashing: the tool definitions, then the system prompt (Anthropic Messages),
// then each message, or the prompt of a completion. Each part is compacted
// JSON with its keys in the order the client sent them, so whitespace does not
// split a shared prefix while the model name and sampling fields stay out of
// it. It returns nil for a body with none of these parts, such as an
// embeddings request, or one that is not JSON.
func promptStream(body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	var req struct {
		Tools     json.RawMessage   `json:"tools"`
		Functions json.RawMessage   `json:"functions"`
		System    json.RawMessage   `json:"system"`
		Messages  []json.RawMessage `json:"messages"`
		Prompt    json.RawMessage   `json:"prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	var out bytes.Buffer
	add := func(tag byte, raw json.RawMessage) {
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			return
		}
		out.WriteByte(tag)
		// raw came out of a successful Unmarshal, so it is valid JSON and
		// Compact cannot fail.
		_ = json.Compact(&out, raw)
		out.WriteByte(0x1e) // record separator: no part can run into the next
	}
	add('T', req.Tools)
	add('F', req.Functions)
	add('S', req.System)
	for _, m := range req.Messages {
		add('M', m)
	}
	add('P', req.Prompt)
	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}
