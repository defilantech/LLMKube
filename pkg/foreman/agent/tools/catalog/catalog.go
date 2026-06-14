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

// Package catalog is a dependency-free leaf holding the canonical set of
// tool names the v0.1 surface exposes. It exists so the Agent admission
// webhook can validate spec.tools whitelists WITHOUT importing the parent
// pkg/foreman/agent/tools package (and through it, the executor's full
// dependency tree: pkg/foreman/agent, git, the OAI client, repomap) into
// the operator binary.
//
// This package deliberately imports nothing. The list below is a hand-
// maintained mirror of the names the real tool constructors return; a
// drift guard in pkg/foreman/agent/tools (catalog_drift_test.go), which
// may import both this leaf and the real tools, fails the build if the two
// ever diverge. That keeps the operator binary lean while preserving the
// "names cannot silently drift from the registry" contract.
package catalog

// canonicalToolNames is the sorted set of every tool name the v0.1 surface
// exposes. Keep it sorted and in sync with the real tool constructors in
// the parent package (enforced by catalog_drift_test.go there).
var canonicalToolNames = []string{
	"bash",
	"fetch_issue",
	"grep",
	"read_file",
	"run_gate_job",
	"str_replace",
	"submit_result",
	"write_file",
}

// CanonicalToolNames returns a copy of the canonical tool-name set. It is
// the single source of truth the Agent admission webhook consumes to
// reject spec.tools typos at apply time, matching the runtime Filter()
// check the foreman-agent's makeRegistryFactory performs.
func CanonicalToolNames() []string {
	out := make([]string, len(canonicalToolNames))
	copy(out, canonicalToolNames)
	return out
}
