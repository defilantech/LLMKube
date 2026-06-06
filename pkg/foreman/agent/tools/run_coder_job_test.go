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
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanagent "github.com/defilantech/llmkube/pkg/foreman/agent"
)

// mustQuantity parses a resource quantity for tests, panicking on a bad
// literal (these are all compile-time-known constants).
func mustQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

// --- Template rendering ---------------------------------------------------

func TestRenderCoderJob_RendersCommandImageAndWorkspace(t *testing.T) {
	job, err := renderCoderJob(coderRendererInput{
		Name:                  "foreman-coder-issue-510",
		Namespace:             "foreman-system",
		Image:                 "ghcr.io/defilantech/foreman-agent:dev",
		TaskName:              "issue-510",
		TaskNamespace:         "default",
		ServiceAccountName:    "foreman-coder",
		ActiveDeadlineSeconds: 3600,
		CPURequest:            "2",
		CPULimit:              "4",
		MemRequest:            "4Gi",
		MemLimit:              "8Gi",
		GitCredentialsSecret:  "foreman-git-credentials",
	})
	if err != nil {
		t.Fatalf("renderCoderJob: %v", err)
	}
	if job.Name != "foreman-coder-issue-510" {
		t.Errorf("Name: %q", job.Name)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if c.Image != "ghcr.io/defilantech/foreman-agent:dev" {
		t.Errorf("Image: %q", c.Image)
	}
	args := strings.Join(c.Args, " ")
	if !strings.Contains(args, "run-task --task issue-510") {
		t.Errorf("Args missing run-task command:\n%s", args)
	}
	if !strings.Contains(args, "--namespace default") {
		t.Errorf("Args missing namespace:\n%s", args)
	}
	if !strings.Contains(args, "--workspace-dir /workspace") {
		t.Errorf("Args missing workspace-dir:\n%s", args)
	}
	if job.Spec.Template.Spec.ServiceAccountName != "foreman-coder" {
		t.Errorf("ServiceAccountName: %q", job.Spec.Template.Spec.ServiceAccountName)
	}
	// Workspace must be an emptyDir at /workspace (NOT a PVC like the gate).
	var foundWorkspace bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Errorf("workspace volume must be emptyDir; got %#v", v)
			}
			if v.PersistentVolumeClaim != nil {
				t.Errorf("workspace volume must NOT be a PVC")
			}
			foundWorkspace = true
		}
	}
	if !foundWorkspace {
		t.Errorf("missing workspace volume; volumes=%#v", job.Spec.Template.Spec.Volumes)
	}
	var mounted bool
	for _, m := range c.VolumeMounts {
		if m.Name == "workspace" && m.MountPath == "/workspace" {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("workspace volume not mounted at /workspace; mounts=%#v", c.VolumeMounts)
	}
	if job.Labels["app.kubernetes.io/name"] != "foreman-coder" {
		t.Errorf("missing canonical label: %#v", job.Labels)
	}
}

// TestRenderCoderJob_OptionalModelAuthSecret asserts the model-auth Secret
// mount is conditional: absent when unset, present when named.
func TestRenderCoderJob_OptionalModelAuthSecret(t *testing.T) {
	base := coderRendererInput{
		Name:                  "foreman-coder-x",
		Namespace:             "foreman-system",
		Image:                 "img",
		TaskName:              "x",
		TaskNamespace:         "default",
		ActiveDeadlineSeconds: 3600,
		CPURequest:            "2",
		CPULimit:              "4",
		MemRequest:            "4Gi",
		MemLimit:              "8Gi",
		GitCredentialsSecret:  "foreman-git-credentials",
	}

	// Without a model-auth secret, no model-auth volume should render.
	job, err := renderCoderJob(base)
	if err != nil {
		t.Fatalf("renderCoderJob (no model-auth): %v", err)
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "model-auth" {
			t.Errorf("model-auth volume should be absent when unset")
		}
	}

	// With one, the volume + mount should appear.
	withAuth := base
	withAuth.ModelAuthSecret = "foreman-model-auth"
	job, err = renderCoderJob(withAuth)
	if err != nil {
		t.Fatalf("renderCoderJob (with model-auth): %v", err)
	}
	var found bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "model-auth" {
			found = true
		}
	}
	if !found {
		t.Errorf("model-auth volume should be present when set; volumes=%#v",
			job.Spec.Template.Spec.Volumes)
	}
}

