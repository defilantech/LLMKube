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

package agent

import (
	"time"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// ResultSchemaVersion is the on-the-wire identifier of the structured
// result envelope every Foreman executor emits. v0.1 ships v1; later
// additive changes (new optional fields) keep the same version, breaking
// changes bump to v2 and the parser gates on this string.
const ResultSchemaVersion = "foreman.v1"

// Result is the structured envelope every Executor returns. The watcher
// serializes it into AgenticTask.status.result as a JSON RawExtension; the
// next pipeline step (or the human reviewer) reads it from there. The
// envelope is intentionally small and stable: Kind discriminates the
// shape of Extra, Verdict carries the externally-meaningful outcome
// category, and Summary is the one-line "what happened" string.
type Result struct {
	// SchemaVersion is always ResultSchemaVersion. The watcher refuses to
	// store a Result with a missing or mismatched version.
	SchemaVersion string `json:"schemaVersion"`

	// Kind identifies which executor produced this result: "stub" in M2,
	// "issue-fix" / "verify" / "freeform" once M3-M4 land. The Foreman
	// scheduler does not interpret Kind; downstream consumers (the next
	// step in the pipeline, the planner's evaluator) may.
	Kind string `json:"kind"`

	// Verdict is the outcome category. Reuses the AgenticTaskVerdict enum
	// so the value in status.result.verdict matches status.verdict
	// exactly when both are present.
	Verdict foremanv1alpha1.AgenticTaskVerdict `json:"verdict"`

	// Summary is a human-readable one-liner. The watcher copies this into
	// the Completed condition's Message; keep it terse.
	Summary string `json:"summary"`

	// Extra carries Kind-specific fields (e.g. filesChanged, gate check
	// breakdown, agent transcript pointers). Opaque to the watcher.
	Extra map[string]any `json:"extra,omitempty"`

	// ElapsedSec is the wall-clock duration of the executor's Run, in
	// seconds. Recorded by the executor, not the watcher.
	ElapsedSec float64 `json:"elapsedSec"`
}

// NewResult builds a Result with the schema version pre-filled. Callers
// fill Extra after construction if they have Kind-specific fields.
func NewResult(kind string, verdict foremanv1alpha1.AgenticTaskVerdict, summary string, elapsed time.Duration) *Result {
	return &Result{
		SchemaVersion: ResultSchemaVersion,
		Kind:          kind,
		Verdict:       verdict,
		Summary:       summary,
		ElapsedSec:    elapsed.Seconds(),
	}
}
