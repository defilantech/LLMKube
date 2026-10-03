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

package tools

import (
	"context"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubprfetch"
	"github.com/defilantech/llmkube/pkg/foreman/agent/worktracker"
)

// BuildAll is the single place the agent's native tool set is constructed.
//
// It used to live inline in cmd/foreman-agent/main.go, which made three copies
// of the same list: the runtime registry there, the webhook allow-list in
// catalog, and a hand-written mirror in the drift test. The drift test compared
// the two copies nobody edited, so it passed while fetch_pull_request,
// run_integrate and run_reconcile all shipped in v0.9.16 registered at runtime
// but rejected by admission, and therefore unusable (#1482).
//
// With construction here, main.go has no list to drift from, and
// TestBuildAllMatchesCatalog derives one side of the comparison from these real
// constructors rather than from a mirror. Adding a tool to this function
// without adding its name to catalog fails that test, and the reverse fails too.
//
// Why this package rather than catalog: catalog is a deliberate zero-import
// leaf so the operator's Agent admission webhook can validate spec.tools
// without linking Kubernetes clients, GitHub clients and the slicer. The
// dependency has to run tools -> catalog, never the other way.
func BuildAll(deps ToolDeps) []Tool {
	return []Tool{
		&ReadFileTool{Workspace: deps.Workspace},
		&WriteFileTool{Workspace: deps.Workspace},
		&StrReplaceTool{Workspace: deps.Workspace},
		&GrepTool{Workspace: deps.Workspace},
		&BashTool{Workspace: deps.Workspace, Timeout: deps.BashTimeout},
		// ripwire: read-only call-graph queries for the coder (#1905). Off
		// until an Agent whitelists it in spec.tools; the binary must be on
		// the host (see the packaging story) or the call returns a tool error.
		&RipwireTool{Workspace: deps.Workspace},
		SubmitResultTool{},
		&RunGateJobTool{
			Client: deps.Client,
			Cfg: RunGateJobToolConfig{
				Namespace:             deps.ForemanNamespace,
				PVCName:               deps.GateCachePVC,
				LogTailFn:             deps.LogTailFn,
				CodeHost:              deps.CodeHost,
				ActiveDeadlineSeconds: deps.GateActiveDeadlineSeconds,
			},
		},
		// fetch_issue: read-only GitHub issue surface for the reviewer. The
		// same token the foreman-agent already loads at startup reaches GitHub
		// through one bounded Go-side call instead of being inherited by every
		// bash subprocess via $GH_TOKEN. Closes #580.
		&FetchIssueTool{
			// WorkItems wins when injected; Fetcher/Token remain the
			// GitHub default so nothing changes for a GitHub fleet.
			WorkItems: deps.WorkItems,
			Fetcher:   githubissue.NewClient(),
			Token:     deps.Token,
		},
		// fetch_pull_request: read-only GitHub PR surface for the coder, same
		// reasoning as fetch_issue. Closes #1434.
		&FetchPullRequestTool{
			Fetcher: githubprfetch.NewClient(),
			Token:   deps.Token,
		},
		// run_integrate: deterministic tool for a sliced Workload's integrate
		// step. Unions the disjoint slice branches onto the current base and
		// pushes the integration branch (#1033).
		&RunIntegrateTool{
			Workspace: deps.Workspace,
			Token:     deps.Token,
		},
		// run_reconcile: deterministic tool for a sliced Workload's reconcile
		// step. Checks the integrated union against the pinned shared
		// identifiers for cross-slice interface drift (#1033).
		&RunReconcileTool{
			Workspace: deps.Workspace,
			Token:     deps.Token,
		},
	}
}

// ToolDeps carries everything the native tool set needs from the process that
// hosts it. Passing a struct rather than a long parameter list means adding a
// dependency for one tool does not churn every call site.
type ToolDeps struct {
	// Workspace is the per-task clone directory. File tools resolve every
	// relative path against it and refuse to escape it.
	Workspace string

	// BashTimeout bounds a single bash invocation.
	BashTimeout time.Duration

	// Client and ForemanNamespace let run_gate_job create and watch the gate
	// Job in the operator's namespace.
	Client           client.Client
	ForemanNamespace string

	// GateCachePVC is the submitting agent's gate-cache PVC name, threaded
	// from --gate-cache-pvc so the gate Job mounts the per-agent claim the
	// chart creates (foreman.gateCache.pvcName). Empty keeps the historical
	// "no volume mount" behavior (#1538).
	GateCachePVC string

	// GateActiveDeadlineSeconds bounds the gate Job's wall-clock runtime,
	// threaded from the verifying Agent's
	// spec.execution.activeDeadlineSeconds so a heavy per-hunk mutation
	// pass can be given more than the 1800 s default (#1748). Zero keeps
	// the default via applyConfigDefaults.
	GateActiveDeadlineSeconds int32

	// LogTailFn reads a finished gate Job's pod logs back for the model.
	LogTailFn func(ctx context.Context, namespace, jobName string) string

	// Token resolves the GitHub credential at call time rather than at
	// startup, so a rotated token is picked up without restarting the agent.
	Token func() (string, error)

	// WorkItems is the provider-neutral work-item seam (#1158). Nil keeps
	// the GitHub default. Set it and fetch_issue reaches the injected
	// provider instead of holding its own GitHub client, which is the tool
	// half of #1298.
	WorkItems worktracker.WorkItems

	// CodeHost is the provider-neutral code-host seam (#1158). Nil keeps
	// the GitHub default. run_gate_job derives its clone URL from this
	// rather than a hardcoded https://github.com.
	CodeHost codehost.CodeHost
}