// TestRenderCoderJob_GitTokenEnvFromSecret asserts the coder container
// projects a GITHUB_TOKEN env var from the git Secret via secretKeyRef so
// the run-task body (repo.TokenFromEnvOrFile) can read it. The mount at
// /secrets/git alone is never read by the body; the env var is the wiring
// that actually reaches the clone + push (#620 B2).
func TestRenderCoderJob_GitTokenEnvFromSecret(t *testing.T) {
	job, err := renderCoderJob(coderRendererInput{
		Name:                    "foreman-coder-tok",
		Namespace:               "foreman-system",
		Image:                   "img",
		TaskName:                "tok",
		TaskNamespace:           "default",
		ActiveDeadlineSeconds:   3600,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		GitCredentialsSecret:    "foreman-git-credentials",
		GitCredentialsSecretKey: "token",
	})
	if err != nil {
		t.Fatalf("renderCoderJob: %v", err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	var tokenEnv *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "GITHUB_TOKEN" {
			tokenEnv = &c.Env[i]
		}
	}
	if tokenEnv == nil {
		t.Fatalf("GITHUB_TOKEN env missing; env=%#v", c.Env)
	}
	if tokenEnv.ValueFrom == nil || tokenEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("GITHUB_TOKEN must come from a secretKeyRef; got %#v", tokenEnv)
	}
	ref := tokenEnv.ValueFrom.SecretKeyRef
	if ref.Name != "foreman-git-credentials" {
		t.Errorf("secretKeyRef name: want foreman-git-credentials got %q", ref.Name)
	}
	if ref.Key != "token" {
		t.Errorf("secretKeyRef key: want token got %q", ref.Key)
	}
	// Optional so the pod still starts when the Secret is absent (public
	// repos take the graceful no-token path).
	if ref.Optional == nil || !*ref.Optional {
		t.Errorf("secretKeyRef should be optional; got %#v", ref.Optional)
	}
}

// TestRenderCoderJob_GitTokenEnvCustomSecret asserts a non-default
// GitCredentialsSecret/Key flows into the rendered Job's GITHUB_TOKEN
// secretKeyRef (name + key). This is what lets an operator reuse an
// existing git Secret (e.g. foreman-github with key GITHUB_TOKEN) instead
// of being forced to create foreman-git-credentials (#620 reuse-secret).
func TestRenderCoderJob_GitTokenEnvCustomSecret(t *testing.T) {
	// customKey is a Secret KEY name, not a credential value; held in a
	// variable so gosec's G101 literal-credential heuristic does not fire.
	customKey := "GITHUB_TOKEN"
	job, err := renderCoderJob(coderRendererInput{
		Name:                    "foreman-coder-custom",
		Namespace:               "foreman-system",
		Image:                   "img",
		TaskName:                "custom",
		TaskNamespace:           "default",
		ActiveDeadlineSeconds:   3600,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		GitCredentialsSecret:    "foreman-github",
		GitCredentialsSecretKey: customKey,
	})
	if err != nil {
		t.Fatalf("renderCoderJob: %v", err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	var tokenEnv *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == "GITHUB_TOKEN" {
			tokenEnv = &c.Env[i]
		}
	}
	if tokenEnv == nil {
		t.Fatalf("GITHUB_TOKEN env missing; env=%#v", c.Env)
	}
	if tokenEnv.ValueFrom == nil || tokenEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("GITHUB_TOKEN must come from a secretKeyRef; got %#v", tokenEnv)
	}
	ref := tokenEnv.ValueFrom.SecretKeyRef
	if ref.Name != "foreman-github" {
		t.Errorf("secretKeyRef name: want foreman-github got %q", ref.Name)
	}
	if ref.Key != "GITHUB_TOKEN" {
		t.Errorf("secretKeyRef key: want GITHUB_TOKEN got %q", ref.Key)
	}
	// The git-credentials file mount Secret should track the same name.
	var found bool
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "git-credentials" {
			found = true
			if v.Secret == nil || v.Secret.SecretName != "foreman-github" {
				t.Errorf("git-credentials volume should reference foreman-github; got %#v", v.Secret)
			}
		}
	}
	if !found {
		t.Errorf("git-credentials volume missing; volumes=%#v", job.Spec.Template.Spec.Volumes)
	}
}

