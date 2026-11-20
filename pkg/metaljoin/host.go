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

package metaljoin

import inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"

// MacOSOnlyBinFlag is an agent binary flag for an engine that needs Apple
// Silicon, and the runtime it selects.
type MacOSOnlyBinFlag struct {
	Flag    string
	Runtime string
}

// MacOSOnlyBinFlags are the agent's binary flags for the runtimes
// inferencev1alpha1.IsMacOSOnlyRuntime reports. Off macOS the agent refuses
// each one at startup unless --runtime already selects its runtime, and
// llmkube metal join refuses them before it spends the join string.
var MacOSOnlyBinFlags = []MacOSOnlyBinFlag{
	{Flag: "mlx-server-bin", Runtime: inferencev1alpha1.RuntimeMLXServer},
	{Flag: "omlx-bin", Runtime: inferencev1alpha1.RuntimeOMLX},
	{Flag: "vllm-swift-bin", Runtime: inferencev1alpha1.RuntimeVLLMSwift},
	{Flag: "tensorfold-bin", Runtime: inferencev1alpha1.RuntimeTensorFold},
}

// LlamaServerPaths is where the agent looks for llama-server without
// --llama-server, in order: Apple Silicon Homebrew, Intel Homebrew or a
// source install, then a Linux distribution package. llmkube metal join
// looks in the same places on a Linux host.
var LlamaServerPaths = []string{
	"/opt/homebrew/bin/llama-server",
	"/usr/local/bin/llama-server",
	"/usr/bin/llama-server",
}
