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

import (
	"strings"
	"testing"
)

func TestUsageTokens(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantPrompt     int64
		wantCompletion int64
		wantOK         bool
	}{
		{
			name:           "prompt and completion",
			body:           `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":60,"completion_tokens":40}}`,
			wantPrompt:     60,
			wantCompletion: 40,
			wantOK:         true,
		},
		{
			name:           "negative clamped to zero",
			body:           `{"usage":{"prompt_tokens":-5,"completion_tokens":-2}}`,
			wantPrompt:     0,
			wantCompletion: 0,
			wantOK:         true,
		},
		{
			name:           "total only charged as completion",
			body:           `{"usage":{"total_tokens":7}}`,
			wantPrompt:     0,
			wantCompletion: 7,
			wantOK:         true,
		},
		{
			name:   "usage absent",
			body:   `{"choices":[{"message":{"content":"hi"}}]}`,
			wantOK: false,
		},
		{
			name:   "empty body",
			body:   "",
			wantOK: false,
		},
		{
			name:   "not json",
			body:   `data: not an object`,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, completion, ok := UsageTokens([]byte(tt.body))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if prompt != tt.wantPrompt {
				t.Errorf("prompt = %d, want %d", prompt, tt.wantPrompt)
			}
			if completion != tt.wantCompletion {
				t.Errorf("completion = %d, want %d", completion, tt.wantCompletion)
			}
		})
	}
}

func TestUsageTokensFromSSE(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		wantPrompt     int64
		wantCompletion int64
		wantOK         bool
	}{
		{
			name: "usage chunk",
			body: "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n" +
				"data: {\"usage\":{\"prompt_tokens\":70,\"completion_tokens\":30}}\n\n" +
				"data: [DONE]\n\n",
			wantPrompt:     70,
			wantCompletion: 30,
			wantOK:         true,
		},
		{
			name: "last usage chunk wins",
			body: "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n" +
				"data: {\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":8}}\n\n",
			wantPrompt:     9,
			wantCompletion: 8,
			wantOK:         true,
		},
		{
			name:   "no usage chunk",
			body:   "data: {\"delta\":\"a\"}\n\ndata: [DONE]\n\n",
			wantOK: false,
		},
		{
			name:   "empty body",
			body:   "",
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prompt, completion, ok := UsageTokensFromSSE([]byte(tt.body))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if prompt != tt.wantPrompt {
				t.Errorf("prompt = %d, want %d", prompt, tt.wantPrompt)
			}
			if completion != tt.wantCompletion {
				t.Errorf("completion = %d, want %d", completion, tt.wantCompletion)
			}
		})
	}
}

func TestCostUSD(t *testing.T) {
	cost := &TokenCost{PromptUSD: 0.5, CompletionUSD: 1.5}

	if got := CostUSD(1_000_000, 2_000_000, cost); got != 0.5+3.0 {
		t.Errorf("CostUSD = %v, want %v", got, 0.5+3.0)
	}
	if got := CostUSD(1_000_000, 1_000_000, nil); got != 0 {
		t.Errorf("CostUSD with nil cost = %v, want 0", got)
	}
}

func TestLooksLikeSSE(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "sse stream", body: "data: {\"delta\":\"a\"}\n\ndata: [DONE]\n\n", want: true},
		{name: "json object", body: `{"choices":[{"message":{"content":"hi"}}]}`, want: false},
		{
			name: "json mentions data",
			body: `{"choices":[{"message":{"content":"send data: to the server"}}],"usage":{"prompt_tokens":60}}`,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeSSE([]byte(tt.body)); got != tt.want {
				t.Errorf("looksLikeSSE(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestInjectStreamUsage(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantChanged bool
		// wantContains is asserted present in the rewritten body when changed.
		wantContains string
		// wantAbsent is asserted absent whenever non-empty.
		wantAbsent string
	}{
		{
			name:         "stream without options gets the flag",
			body:         `{"model":"any","stream":true}`,
			wantChanged:  true,
			wantContains: `"include_usage":true`,
		},
		{
			name:        "non-stream is untouched",
			body:        `{"model":"any"}`,
			wantChanged: false,
			wantAbsent:  "include_usage",
		},
		{
			name:        "stream false is untouched",
			body:        `{"model":"any","stream":false}`,
			wantChanged: false,
			wantAbsent:  "include_usage",
		},
		{
			name:         "client stream_options asking false is forced to true",
			body:         `{"model":"any","stream":true,"stream_options":{"include_usage":false}}`,
			wantChanged:  true,
			wantContains: `"include_usage":true`,
			wantAbsent:   `"include_usage":false`,
		},
		{
			name:         "existing stream_options siblings are preserved",
			body:         `{"model":"any","stream":true,"stream_options":{"include_usage":false,"foo":"bar"}}`,
			wantChanged:  true,
			wantContains: `"foo":"bar"`,
		},
		{
			name:         "existing include_usage true is left unchanged",
			body:         `{"model":"any","stream":true,"stream_options":{"include_usage":true}}`,
			wantChanged:  false,
			wantContains: `"include_usage":true`,
		},
		{
			name:         "non-object stream_options is replaced",
			body:         `{"model":"any","stream":true,"stream_options":"none"}`,
			wantChanged:  true,
			wantContains: `"include_usage":true`,
		},
		{
			name:        "empty body is untouched",
			body:        "",
			wantChanged: false,
		},
		{
			name:        "non-json is untouched",
			body:        `not json`,
			wantChanged: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changed := InjectStreamUsage([]byte(tt.body))
			if changed != tt.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tt.wantChanged)
			}
			if tt.wantContains != "" && !strings.Contains(string(out), tt.wantContains) {
				t.Errorf("rewritten body = %q, want it to contain %q", out, tt.wantContains)
			}
			if tt.wantAbsent != "" && strings.Contains(string(out), tt.wantAbsent) {
				t.Errorf("rewritten body = %q, want it to not contain %q", out, tt.wantAbsent)
			}
		})
	}
}