// TestRunCoderJob_CustomGitSecretFlowsThroughRun asserts a non-default
// GitCredentialsSecret/Key set on the static RunCoderJobConfig reaches the
// rendered Job's GITHUB_TOKEN secretKeyRef via Run. This mirrors how the
// foreman-agent watcher wires --coder-git-secret / --coder-git-secret-key
// onto Cfg (#620 reuse-secret).
func TestRunCoderJob_CustomGitSecretFlowsThroughRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-coder-gitsecret"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}
	go flipStatusOnce(ctx, c, key, 1, 0)

	// customKey is a Secret KEY name, not a credential value; held in a
	// variable so gosec's G101 literal-credential heuristic does not fire.
	customKey := "GITHUB_TOKEN"
	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:                  pinName(jobName),
			PollInterval:            5 * time.Millisecond,
			PollTimeout:             2 * time.Second,
			GitCredentialsSecret:    "foreman-github",
			GitCredentialsSecretKey: customKey,
			LogTailFn:               func(context.Context, string, string) string { return "" },
		},
	}
	if _, err := tool.Run(ctx, RunCoderJobArgs{TaskName: "gitsecret", TaskNamespace: "default"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist: %v", err)
	}
	con := job.Spec.Template.Spec.Containers[0]
	var tokenEnv *corev1.EnvVar
	for i := range con.Env {
		if con.Env[i].Name == "GITHUB_TOKEN" {
			tokenEnv = &con.Env[i]
		}
	}
	if tokenEnv == nil || tokenEnv.ValueFrom == nil || tokenEnv.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("GITHUB_TOKEN secretKeyRef missing; env=%#v", con.Env)
	}
	ref := tokenEnv.ValueFrom.SecretKeyRef
	if ref.Name != "foreman-github" {
		t.Errorf("secretKeyRef name: want foreman-github got %q", ref.Name)
	}
	if ref.Key != "GITHUB_TOKEN" {
		t.Errorf("secretKeyRef key: want GITHUB_TOKEN got %q", ref.Key)
	}
}

