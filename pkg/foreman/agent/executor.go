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
	"context"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
)

// Executor runs an AgenticTask and produces a structured Result. v0.1
// ships only a stub executor (M2); the native agent loop (M3) and the
// gate-job executor (M4) plug in behind the same interface so the
// watcher does not need to learn about new task kinds.
//
// Implementations honor ctx cancellation: a cancelled ctx during Execute
// must return promptly with ctx.Err(). The watcher cancels ctx on
// SIGTERM so the host can shut down cleanly during a long run.
type Executor interface {
	// Kind identifies this executor for logs and Result.Kind. Stable
	// values: "stub", "issue-fix", "verify", "freeform".
	Kind() string

	// Execute runs the task to completion. A nil error means the
	// executor produced a Result (which itself may carry NO-GO or
	// GATE-FAIL verdict). A non-nil error is a system failure: the
	// executor did not complete, and the watcher patches the task as
	// Failed with the error in the Completed condition.
	Execute(ctx context.Context, task *foremanv1alpha1.AgenticTask) (*Result, error)
}
