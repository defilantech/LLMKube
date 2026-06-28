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

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	"github.com/defilantech/llmkube/pkg/foreman/agent/githubissue"
)

// Short aliases keep the table-driven cases under the line-length limit.
type (
	aPhase   = foremanv1alpha1.AgenticTaskPhase
	aVerdict = foremanv1alpha1.AgenticTaskVerdict
)

const (
	phSucceeded = foremanv1alpha1.AgenticTaskPhaseSucceeded
	phFailed    = foremanv1alpha1.AgenticTaskPhaseFailed
	phRunning   = foremanv1alpha1.AgenticTaskPhaseRunning
	vGo         = foremanv1alpha1.AgenticTaskVerdictGo
	vGatePass   = foremanv1alpha1.AgenticTaskVerdictGatePass
	vNoGo       = foremanv1alpha1.AgenticTaskVerdictNoGo
	vIncomplete = foremanv1alpha1.AgenticTaskVerdictIncomplete
)

func TestParseIssues(t *testing.T) {
	t.Run("valid, ordered, deduped", func(t *testing.T) {
		got, err := parseIssues([]string{"813", "854", "813", "89"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []int{813, 854, 89}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	})
	for _, bad := range []string{"0", "-3", "abc", "12x"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := parseIssues([]string{bad}); err == nil {
				t.Fatalf("expected error for %q", bad)
			}
		})
	}
	t.Run("empty", func(t *testing.T) {
		if _, err := parseIssues(nil); err == nil {
			t.Fatal("expected error for empty input")
		}
	})
}

func TestAssignAgents(t *testing.T) {
	got := assignAgents([]int{813, 854, 89, 90}, []string{"amd", "metal"})
	want := []taskAssignment{
		{813, "amd"}, {854, "metal"}, {89, "amd"}, {90, "metal"},
	}
	if len(got) != len(want) {
		t.Fatalf("len got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d got %+v want %+v", i, got[i], want[i])
		}
	}

	// Single agent: every issue lands on it.
	single := assignAgents([]int{1, 2, 3}, []string{"only"})
	for _, a := range single {
		if a.Agent != "only" {
			t.Fatalf("single-agent assignment got %q", a.Agent)
		}
	}
}

func TestParsePromptOverrides(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "813.md")
	if err := os.WriteFile(p, []byte("custom body"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := parsePromptOverrides([]string{"813=" + p})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got[813] != "custom body" {
		t.Fatalf("got %q", got[813])
	}

	for _, bad := range []string{"no-equals", "x=" + p, "813=/does/not/exist"} {
		if _, err := parsePromptOverrides([]string{bad}); err == nil {
			t.Fatalf("expected error for %q", bad)
		}
	}
}

func TestFormatIssuePrompt(t *testing.T) {
	got := formatIssuePrompt(&githubissue.Issue{Title: "Fix the thing", Body: "details here"})
	if !strings.Contains(got, "# Fix the thing") || !strings.Contains(got, "details here") {
		t.Fatalf("got %q", got)
	}
	empty := formatIssuePrompt(&githubissue.Issue{Title: "T", Body: "  "})
	if !strings.Contains(empty, "(no description provided)") {
		t.Fatalf("empty body not handled: %q", empty)
	}
}

func TestBuildTask(t *testing.T) {
	opts := &dispatchOptions{timeoutSecs: 1800, baseBranch: "main", branchPrefix: "foreman/"}
	task := buildTask("default", "20260628-120000", "defilantech/LLMKube",
		taskAssignment{Issue: 813, Agent: "coder-amd"}, "the prompt", opts)

	if task.Name != "dispatch-813-20260628-120000" {
		t.Fatalf("name: %s", task.Name)
	}
	if task.Labels[dispatchRunLabel] != "20260628-120000" ||
		task.Labels[dispatchIssueLabel] != "813" ||
		task.Labels[dispatchAgentLabel] != "coder-amd" {
		t.Fatalf("labels: %v", task.Labels)
	}
	if task.Spec.Kind != foremanv1alpha1.AgenticTaskKindIssueFix {
		t.Fatalf("kind: %s", task.Spec.Kind)
	}
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "coder-amd" {
		t.Fatalf("agentRef: %+v", task.Spec.AgentRef)
	}
	if task.Spec.TimeoutSeconds != 1800 {
		t.Fatalf("timeout: %d", task.Spec.TimeoutSeconds)
	}
	p := task.Spec.Payload
	if p.Repo != "defilantech/LLMKube" || p.Issue != 813 || p.Prompt != "the prompt" ||
		p.BaseBranch != "main" || p.BranchPrefix != "foreman/" || p.Agent != "coder-amd" {
		t.Fatalf("payload: %+v", p)
	}
}

func TestTaskSucceededAndExitErr(t *testing.T) {
	mk := func(phase aPhase, v aVerdict) *foremanv1alpha1.AgenticTask {
		return &foremanv1alpha1.AgenticTask{Status: foremanv1alpha1.AgenticTaskStatus{Phase: phase, Verdict: v}}
	}
	cases := []struct {
		name   string
		task   *foremanv1alpha1.AgenticTask
		wantOK bool
	}{
		{"succeeded GO", mk(phSucceeded, vGo), true},
		{"succeeded GATE-PASS", mk(phSucceeded, vGatePass), true},
		{"succeeded NO-GO", mk(phSucceeded, vNoGo), false},
		{"succeeded INCOMPLETE", mk(phSucceeded, vIncomplete), false},
		{"failed", mk(phFailed, ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskSucceeded(tc.task); got != tc.wantOK {
				t.Fatalf("taskSucceeded got %v want %v", got, tc.wantOK)
			}
		})
	}

	// All-good batch -> nil; any bad -> error naming the issue.
	good := []taskResult{
		{Issue: 1, Phase: phSucceeded, Verdict: vGo},
		{Issue: 2, Phase: phSucceeded, Verdict: vGatePass},
	}
	if err := exitErrFromResults(good); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	mixed := []taskResult{
		{Issue: 1, Phase: phSucceeded, Verdict: vGo},
		{Issue: 3, Phase: phSucceeded, Verdict: vNoGo},
	}
	err := exitErrFromResults(mixed)
	if err == nil || !strings.Contains(err.Error(), "#3") {
		t.Fatalf("expected error naming #3, got %v", err)
	}
}

func TestRenderSummary(t *testing.T) {
	var sb strings.Builder
	renderSummary(&sb, []taskResult{
		{
			Issue: 854, Agent: "metal", Node: "m5",
			Phase: phSucceeded, Verdict: vGo,
			Branch: "foreman/issue-854", Duration: 90 * time.Second,
		},
		{
			Issue: 813, Agent: "amd", Node: "incluster", Phase: phFailed,
			Reason: foremanv1alpha1.AgenticTaskFailureReason("Timeout"),
		},
	})
	out := sb.String()
	// Sorted: #813 before #854.
	if strings.Index(out, "#813") > strings.Index(out, "#854") {
		t.Fatalf("rows not sorted by issue:\n%s", out)
	}
	for _, want := range []string{"ISSUE", "VERDICT", "#813", "#854", "foreman/issue-854", "Timeout", "1m30s"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}
}

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := foremanv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&foremanv1alpha1.AgenticTask{}).
		Build()
}

