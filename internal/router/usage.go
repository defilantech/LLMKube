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

package router

/*
Token accounting for budget enforcement (#434).

The proxy charges budgets from the upstream's OpenAI `usage` object, not
from a local tokenizer: the upstream already counted the tokens it served,
and re-deriving them here would drift from the provider's own count. When an
upstream reports no usage (a streaming response without
`stream_options.include_usage`, an older server, a non-conforming body) the
request is charged zero and surfaced via RouterBudgetUnchargedTotal rather
than estimated, so a silently under-enforcing budget is visible.
*/

import (
	"bytes"
	"encoding/json"
)

// usageEnvelope is the minimal OpenAI-compatible shape we read. Every field
// is a pointer so "usage absent" is distinguishable from "usage all zeros".
type usageEnvelope struct {
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
		TotalTokens      *int64 `json:"total_tokens"`
	} `json:"usage"`
}

// UsageTokens extracts prompt and completion token counts from a
// non-streaming OpenAI-compatible response body. ok is false when the body
// carries no usage object (or is not JSON), which is the signal to leave the
// request uncharged.
func UsageTokens(body []byte) (prompt, completion int64, ok bool) {
	if len(body) == 0 {
		return 0, 0, false
	}
	var env usageEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return 0, 0, false
	}
	return usageCounts(env.Usage.PromptTokens, env.Usage.CompletionTokens, env.Usage.TotalTokens)
}

// UsageTokensFromSSE extracts usage from a buffered SSE stream. Providers
// emit a final `data: {...}` chunk carrying the usage object when the client
// requested it; the last such chunk wins. ok is false when no chunk carried
// usage, which is the common case for llama.cpp/vLLM streaming and the
// signal to leave the request uncharged.
func UsageTokensFromSSE(body []byte) (prompt, completion int64, ok bool) {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		payload, found := bytes.CutPrefix(line, []byte("data:"))
		if !found {
			continue
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var env usageEnvelope
		if err := json.Unmarshal(payload, &env); err != nil || env.Usage == nil {
			continue
		}
		if p, c, ok2 := usageCounts(env.Usage.PromptTokens, env.Usage.CompletionTokens, env.Usage.TotalTokens); ok2 {
			prompt, completion, ok = p, c, true
		}
	}
	return prompt, completion, ok
}

// usageCounts normalizes the three usage fields, clamping negatives to zero.
// It reports ok when any field was present, so a response that carries only
// a total is still chargeable.
func usageCounts(promptPtr, completionPtr, totalPtr *int64) (prompt, completion int64, ok bool) {
	if promptPtr == nil && completionPtr == nil && totalPtr == nil {
		return 0, 0, false
	}
	if promptPtr != nil {
		prompt = *promptPtr
	}
	if completionPtr != nil {
		completion = *completionPtr
	}
	if promptPtr == nil && completionPtr == nil && totalPtr != nil {
		// Only a total was reported; charge it as completion, the side that
		// dominates generation traffic, rather than dropping it.
		completion = *totalPtr
	}
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	return prompt, completion, true
}

// CostUSD prices a charged request. A nil cost means the backend declared no
// pricing and the request costs 0 USD against dollar budgets.
func CostUSD(prompt, completion int64, cost *TokenCost) float64 {
	if cost == nil {
		return 0
	}
	return float64(prompt)/1e6*cost.PromptUSD + float64(completion)/1e6*cost.CompletionUSD
}

// InjectStreamUsage rewrites a buffered OpenAI-compatible request body to ask
// the upstream for a token-usage object on a streamed response. The proxy
// charges from the provider's own count, so a budgeted stream must request one
// or it is charged zero. A body that is not a stream is returned unchanged; a
// body that already asks for usage is unchanged; otherwise stream_options is
// set to include_usage true, merged into any existing stream_options so
// sibling fields survive. It reports whether the body changed.
func InjectStreamUsage(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}
	var stream bool
	if err := json.Unmarshal(req["stream"], &stream); err != nil || !stream {
		return body, false
	}

	// A stream_options that is not a JSON object (or is null) carries no flag
	// to preserve, so it is replaced wholesale.
	opts := map[string]json.RawMessage{}
	if raw, ok := req["stream_options"]; ok {
		if err := json.Unmarshal(raw, &opts); err != nil || opts == nil {
			opts = map[string]json.RawMessage{}
		}
	}
	var alreadyAsking bool
	if inc, ok := opts["include_usage"]; ok {
		_ = json.Unmarshal(inc, &alreadyAsking)
	}
	if alreadyAsking {
		return body, false
	}
	opts["include_usage"] = json.RawMessage(`true`)
	mergedOpts, err := json.Marshal(opts)
	if err != nil {
		return body, false
	}
	req["stream_options"] = json.RawMessage(mergedOpts)
	out, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return out, true
}

// looksLikeSSE reports whether a captured response body is an SSE stream
// rather than a single JSON object. Used to pick the right usage parser. An
// SSE body carries at least one line beginning with "data:"; a single JSON
// object does not, however often its content mentions "data:".
func looksLikeSSE(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			return true
		}
	}
	return false
}
