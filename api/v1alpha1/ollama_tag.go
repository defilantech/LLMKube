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

// ollamaModelTags maps LLMKube catalog Model names to the Ollama tags that
// serve them. A name not listed here is used as the tag unchanged.
var ollamaModelTags = map[string]string{
	"llama-3.2-3b":       "llama3.2:3b",
	"llama-3.1-8b":       "llama3.1:8b",
	"llama-3.3-70b":      "llama3.3:70b",
	"qwen-2.5-14b":       "qwen2.5:14b",
	"qwen-2.5-32b":       "qwen2.5:32b",
	"qwen-2.5-coder-7b":  "qwen2.5-coder:7b",
	"qwen-2.5-coder-32b": "qwen2.5-coder:32b",
	"qwen-3-32b":         "qwen3:32b",
	"mistral-7b":         "mistral:7b",
	"mistral-small-24b":  "mistral-small:24b",
	"gemma-3-4b":         "gemma3:4b",
	"gemma-3-12b":        "gemma3:12b",
	"phi-4-mini":         "phi4-mini:latest",
	"deepseek-r1-7b":     "deepseek-r1:7b",
	"deepseek-r1-14b":    "deepseek-r1:14b",
	"deepseek-r1-32b":    "deepseek-r1:32b",
}

// OllamaModelTag returns the tag the ollama runtime serves for a Model: the
// tag in a hailo:// source (hailo is then true, and err is set when that tag
// is not "name:tag"), else the Model name mapped through the catalog table,
// else the Model name itself.
//
// The metal agent's Ollama executor pulls and loads this tag, and the
// ModelRouter compiles it as an Ollama backend's served model (#2586). Both
// call this one function so the router never rewrites a request to a name the
// daemon does not serve.
func OllamaModelTag(modelName, source string) (tag string, hailo bool, err error) {
	if tag, ok, err := HailoSourceTag(source); ok {
		return tag, true, err
	}
	if mapped, ok := ollamaModelTags[modelName]; ok {
		return mapped, false, nil
	}
	return modelName, false, nil
}