func terminalTask(name string, issue int32, phase aPhase, v aVerdict) *foremanv1alpha1.AgenticTask {
	start := metav1.NewTime(time.Unix(1_700_000_000, 0))
	end := metav1.NewTime(time.Unix(1_700_000_090, 0))
	return &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: foremanv1alpha1.AgenticTaskSpec{
			Payload: foremanv1alpha1.AgenticTaskPayload{Issue: issue},
		},
		Status: foremanv1alpha1.AgenticTaskStatus{
			Phase: phase, Verdict: v, AssignedNode: "node-x",
			StartedAt: &start, FinishedAt: &end,
		},
	}
}

func TestWatchTasks_AllTerminalReturnsResults(t *testing.T) {
	t1 := terminalTask("dispatch-813-x", 813, phSucceeded, vGo)
	t2 := terminalTask("dispatch-854-x", 854, phFailed, "")
	c := newFakeClient(t, t1, t2)

	var sb strings.Builder
	results, err := watchTasks(context.Background(), c, &sb, "default",
		[]*foremanv1alpha1.AgenticTask{t1, t2}, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	byIssue := map[int]taskResult{}
	for _, r := range results {
		byIssue[r.Issue] = r
	}
	if byIssue[813].Verdict != vGo || byIssue[813].Duration != 90*time.Second {
		t.Fatalf("813 result wrong: %+v", byIssue[813])
	}
	if byIssue[854].Phase != phFailed {
		t.Fatalf("854 result wrong: %+v", byIssue[854])
	}
}

func TestWatchTasks_ContextCancelledReturnsPartial(t *testing.T) {
	pending := &foremanv1alpha1.AgenticTask{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch-1-x", Namespace: "default"},
		Spec:       foremanv1alpha1.AgenticTaskSpec{Payload: foremanv1alpha1.AgenticTaskPayload{Issue: 1}},
		Status:     foremanv1alpha1.AgenticTaskStatus{Phase: phRunning},
	}
	c := newFakeClient(t, pending)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the first poll is non-terminal, then ctx.Done fires.

	var sb strings.Builder
	results, err := watchTasks(ctx, c, &sb, "default",
		[]*foremanv1alpha1.AgenticTask{pending}, time.Hour)
	if err == nil {
		t.Fatal("expected a context error")
	}
	if len(results) != 1 || results[0].Phase != phRunning {
		t.Fatalf("expected partial running result, got %+v", results)
	}
}

func TestPrintTasksYAML(t *testing.T) {
	opts := &dispatchOptions{timeoutSecs: 1800}
	task := buildTask("default", "run1", "defilantech/LLMKube",
		taskAssignment{Issue: 813, Agent: "coder-amd"}, "prompt", opts)
	var sb strings.Builder
	if err := printTasksYAML(&sb, []*foremanv1alpha1.AgenticTask{task}); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{"apiVersion: foreman.llmkube.dev", "kind: AgenticTask", "issue: 813", "coder-amd"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run YAML missing %q:\n%s", want, out)
		}
	}
}
