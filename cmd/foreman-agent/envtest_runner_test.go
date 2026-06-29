package main

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremantools "github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

func TestMapGateVerdict(t *testing.T) {
	cases := []struct {
		verdict           string
		wantPass, wantRan bool
	}{
		{"GATE-PASS", true, true},
		{"GATE-FAIL", false, true},
		{"GATE-ERROR", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		pass, ran := mapGateVerdict(c.verdict)
		if pass != c.wantPass || ran != c.wantRan {
			t.Errorf("mapGateVerdict(%q) = (%v,%v) want (%v,%v)", c.verdict, pass, ran, c.wantPass, c.wantRan)
		}
	}
}

// TestEnvtestJobRunnerStampsTaskName is the #893 regression: the post-push
// envtest gate Job must carry the originating AgenticTask name in the
// foreman.llmkube.dev/task-name label, not the "unknown"/"task" default the
// gate renderer falls back to when no taskRef is threaded through. The runner
// submits the Job via a fake client; we read the created Job back out and
// assert the label matches the task name the executor passed in.
func TestEnvtestJobRunnerStampsTaskName(t *testing.T) {
	s := runtime.NewScheme()
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("add batchv1 scheme: %v", err)
	}
	kc := fake.NewClientBuilder().WithScheme(s).Build()

	const (
		taskNS   = "foreman-system"
		taskName = "fix-issue-893"
		jobName  = "foreman-gate-fix-issue-893-pinned"
	)

	runner := &envtestJobRunnerImpl{
		tool: &foremantools.RunGateJobTool{
			Client: kc,
			Cfg: foremantools.RunGateJobToolConfig{
				Namespace:    taskNS,
				PollInterval: time.Millisecond,
				// Bound the poll so the test does not block: the fake client
				// never drives the Job to a terminal phase, so Run returns a
				// could-not-run GATE-ERROR. We only care that the Job was
				// created with the right label before polling gives up.
				PollTimeout: 5 * time.Millisecond,
				NameFn:      func(string) string { return jobName },
			},
		},
	}

	// The runner does not need to reach a verdict for this assertion; the Job
	// is created up front, before polling. Run with a short-lived context.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _ = runner.Run(
		ctx, taskNS, taskName,
		"defilantech/LLMKube", "foreman/issue-893",
		"https://github.com/Defilan/LLMKube.git",
	)

	var job batchv1.Job
	key := types.NamespacedName{Namespace: taskNS, Name: jobName}
	if err := kc.Get(context.Background(), key, &job); err != nil {
		t.Fatalf("gate Job %s was not created: %v", key, err)
	}

	if got := job.Labels["foreman.llmkube.dev/task-name"]; got != taskName {
		t.Errorf("gate Job task-name label = %q, want %q", got, taskName)
	}
	if got := job.Labels["foreman.llmkube.dev/task-namespace"]; got != taskNS {
		t.Errorf("gate Job task-namespace label = %q, want %q", got, taskNS)
	}
	// Pod template labels must match so a pod can also be traced back.
	if got := job.Spec.Template.Labels["foreman.llmkube.dev/task-name"]; got != taskName {
		t.Errorf("gate Job pod template task-name label = %q, want %q", got, taskName)
	}
}
