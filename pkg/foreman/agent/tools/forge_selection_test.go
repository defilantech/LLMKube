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

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/defilantech/llmkube/pkg/foreman/agent/codehost"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubprfetch"
)

// The watcher's forge settings reach the coder Job's run-task, so the Job
// pushes to and opens pull requests on the same forge (#1300).
func TestRunCoderJobPassesForgeFlags(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-coder-forge"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}
	go flipStatusOnce(ctx, c, key, 1, 0)

	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:          pinName(jobName),
			PollInterval:    5 * time.Millisecond,
			PollTimeout:     2 * time.Second,
			Forge:           "github",
			ForgeAPIBaseURL: "https://ghe.example.com/api/v3",
			LogTailFn:       func(context.Context, string, string) string { return "" },
		},
	}
	if _, err := tool.Run(ctx, RunCoderJobArgs{TaskName: "forge", TaskNamespace: "default"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	for _, want := range []string{"--forge=github", "--forge-api-base-url=https://ghe.example.com/api/v3"} {
		if !strings.Contains(args, want) {
			t.Errorf("Job args missing %s:\n%s", want, args)
		}
	}
}

// Unset forge settings leave the flags off, so run-task infers the forge the
// way the watcher did instead of parsing an empty value.
func TestRenderCoderJobOmitsUnsetForgeFlags(t *testing.T) {
	job, err := renderCoderJob(coderRendererInput{
		Name: "foreman-coder-noforge", Namespace: "foreman-system", Image: "img",
		TaskName: "noforge", TaskNamespace: "default", ActiveDeadlineSeconds: 3600,
		CPURequest: "2", CPULimit: "4", MemRequest: "4Gi", MemLimit: "8Gi",
		GitCredentialsSecret: "foreman-git-credentials",
	})
	if err != nil {
		t.Fatalf("renderCoderJob: %v", err)
	}
	if args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " "); strings.Contains(args, "--forge") {
		t.Errorf("Args should omit the forge flags when unset:\n%s", args)
	}
}

// A selected forge that cannot read pull requests makes fetch_pull_request
// say so with codehost.ErrNotSupported, rather than quietly reading GitHub.
// With no forge selected the tool keeps its GitHub client.
func TestFetchPullRequestReportsUnsupportedForge(t *testing.T) {
	tool := findTool(t, BuildAll(ToolDeps{ForgeName: "gitea"}), "fetch_pull_request").(*FetchPullRequestTool)
	if tool.Fetcher != nil {
		t.Fatalf("Fetcher = %T, want nil for a forge without a pull request reader", tool.Fetcher)
	}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"o/r","number":1}`))
	if !errors.Is(err, codehost.ErrNotSupported) || !strings.Contains(err.Error(), `"gitea"`) {
		t.Errorf("Execute err = %v, want codehost.ErrNotSupported naming gitea", err)
	}

	legacy := findTool(t, BuildAll(ToolDeps{}), "fetch_pull_request").(*FetchPullRequestTool)
	if _, ok := legacy.Fetcher.(*githubprfetch.Client); !ok {
		t.Errorf("Fetcher = %T with no forge selected, want the GitHub client", legacy.Fetcher)
	}
}

func findTool(t *testing.T, all []Tool, name string) Tool {
	t.Helper()
	for _, tool := range all {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("BuildAll has no %s", name)
	return nil
}
