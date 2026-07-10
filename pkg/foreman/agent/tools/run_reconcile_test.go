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
	"encoding/json"
	"path/filepath"
	"testing"
)

// reconcileFixture builds a bare origin with an integration branch that holds
// the given files, and clones it into a workspace. Returns the workspace path.
func reconcileFixture(t *testing.T, integBranch string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", origin)

	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", origin, seed)
	git(t, seed, "config", "user.email", "t@e.com")
	git(t, seed, "config", "user.name", "T")
	git(t, seed, "config", "commit.gpgsign", "false")
	git(t, seed, "checkout", "-q", "-B", integBranch)
	for p, c := range files {
		writeFile(t, seed, p, c)
	}
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-qm", "union")
	git(t, seed, "push", "-q", "origin", integBranch)

	ws := filepath.Join(root, "ws")
	git(t, root, "clone", "-q", origin, ws)
	return ws
}

func TestRunReconcile_CleanIsGatePass(t *testing.T) {
	ws := reconcileFixture(t, "foreman/s/integ", map[string]string{
		"config/exp.yaml":  "emit rocm_smi_gpu_temp here",
		"config/dash.json": `{"expr":"rocm_smi_gpu_temp"}`,
	})
	tool := &RunReconcileTool{Workspace: ws}
	args, _ := json.Marshal(map[string]any{
		"branch": "foreman/s/integ",
		"slices": []map[string]any{
			{"name": "exp", "files": []string{"config/exp.yaml"}},
			{"name": "dash", "files": []string{"config/dash.json"}},
		},
		"sharedIdentifiers": []map[string]any{
			{"id": "rocm_smi_gpu_temp", "definedBy": "exp", "referencedBy": []string{"dash"}},
		},
	})
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGatePass {
		t.Fatalf("verdict = %q (%s), want GATE-PASS", res.Verdict, res.Summary)
	}
}

func TestRunReconcile_DriftIsGateFail(t *testing.T) {
	// the exporter emits a DIFFERENT metric than the dashboard queries.
	ws := reconcileFixture(t, "foreman/s/integ", map[string]string{
		"config/exp.yaml":  "emit rocm_smi_temperature_edge here",
		"config/dash.json": `{"expr":"rocm_smi_gpu_temp"}`,
	})
	tool := &RunReconcileTool{Workspace: ws}
	args, _ := json.Marshal(map[string]any{
		"branch": "foreman/s/integ",
		"slices": []map[string]any{
			{"name": "exp", "files": []string{"config/exp.yaml"}},
			{"name": "dash", "files": []string{"config/dash.json"}},
		},
		"sharedIdentifiers": []map[string]any{
			{"id": "rocm_smi_gpu_temp", "definedBy": "exp", "referencedBy": []string{"dash"}},
		},
	})
	res, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateFail {
		t.Fatalf("verdict = %q (%s), want GATE-FAIL for drift", res.Verdict, res.Summary)
	}
}

func TestRunReconcile_ArgvInjectionRejected(t *testing.T) {
	tool := &RunReconcileTool{Workspace: t.TempDir()}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"branch":"--upload-pack=x"}`))
	if res.Verdict != VerdictGateError {
		t.Fatalf("verdict = %q, want GATE-ERROR for option-looking branch", res.Verdict)
	}
}