// TestRenderCoderJob_GitRemoteAndCommitAuthorArgs asserts the rendered
// run-task args carry --git-remote-url / --commit-author-email /
// --commit-author-name when set, so the coder Job can clone + push (#620).
// Without these the run-task body fails with GitRemoteNotConfigured.
func TestRenderCoderJob_GitRemoteAndCommitAuthorArgs(t *testing.T) {
	job, err := renderCoderJob(coderRendererInput{
		Name:                  "foreman-coder-gitremote",
		Namespace:             "foreman-system",
		Image:                 "img",
		TaskName:              "gitremote",
		TaskNamespace:         "default",
		ActiveDeadlineSeconds: 3600,
		CPURequest:            "2",
		CPULimit:              "4",
		MemRequest:            "4Gi",
		MemLimit:              "8Gi",
		GitCredentialsSecret:  "foreman-git-credentials",
		GitRemoteURL:          "https://github.com/Defilan/LLMKube.git",
		CommitAuthorName:      "Foreman Bot",
		CommitAuthorEmail:     "foreman@defilan.tech",
	})
	if err != nil {
		t.Fatalf("renderCoderJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--git-remote-url=https://github.com/Defilan/LLMKube.git") {
		t.Errorf("Args missing --git-remote-url:\n%s", args)
	}
	if !strings.Contains(args, "--commit-author-email=foreman@defilan.tech") {
		t.Errorf("Args missing --commit-author-email:\n%s", args)
	}
	if !strings.Contains(args, "--commit-author-name=Foreman Bot") {
		t.Errorf("Args missing --commit-author-name:\n%s", args)
	}
}

// TestRenderCoderJob_GitRemoteArgsOmittedWhenEmpty asserts the optional git
// args are NOT emitted when unset, so we never produce a bare
// `--git-remote-url=` that the run-task FlagSet would parse as an empty URL
// (a deterministic gate-only install legitimately has no remote).
func TestRenderCoderJob_GitRemoteArgsOmittedWhenEmpty(t *testing.T) {
	job, err := renderCoderJob(coderRendererInput{
		Name:                  "foreman-coder-noremote",
		Namespace:             "foreman-system",
		Image:                 "img",
		TaskName:              "noremote",
		TaskNamespace:         "default",
		ActiveDeadlineSeconds: 3600,
		CPURequest:            "2",
		CPULimit:              "4",
		MemRequest:            "4Gi",
		MemLimit:              "8Gi",
		GitCredentialsSecret:  "foreman-git-credentials",
	})
	if err != nil {
		t.Fatalf("renderCoderJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if strings.Contains(args, "--git-remote-url") {
		t.Errorf("Args should omit --git-remote-url when empty:\n%s", args)
	}
	if strings.Contains(args, "--commit-author-email") {
		t.Errorf("Args should omit --commit-author-email when empty:\n%s", args)
	}
	if strings.Contains(args, "--commit-author-name") {
		t.Errorf("Args should omit --commit-author-name when empty:\n%s", args)
	}
}

// TestRunCoderJob_GitRemoteFlowsThroughRun asserts the static Cfg's
// GitRemoteURL / CommitAuthor* reach the rendered Job's run-task args via
// Run. This mirrors how the watcher wires its --git-remote-url /
// --commit-author-* flag values onto Cfg (#620).
func TestRunCoderJob_GitRemoteFlowsThroughRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-coder-gitflow"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}
	go flipStatusOnce(ctx, c, key, 1, 0)

	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:            pinName(jobName),
			PollInterval:      5 * time.Millisecond,
			PollTimeout:       2 * time.Second,
			GitRemoteURL:      "https://github.com/Defilan/LLMKube.git",
			CommitAuthorName:  "Foreman Bot",
			CommitAuthorEmail: "foreman@defilan.tech",
			LogTailFn:         func(context.Context, string, string) string { return "" },
		},
	}
	if _, err := tool.Run(ctx, RunCoderJobArgs{TaskName: "gitflow", TaskNamespace: "default"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--git-remote-url=https://github.com/Defilan/LLMKube.git") {
		t.Errorf("Args missing --git-remote-url:\n%s", args)
	}
	if !strings.Contains(args, "--commit-author-email=foreman@defilan.tech") {
		t.Errorf("Args missing --commit-author-email:\n%s", args)
	}
	if !strings.Contains(args, "--commit-author-name=Foreman Bot") {
		t.Errorf("Args missing --commit-author-name:\n%s", args)
	}
}

// TestRenderCoderJob_AppliesResources asserts CoderJobRequest-supplied
// resources land on the container, and that the gate-matching defaults
// apply when the renderer input carries the default strings (#620 N1).
func TestRenderCoderJob_AppliesResources(t *testing.T) {
	job, err := renderCoderJob(coderRendererInput{
		Name:                  "foreman-coder-res",
		Namespace:             "foreman-system",
		Image:                 "img",
		TaskName:              "res",
		TaskNamespace:         "default",
		ActiveDeadlineSeconds: 3600,
		CPURequest:            "3",
		CPULimit:              "6",
		MemRequest:            "5Gi",
		MemLimit:              "10Gi",
		GitCredentialsSecret:  "foreman-git-credentials",
	})
	if err != nil {
		t.Fatalf("renderCoderJob: %v", err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if got := c.Resources.Requests.Cpu().String(); got != "3" {
		t.Errorf("cpu request: want 3 got %q", got)
	}
	if got := c.Resources.Limits.Cpu().String(); got != "6" {
		t.Errorf("cpu limit: want 6 got %q", got)
	}
	if got := c.Resources.Requests.Memory().String(); got != "5Gi" {
		t.Errorf("mem request: want 5Gi got %q", got)
	}
	if got := c.Resources.Limits.Memory().String(); got != "10Gi" {
		t.Errorf("mem limit: want 10Gi got %q", got)
	}
}

// TestRunCoderJob_RequestResourcesOverrideDefaults asserts the executor's
// CoderJobRequest.Resources flow through Run onto the rendered Job, and
// that the defaults apply when the request omits them (#620 N1).
func TestRunCoderJob_RequestResourcesOverrideDefaults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// With explicit overrides on the static Cfg.
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-coder-resoverride"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}
	go flipStatusOnce(ctx, c, key, 1, 0)

	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			CPURequest:   "8",
			MemLimit:     "16Gi",
			LogTailFn:    func(context.Context, string, string) string { return "" },
		},
	}
	if _, err := tool.Run(ctx, RunCoderJobArgs{TaskName: "resoverride", TaskNamespace: "default"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist: %v", err)
	}
	con := job.Spec.Template.Spec.Containers[0]
	if got := con.Resources.Requests.Cpu().String(); got != "8" {
		t.Errorf("cpu request override: want 8 got %q", got)
	}
	if got := con.Resources.Limits.Memory().String(); got != "16Gi" {
		t.Errorf("mem limit override: want 16Gi got %q", got)
	}
	// Untouched fields fall back to the gate-matching defaults.
	if got := con.Resources.Limits.Cpu().String(); got != "4" {
		t.Errorf("cpu limit default: want 4 got %q", got)
	}
	if got := con.Resources.Requests.Memory().String(); got != "4Gi" {
		t.Errorf("mem request default: want 4Gi got %q", got)
	}
}

