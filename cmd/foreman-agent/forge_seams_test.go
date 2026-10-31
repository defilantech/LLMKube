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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
	foremantools "github.com/defilantech/llmkube/pkg/foreman/agent/tools"
	"github.com/defilantech/llmkube/pkg/foreman/agent/worktracker"
)

type recordingWorkItems struct{ asked []string }

func (r *recordingWorkItems) Get(_ context.Context, repoSlug, id string) (*worktracker.WorkItem, error) {
	r.asked = append(r.asked, repoSlug+"#"+id)
	return &worktracker.WorkItem{ID: id, Title: "from the injected forge"}, nil
}

// The agent's tools reach the forge seams the executor was given. The
// registry factory left ToolDeps.WorkItems and CodeHost nil, so fetch_issue
// and the gate's clone URL always fell back to GitHub whatever seam the
// executor held (#1298).
func TestRegistryFactoryThreadsForgeSeamsIntoTools(t *testing.T) {
	items := &recordingWorkItems{}
	factory := makeRegistryFactory(fake.NewClientBuilder().Build(), k8sfake.NewClientset(), "foreman", "",
		forgeSeams{workItems: items})
	ag := &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{Tools: []string{"fetch_issue"}}}
	reg, err := factory(context.Background(), t.TempDir(), ag, false)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	res, err := reg.Dispatch(context.Background(), "fetch_issue", json.RawMessage(`{"repo":"group/project","number":7}`))
	if err != nil {
		t.Fatalf("fetch_issue: %v", err)
	}
	if len(items.asked) != 1 || items.asked[0] != "group/project#7" {
		t.Errorf("injected WorkItems calls = %v, want one for group/project#7", items.asked)
	}
	if out, _ := json.Marshal(res.Output); !strings.Contains(string(out), "from the injected forge") {
		t.Errorf("fetch_issue output = %s, want the injected forge's work item", out)
	}
}

// The flags a coder Job hands run-task must all be defined on the run-task
// FlagSet, or every coder Job exits 2 at startup. The Job's real rendered args
// are parsed by runTaskCommand in a child process; KUBECONFIG points at an
// empty file, so once the flags parse the child fails on cluster config and
// can reach no cluster.
func TestCoderJobArgsParseInRunTask(t *testing.T) {
	if raw := os.Getenv("FOREMAN_TEST_RUN_TASK_ARGS"); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			os.Exit(3)
		}
		runTaskCommand(args)
		os.Exit(0)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	tool := &foremantools.RunCoderJob{
		Client: c,
		Cfg: foremantools.RunCoderJobConfig{
			Namespace:            "foreman-system",
			NameFn:               func(string) string { return "coder-flags" },
			PollInterval:         5 * time.Millisecond,
			PollTimeout:          20 * time.Millisecond,
			GitRemoteURL:         "https://github.com/o/r.git",
			Forge:                "github",
			ForgeAPIBaseURL:      "https://ghe.example.com/api/v3",
			CommitAuthorName:     "Foreman Bot",
			CommitAuthorEmail:    "bot@example.com",
			CommitCommitterName:  "Human",
			CommitCommitterEmail: "human@example.com",
			LogTailFn:            func(context.Context, string, string) string { return "" },
		},
	}
	_, _ = tool.Run(context.Background(), foremantools.RunCoderJobArgs{TaskName: "flags", TaskNamespace: "default"})
	var job batchv1.Job
	key := types.NamespacedName{Namespace: "foreman-system", Name: "coder-flags"}
	if err := c.Get(context.Background(), key, &job); err != nil {
		t.Fatalf("coder Job not created: %v", err)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	if len(args) == 0 || args[0] != "run-task" {
		t.Fatalf("coder Job args = %q, want them to start with run-task", args)
	}
	if !slices.Contains(args, "--forge=github") {
		t.Fatalf("coder Job args = %q, want --forge among them", args)
	}
	raw, _ := json.Marshal(args[1:])
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G702: re-executes this test binary with a fixed -test.run; the args are the Job's, read as data.
	child := exec.Command(os.Args[0], "-test.run=^TestCoderJobArgsParseInRunTask$")
	child.Env = append(os.Environ(), "FOREMAN_TEST_RUN_TASK_ARGS="+string(raw), "KUBECONFIG="+kubeconfig,
		"KUBERNETES_SERVICE_HOST=", "KUBERNETES_SERVICE_PORT=")
	out, err := child.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 2 || strings.Contains(string(out), "flag provided but not defined") {
		t.Errorf("run-task rejected the coder Job's args %q:\n%s", args[1:], out)
	}
}

// The agent selects its forge at startup: GitHub when nothing names one, and
// an error, not a silent GitHub fallback, for a forge this build lacks.
func TestNewForgeSeamsSelection(t *testing.T) {
	seams, err := newForgeSeams("", "", "https://github.com/o/r.git")
	if err != nil || seams.name != "github" ||
		seams.codeHost == nil || seams.workItems == nil || seams.pullRequests == nil {
		t.Fatalf("newForgeSeams = %+v, %v; want the github provider with every seam", seams, err)
	}
	if _, err := newForgeSeams("", "", "git@gitlab.com:group/p.git"); err == nil ||
		!strings.Contains(err.Error(), "not built into this agent") {
		t.Errorf("gitlab.com remote: err = %v, want it refused, not served by GitHub", err)
	}
	if _, err := newForgeSeams("github", "ghe.example.com", ""); err == nil {
		t.Error("a relative --forge-api-base-url was accepted")
	}
}

// The selected forge's name and pull request reader reach fetch_pull_request:
// a forge without a reader is reported, not served from GitHub.
func TestRegistryFactoryThreadsForgePullRequests(t *testing.T) {
	factory := makeRegistryFactory(fake.NewClientBuilder().Build(), k8sfake.NewClientset(), "foreman", "",
		forgeSeams{name: "gitea", workItems: &recordingWorkItems{}})
	ag := &foremanv1alpha1.Agent{Spec: foremanv1alpha1.AgentSpec{Tools: []string{"fetch_pull_request"}}}
	reg, err := factory(context.Background(), t.TempDir(), ag, false)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	_, err = reg.Dispatch(context.Background(), "fetch_pull_request", json.RawMessage(`{"repo":"o/r","number":1}`))
	if !errors.Is(err, codehost.ErrNotSupported) {
		t.Errorf("fetch_pull_request on a forge without a reader: err = %v, want codehost.ErrNotSupported", err)
	}
}