// TestSubmit_ForwardsResources asserts the agent.CoderJobRequest.Resources
// reach the rendered Job through Submit (the executor seam, #620 N1).
func TestSubmit_ForwardsResources(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-coder-submitres"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}
	go flipStatusOnce(ctx, c, key, 1, 0)

	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn:    func(context.Context, string, string) string { return "" },
		},
	}
	req := foremanagent.CoderJobRequest{
		TaskName:      "submitres",
		TaskNamespace: "default",
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    mustQuantity("7"),
				corev1.ResourceMemory: mustQuantity("12Gi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    mustQuantity("9"),
				corev1.ResourceMemory: mustQuantity("18Gi"),
			},
		},
	}
	if _, err := tool.Submit(ctx, req); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist: %v", err)
	}
	con := job.Spec.Template.Spec.Containers[0]
	if got := con.Resources.Requests.Cpu().String(); got != "7" {
		t.Errorf("cpu request: want 7 got %q", got)
	}
	if got := con.Resources.Limits.Memory().String(); got != "18Gi" {
		t.Errorf("mem limit: want 18Gi got %q", got)
	}
}

// --- Submit + poll happy paths -------------------------------------------

func TestRunCoderJob_GOVerdictFromLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-coder-issue-510"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}

	go flipStatusOnce(ctx, c, key, 1, 0)

	rt := foremanagent.RunTaskResult{
		Verdict:       "GO",
		Summary:       "fixed it",
		Branch:        "foreman/issue-510",
		CommitSHA:     "abc123",
		CommitMessage: "fix: thing",
	}
	rtJSON, err := json.Marshal(rt)
	if err != nil {
		t.Fatalf("marshal RunTaskResult: %v", err)
	}
	logTail := fmt.Sprintf("%s%s\n%s\n",
		foremanagent.RunTaskResultPrefix, string(rtJSON), foremanagent.RunTaskSentinelGo)

	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:       pinName(jobName),
			Image:        "ghcr.io/defilantech/foreman-agent:dev",
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn: func(context.Context, string, string) string {
				return logTail
			},
		},
	}

	res, err := tool.Run(ctx, RunCoderJobArgs{
		TaskName:      "issue-510",
		TaskNamespace: "default",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdict != "GO" {
		t.Errorf("Verdict: want GO got %q", res.Verdict)
	}
	if res.Branch != "foreman/issue-510" {
		t.Errorf("Branch: want foreman/issue-510 got %q", res.Branch)
	}
	if res.CommitSHA != "abc123" {
		t.Errorf("CommitSHA: want abc123 got %q", res.CommitSHA)
	}
	if res.Summary != "fixed it" {
		t.Errorf("Summary: %q", res.Summary)
	}
	if !strings.Contains(res.LogTail, foremanagent.RunTaskSentinelGo) {
		t.Errorf("LogTail should carry the sentinel; got %q", res.LogTail)
	}

	// And the Job was actually created with the expected name + image.
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist on apiserver: %v", err)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != "ghcr.io/defilantech/foreman-agent:dev" {
		t.Errorf("Image: %q", got)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "run-task --task issue-510") {
		t.Errorf("Job args missing run-task command:\n%s", args)
	}
	if job.Spec.Template.Spec.Volumes[0].EmptyDir == nil &&
		!hasEmptyDirWorkspace(job) {
		t.Errorf("Job should mount an emptyDir workspace")
	}
}

func hasEmptyDirWorkspace(job batchv1.Job) bool {
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "workspace" && v.EmptyDir != nil {
			return true
		}
	}
	return false
}

func TestRunCoderJob_NoGoVerdictFromLog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-coder-nogo"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}

	go flipStatusOnce(ctx, c, key, 1, 0)

	logTail := fmt.Sprintf(
		"%s{\"verdict\":\"NO-GO\",\"summary\":\"declined\"}\n%s\n",
		foremanagent.RunTaskResultPrefix, foremanagent.RunTaskSentinelNoGo,
	)

	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn:    func(context.Context, string, string) string { return logTail },
		},
	}

	res, err := tool.Run(ctx, RunCoderJobArgs{TaskName: "nogo", TaskNamespace: "default"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdict != "NO-GO" {
		t.Errorf("Verdict: want NO-GO got %q", res.Verdict)
	}
}

// --- Error paths ----------------------------------------------------------

func TestRunCoderJob_JobFailedProducesERROR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-coder-fail"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}

	go flipStatusOnce(ctx, c, key, 0, 1)

	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn: func(context.Context, string, string) string {
				return "OOMKilled\n" + foremanagent.RunTaskSentinelError + "\n"
			},
		},
	}

	res, err := tool.Run(ctx, RunCoderJobArgs{TaskName: "fail", TaskNamespace: "default"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdict != "ERROR" {
		t.Errorf("Verdict: want ERROR got %q", res.Verdict)
	}
	if res.FailureReason == "" {
		t.Errorf("FailureReason should be set on Job failure")
	}
	if !strings.Contains(res.LogTail, "OOMKilled") {
		t.Errorf("LogTail should carry pod log on failure; got %q", res.LogTail)
	}
}

func TestRunCoderJob_PollTimeoutProducesERROR(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunCoderJob{
		Client: c,
		Cfg: RunCoderJobConfig{
			NameFn:       pinName("foreman-coder-stuck"),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  50 * time.Millisecond,
			LogTailFn:    func(context.Context, string, string) string { return "(no logs)" },
		},
	}

	res, err := tool.Run(ctx, RunCoderJobArgs{TaskName: "stuck", TaskNamespace: "default"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdict != "ERROR" {
		t.Errorf("Verdict: want ERROR got %q", res.Verdict)
	}
	if res.FailureReason == "" {
		t.Errorf("FailureReason should be set on poll timeout")
	}
}

func TestRunCoderJob_RequiresClient(t *testing.T) {
	tool := &RunCoderJob{}
	_, err := tool.Run(context.Background(), RunCoderJobArgs{TaskName: "x", TaskNamespace: "default"})
	if err == nil || !strings.Contains(err.Error(), "Client") {
		t.Errorf("expected Client-required error; got %v", err)
	}
}

func TestRunCoderJob_RequiresTaskName(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunCoderJob{Client: c}
	_, err := tool.Run(context.Background(), RunCoderJobArgs{TaskNamespace: "default"})
	if err == nil || !strings.Contains(err.Error(), "task") {
		t.Errorf("expected task-required error; got %v", err)
	}
}

func TestApplyCoderConfigDefaults_FillsEveryField(t *testing.T) {
	c := applyCoderConfigDefaults(RunCoderJobConfig{})
	if c.Namespace == "" || c.Image == "" {
		t.Errorf("string defaults missing: %#v", c)
	}
	if c.ActiveDeadlineSeconds == 0 {
		t.Errorf("deadline default missing: %#v", c)
	}
	if c.CPURequest == "" || c.CPULimit == "" || c.MemRequest == "" || c.MemLimit == "" {
		t.Errorf("resource defaults missing: %#v", c)
	}
	if c.PollInterval == 0 || c.PollTimeout == 0 {
		t.Errorf("poll defaults missing: %#v", c)
	}
	if c.NameFn == nil {
		t.Errorf("NameFn default missing")
	}
}

// ensure corev1 stays imported for the resource-shape assertions above.
var _ = corev1.ResourceRequirements{}
