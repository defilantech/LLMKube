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
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// gateScheme is the scheme the fake controller-runtime client uses for
// every gate-tool test. batchv1 is the only API the tool touches.
func gateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("add batchv1 scheme: %v", err)
	}
	return s
}

// pinName returns a deterministic NameFn so polling can look the Job
// up without having to scan.
func pinName(name string) func(string) string {
	return func(string) string { return name }
}

// argsJSON builds a runGateJobArgs payload mirroring what
// executor_native.go's buildDeterministicArgs produces for the gate
// Agent. The checks field is omitted so the tool falls through to
// DefaultGateChecks; renderGateJob is tested separately for explicit
// check lists.
func argsJSON(t *testing.T, repo, branch string) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"repo":    repo,
		"branch":  branch,
		"taskRef": map[string]string{"namespace": "default", "name": "gate"},
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return out
}

// flipStatusOnce watches for the Job to appear, then patches the named
// status field to the requested count and returns. Tests use it to
// drive the fake apiserver through a Succeeded / Failed transition
// shortly after the tool's Create call.
func flipStatusOnce(ctx context.Context, c client.Client, key types.NamespacedName, succeeded, failed int32) {
	for ctx.Err() == nil {
		var job batchv1.Job
		if err := c.Get(ctx, key, &job); err == nil {
			job.Status.Succeeded = succeeded
			job.Status.Failed = failed
			_ = c.Status().Update(ctx, &job)
			return
		}
		time.Sleep(1 * time.Millisecond)
	}
}

// --- Schema / Name --------------------------------------------------------

func TestRunGateJob_NameAndSchema(t *testing.T) {
	tool := RunGateJobTool{}
	if got := tool.Name(); got != "run_gate_job" {
		t.Errorf("Name(): got %q", got)
	}
	schema := tool.Schema()
	if schema.Name != "run_gate_job" {
		t.Errorf("Schema.Name: got %q", schema.Name)
	}
	if !strings.Contains(string(schema.Parameters), "repo") ||
		!strings.Contains(string(schema.Parameters), "branch") {
		t.Errorf("Schema.Parameters missing repo/branch keys: %s", schema.Parameters)
	}
}

// --- Argument validation --------------------------------------------------

func TestRunGateJob_RequiresClient(t *testing.T) {
	tool := &RunGateJobTool{}
	_, err := tool.Execute(context.Background(), argsJSON(t, "x/y", "b"))
	if err == nil || !strings.Contains(err.Error(), "Client") {
		t.Errorf("expected Client-required error; got %v", err)
	}
}

func TestRunGateJob_MissingRepo(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Errorf("expected repo-required error; got %v", err)
	}
}

func TestRunGateJob_MissingBranch(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"x/y"}`))
	if err == nil || !strings.Contains(err.Error(), "branch") {
		t.Errorf("expected branch-required error; got %v", err)
	}
}

func TestRunGateJob_BadArgsJSON(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{not-json`))
	if err == nil || !strings.Contains(err.Error(), "bad args") {
		t.Errorf("expected bad-args error; got %v", err)
	}
}

// --- Submit + poll happy paths -------------------------------------------

func TestRunGateJob_SucceededProducesGATEPASS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-gate-fake-pass"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}

	go flipStatusOnce(ctx, c, key, 1, 0)

	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn: func(_ context.Context, _, name string) string {
				return "=== make test ===\nok  github.com/x/y\nGATE PASS\n"
			},
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "defilantech/LLMKube", "foreman/issue-503"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Terminal {
		t.Errorf("Terminal: want true")
	}
	if res.Verdict != VerdictGatePass {
		t.Errorf("Verdict: want %s got %s", VerdictGatePass, res.Verdict)
	}
	if !strings.Contains(res.Summary, "passed") {
		t.Errorf("Summary should announce pass; got %q", res.Summary)
	}
	if got, _ := res.Extra["logTail"].(string); !strings.Contains(got, "GATE PASS") {
		t.Errorf("logTail should carry the gate output; got %q", got)
	}
	if got, _ := res.Extra["jobName"].(string); got != jobName {
		t.Errorf("Extra.jobName: want %s got %s", jobName, got)
	}

	// And the Job actually got created at the apiserver with the
	// expected name + namespace.
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist on apiserver: %v", err)
	}
	if job.Labels["app.kubernetes.io/name"] != "foreman-gate" {
		t.Errorf("missing canonical label: %#v", job.Labels)
	}
}

func TestRunGateJob_FailedProducesGATEFAIL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-gate-fake-fail"
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}

	go flipStatusOnce(ctx, c, key, 0, 1)

	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  2 * time.Second,
			LogTailFn: func(_ context.Context, _, _ string) string {
				return "FAIL  github.com/x/y\nGATE FAIL\n"
			},
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "x/y", "b"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateFail {
		t.Errorf("Verdict: want %s got %s", VerdictGateFail, res.Verdict)
	}
	if got, _ := res.Extra["logTail"].(string); !strings.Contains(got, "GATE FAIL") {
		t.Errorf("logTail should carry failure marker; got %q", got)
	}
}

// --- Error paths ----------------------------------------------------------

func TestRunGateJob_PollTimeoutProducesGATEERROR(t *testing.T) {
	// No goroutine to flip status -- the poll loop should hit PollTimeout
	// and return GATE-ERROR with a "poll timeout" reason.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName("foreman-gate-stuck"),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  50 * time.Millisecond,
			LogTailFn:    func(context.Context, string, string) string { return "(no logs)" },
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "x/y", "b"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateError {
		t.Errorf("Verdict: want %s got %s", VerdictGateError, res.Verdict)
	}
	if got, _ := res.Extra["pollError"].(string); got == "" {
		t.Errorf("pollError should be set on timeout; got empty")
	}
}

func TestRunGateJob_DuplicateCreateProducesGATEERROR(t *testing.T) {
	// Seed the fake apiserver with a Job at the name we will pin. The
	// Create call inside Execute should fail with AlreadyExists; the
	// tool maps that to a GATE-ERROR terminal result rather than a
	// Go-level error.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	seeded := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "foreman-gate-collide", Namespace: "foreman-system"},
	}
	c := fake.NewClientBuilder().
		WithScheme(gateScheme(t)).
		WithObjects(seeded).
		Build()

	tool := &RunGateJobTool{
		Client: c,
		Cfg: RunGateJobToolConfig{
			NameFn:       pinName("foreman-gate-collide"),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  500 * time.Millisecond,
		},
	}

	res, err := tool.Execute(ctx, argsJSON(t, "x/y", "b"))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictGateError {
		t.Errorf("Verdict: want %s got %s", VerdictGateError, res.Verdict)
	}
	if got, _ := res.Extra["reason"].(string); !strings.Contains(got, "create job") {
		t.Errorf("reason should mention create failure; got %q", got)
	}
	// Sanity check the AlreadyExists shape on the underlying client so
	// we are not relying on a brittle string comparison.
	if err := c.Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "foreman-gate-collide", Namespace: "foreman-system"},
	}); !apierrors.IsAlreadyExists(err) {
		t.Errorf("expected AlreadyExists; got %v", err)
	}
}

// --- Defaults + sanitization ---------------------------------------------

func TestApplyConfigDefaults_FillsEveryField(t *testing.T) {
	c := applyConfigDefaults(RunGateJobToolConfig{})
	if c.Namespace == "" || c.PVCName == "" || c.Image == "" || c.CloneURLBase == "" {
		t.Errorf("string defaults missing: %#v", c)
	}
	if c.ActiveDeadlineSeconds == 0 || c.TTLSecondsAfterFinished == 0 {
		t.Errorf("deadline defaults missing: %#v", c)
	}
	if c.PollInterval == 0 || c.PollTimeout == 0 {
		t.Errorf("poll defaults missing: %#v", c)
	}
	if c.NameFn == nil {
		t.Errorf("NameFn default missing")
	}

	// PollTimeout defaults to 2 * ActiveDeadlineSeconds so the Job's
	// own deadline always fires before ours.
	want := 2 * time.Duration(c.ActiveDeadlineSeconds) * time.Second
	if c.PollTimeout != want {
		t.Errorf("PollTimeout default: want %s got %s", want, c.PollTimeout)
	}
}

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"issue-503-foo": "issue-503-foo",
		"Issue 503 Foo": "issue-503-foo",
		"weird/path!!":  "weird-path",
		"":              "task",
		"--leading--":   "leading",
		"BIG_CAPS_NAME": "big-caps-name",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q): want %q got %q", in, want, got)
		}
	}
}

// --- Template rendering ---------------------------------------------------

// renderGateArgsForTest renders a gate Job from the subset of rendererInput
// fields the bite-check tests care about, filling in the same boilerplate
// every other render test in this file repeats verbatim (image, PVC,
// resource sizing, task labels). Returns the joined container Args string.
// Recognized keys: repo, branch, baseBranch, upstreamURL (all string),
// biteCheck (bool), checks ([]string).
func renderGateArgsForTest(t *testing.T, in map[string]any) string {
	t.Helper()
	rin := rendererInput{
		Name:                    "foreman-gate-test",
		Namespace:               "foreman-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/x",
		Checks:                  []string{"fmt", "test"},
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-test",
	}
	if v, ok := in["repo"].(string); ok {
		rin.Repo = v
	}
	if v, ok := in["branch"].(string); ok {
		rin.Branch = v
	}
	if v, ok := in["baseBranch"].(string); ok {
		rin.BaseBranch = v
	}
	if v, ok := in["biteCheck"].(bool); ok {
		rin.BiteCheck = v
	}
	if v, ok := in["upstreamURL"].(string); ok {
		rin.UpstreamURL = v
	}
	if v, ok := in["checks"].([]string); ok {
		rin.Checks = v
	}
	job, err := renderGateJob(rin)
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	return strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
}

// TestGateJobBiteCheckUsesUpstreamMergeBase asserts that when upstreamURL is
// set, the bite check fetches BaseBranch from the canonical repo (not the
// fork) and diffs/reverts against the merge-base of HEAD and that ref, never
// against the fork's (possibly stale) base tip. Regression for #1259: a
// stale fork main manufactured compile failures in untouched packages and a
// false GATE-FAIL on a good branch.
func TestGateJobBiteCheckUsesUpstreamMergeBase(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "foreman/x", "biteCheck": true,
		"upstreamURL": "https://github.com/defilantech/LLMKube.git",
	})
	for _, want := range []string{
		`git fetch --depth 100 "$UPSTREAM_URL" "+refs/heads/$BASE:refs/remotes/ubase/$BASE"`,
		`git merge-base HEAD "refs/remotes/ubase/$BASE"`,
		`git fetch --deepen=1000 origin "$BRANCH"`,
		`git diff --name-only "$MB" HEAD`,
		`git checkout "$MB" -- "$f"`,
		`GATE-WARN: bite check skipped`,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("rendered gate args missing %q", want)
		}
	}
	// The fork-tip revert must be GONE on the upstream path.
	if strings.Contains(args, `git checkout "origin/$BASE" -- "$f"`) &&
		!strings.Contains(args, "UPSTREAM_URL") {
		t.Error("bite check still reverts to fork tip origin/$BASE")
	}
}

// TestGateJobBiteCheckFallsBackToOriginWithoutUpstreamURL asserts the
// pre-#1259 origin-fetch path survives for freeform tasks that carry no repo
// slug/upstream URL: the base ref is fetched from the fork itself.
func TestGateJobBiteCheckFallsBackToOriginWithoutUpstreamURL(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "foreman/x", "biteCheck": true,
	})
	if !strings.Contains(args, `git fetch --depth 1 origin "+refs/heads/$BASE:refs/remotes/origin/$BASE"`) {
		t.Error("origin fallback path missing when upstreamURL is empty")
	}
}

// TestGateJobBiteCheckSanityCap asserts the bite check skips (rather than
// fails) when the merge-base diff touches an implausible number of files,
// the signature of a bad base resolution rather than a real coder diff.
func TestGateJobBiteCheckSanityCap(t *testing.T) {
	args := renderGateArgsForTest(t, map[string]any{
		"repo": "defilantech/LLMKube", "branch": "foreman/x", "biteCheck": true,
		"upstreamURL": "https://github.com/defilantech/LLMKube.git",
	})
	if !strings.Contains(args, `-gt 200`) {
		t.Error("sanity cap (200 files) missing from bite block")
	}
}

func TestRenderGateJob_RendersChecksAndPVCMount(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "foreman-gate-x",
		Namespace:               "foreman-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/issue-503",
		Checks:                  []string{"fmt", "vet", "test"},
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-503",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	if job.Name != "foreman-gate-x" {
		t.Errorf("Name: %q", job.Name)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != "golang:1.26" {
		t.Errorf("Image: %q", got)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	if !strings.Contains(args, "make fmt") ||
		!strings.Contains(args, "make vet") ||
		!strings.Contains(args, "make test") {
		t.Errorf("Args missing one of the requested checks:\n%s", args)
	}
	if !strings.Contains(args, "defilantech/LLMKube") ||
		!strings.Contains(args, "foreman/issue-503") {
		t.Errorf("Args missing repo/branch substitution:\n%s", args)
	}
	if got := job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; got != "foreman-gate-cache" {
		t.Errorf("PVC claim name: %q", got)
	}
	if got := job.Labels["foreman.llmkube.dev/task-name"]; got != "gate-503" {
		t.Errorf("task-name label: %q", got)
	}
}

func TestRenderGateJob_GenericRunsCommandsNotMakeOrBiteCheck(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "foreman-gate-py",
		Namespace:               "foreman-system",
		Image:                   "python:3.13",
		Repo:                    "acme/widgets",
		Branch:                  "foreman/issue-1",
		BiteCheck:               true, // must be ignored on the generic path
		Generic:                 true,
		Commands:                []string{"ruff check .", "pytest -q"},
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-1",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != "python:3.13" {
		t.Errorf("Image: %q", got)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	if !strings.Contains(args, "( ruff check . )") || !strings.Contains(args, "( pytest -q )") {
		t.Errorf("generic args missing the resolved commands:\n%s", args)
	}
	if strings.Contains(args, "make ") {
		t.Errorf("generic path must not run make targets:\n%s", args)
	}
	if strings.Contains(args, "bite check") {
		t.Errorf("generic path must not run the Go-specific bite check:\n%s", args)
	}
}

// TestRenderGateJob_PodSecurityContextFSGroup asserts the rendered gate Job
// carries a PodSecurityContext with fsGroup=100 so non-root gate images
// (e.g. USER 65534) can write under XDG_DATA_HOME=/cache/xdg on the RWX
// gate-cache PVC. Regression for #1055.
func TestRenderGateJob_PodSecurityContextFSGroup(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "foreman-gate-fsgroup",
		Namespace:               "foreman-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/issue-1055",
		Checks:                  []string{"fmt"},
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-1055",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	psc := job.Spec.Template.Spec.SecurityContext
	if psc == nil {
		t.Fatalf("PodSecurityContext must be set on the gate pod")
	}
	if psc.FSGroup == nil {
		t.Fatalf("PodSecurityContext.FSGroup must be set for non-root gate images to write XDG_DATA_HOME on the RWX PVC")
	}
	if *psc.FSGroup != 100 {
		t.Errorf("PodSecurityContext.FSGroup: want 100 got %d", *psc.FSGroup)
	}
}

// TestRenderGateJob_CloneURLOverride asserts the template branches
// correctly on the CloneURL field: when set, the git clone target is
// the override URL verbatim; when empty, the historical
// CloneURLBase + Repo + .git is preserved. The human-readable log
// line ("=== clone <repo> @ <branch> ===") always names Repo so an
// operator sees what was being verified regardless of where the
// branch physically lives. Regression for #528 part 2.
func TestRenderGateJob_CloneURLOverride(t *testing.T) {
	cases := []struct {
		name        string
		cloneURL    string
		mustHave    []string
		mustNotHave []string
	}{
		{
			name:     "override wins",
			cloneURL: "https://github.com/Defilan/LLMKube.git",
			mustHave: []string{
				`"https://github.com/Defilan/LLMKube.git"`,
				"=== clone defilantech/LLMKube @ foreman/issue-510 ===",
			},
			mustNotHave: []string{
				`"https://github.com/defilantech/LLMKube.git"`,
			},
		},
		{
			name:     "empty falls back to CloneURLBase + Repo",
			cloneURL: "",
			mustHave: []string{
				`"https://github.com/defilantech/LLMKube.git"`,
				"=== clone defilantech/LLMKube @ foreman/issue-510 ===",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job, err := renderGateJob(rendererInput{
				Name:                    "foreman-gate-x",
				Namespace:               "foreman-system",
				Image:                   "golang:1.26",
				Repo:                    "defilantech/LLMKube",
				Branch:                  "foreman/issue-510",
				Checks:                  []string{"fmt"},
				PVCName:                 "foreman-gate-cache",
				ActiveDeadlineSeconds:   1800,
				TTLSecondsAfterFinished: 86400,
				CPURequest:              "2",
				CPULimit:                "4",
				MemRequest:              "4Gi",
				MemLimit:                "8Gi",
				CloneURLBase:            "https://github.com",
				CloneURL:                tc.cloneURL,
				TaskNamespace:           "default",
				TaskName:                "gate-510",
			})
			if err != nil {
				t.Fatalf("renderGateJob: %v", err)
			}
			args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
			for _, sub := range tc.mustHave {
				if !strings.Contains(args, sub) {
					t.Errorf("missing %q in rendered args:\n%s", sub, args)
				}
			}
			for _, sub := range tc.mustNotHave {
				if strings.Contains(args, sub) {
					t.Errorf("unwanted %q present in rendered args:\n%s", sub, args)
				}
			}
		})
	}
}

// TestRenderGateJob_BiteCheck_Enabled asserts the bite check phase is
// rendered when BiteCheck is true, and that it contains the key logic
// for detecting test files, reverting production, and checking test
// results. Regression for #799.
func TestRenderGateJob_BiteCheck_Enabled(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "foreman-gate-bite",
		Namespace:               "foreman-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/issue-799",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               true,
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// The bite check phase must be present.
	if !strings.Contains(args, "bite check") {
		t.Errorf("bite check phase missing from rendered args:\\n%s", args)
	}
	// Must detect changes by committed diff against the resolved merge-base
	// (#1259), NOT by git status: the gate Job clones a committed branch, so
	// the working tree is clean and git status would always be empty (the
	// round-3 fix).
	if !strings.Contains(args, `git diff --name-only "$MB" HEAD`) {
		t.Errorf("bite check must detect changes via committed diff against the merge-base:\\n%s", args)
	}
	if strings.Contains(args, "git status --porcelain") {
		t.Errorf("bite check must NOT use git status (clean tree on a committed clone):\\n%s", args)
	}
	// Must detect test files among the diff.
	if !strings.Contains(args, "_test\\.go") {
		t.Errorf("bite check must detect test files:\\n%s", args)
	}
	// Must revert ONLY production files to their merge-base version, keeping
	// the new test changes so they run against pre-change production.
	if !strings.Contains(args, `git checkout "$MB" -- "$f"`) {
		t.Errorf("bite check must revert production files to the merge-base version:\\n%s", args)
	}
	// Must NOT use the removed stash mechanism (no-op on a clean committed tree).
	if strings.Contains(args, "git stash") {
		t.Errorf("bite check must not use git stash (the round-3 fix removed it):\\n%s", args)
	}
	// Must restore branch state after the check.
	if !strings.Contains(args, "git checkout HEAD -- $prod_files") {
		t.Errorf("bite check must restore branch state via git checkout HEAD:\\n%s", args)
	}
	// Must run ONLY the changed test packages, never the whole module: a
	// module-wide `go test ./...` fails on unrelated packages (controller
	// envtest needs KUBEBUILDER_ASSETS, absent in the gate image) and the bite
	// check would misread that as biting -> false pass.
	if !strings.Contains(args, "test_pkgs=$(echo \"$test_files\" | xargs -n1 dirname | sort -u") {
		t.Errorf("bite check must scope tests to the changed packages:\\n%s", args)
	}
	if strings.Contains(args, "go test -count=1 -timeout=180s ./...") {
		t.Errorf("bite check must NOT run the whole module (./...); unrelated failures mask the signal:\\n%s", args)
	}
	// Infra anomalies must surface as a loud GATE-WARN skip, never a
	// GATE-ERROR/GATE-FAIL: only a genuine non-biting result blocks the gate
	// (#1259).
	if !strings.Contains(args, "GATE-WARN: bite check skipped") {
		t.Errorf("bite check must surface infra anomalies as GATE-WARN skips:\\n%s", args)
	}
	if strings.Contains(args, "GATE-ERROR") {
		t.Errorf("bite check must not produce GATE-ERROR; infra anomalies are a GATE-WARN skip (#1259):\\n%s", args)
	}
	// Must report non-biting tests as GATE FAIL.
	if !strings.Contains(args, "non-biting") {
		t.Errorf("bite check must report non-biting tests:\\n%s", args)
	}
}

// TestRenderGateJob_BiteCheck_Disabled asserts the bite check phase is
// absent when BiteCheck is false (the default). Regression for #799.
func TestRenderGateJob_BiteCheck_Disabled(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "foreman-gate-no-bite",
		Namespace:               "foreman-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/issue-799",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               false,
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// The bite check phase must NOT be present.
	if strings.Contains(args, "bite check") {
		t.Errorf("bite check phase should not be present when BiteCheck is false:\\n%s", args)
	}
}

// TestRenderGateJob_BiteCheck_SkipsWhenNoTestFiles asserts the bite check
// phase includes logic to skip when no test files changed, preventing
// false rejections of PRs that add/modify no test files. Regression for #799.
func TestRenderGateJob_BiteCheck_SkipsWhenNoTestFiles(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "foreman-gate-skip",
		Namespace:               "foreman-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/issue-799",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               true,
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// Must have the skip logic for no test files.
	if !strings.Contains(args, "no test files changed, skipping") {
		t.Errorf("bite check must skip when no test files changed:\\n%s", args)
	}
}

// TestRenderGateJob_BiteCheck_SkipsWhenNoProdFiles asserts the bite check
// phase includes logic to skip when no production files changed, preventing
// false rejections of PRs that only add tests. Regression for #799.
func TestRenderGateJob_BiteCheck_SkipsWhenNoProdFiles(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "foreman-gate-skip-prod",
		Namespace:               "foreman-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/issue-799",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               true,
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// Must have the skip logic for no production files.
	if !strings.Contains(args, "no production files changed, skipping") {
		t.Errorf("bite check must skip when no production files changed:\\n%s", args)
	}
}

// TestRenderGateJob_BiteCheck_FetchesBaseRef asserts the origin-fallback path
// (no upstreamURL) fetches the base ref with an explicit refspec (the clone
// is shallow + single-branch, so a plain fetch would not create
// origin/$BASE) and skips loudly (GATE-WARN, #1259), rather than failing the
// gate, when the base ref cannot be established. Regression for the round-3
// fix.
func TestRenderGateJob_BiteCheck_FetchesBaseRef(t *testing.T) {
	job, err := renderGateJob(rendererInput{
		Name:                    "foreman-gate-base",
		Namespace:               "foreman-system",
		Image:                   "golang:1.26",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/issue-799",
		BaseBranch:              "main",
		Checks:                  []string{"fmt", "test"},
		BiteCheck:               true,
		PVCName:                 "foreman-gate-cache",
		ActiveDeadlineSeconds:   1800,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "gate-799",
	})
	if err != nil {
		t.Fatalf("renderGateJob: %v", err)
	}
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	// Must fetch the base ref with an explicit refspec so origin/$BASE resolves.
	if !strings.Contains(args, `git fetch --depth 1 origin "+refs/heads/$BASE:refs/remotes/origin/$BASE"`) {
		t.Errorf("bite check must fetch base with an explicit refspec:\\n%s", args)
	}
	// Must verify the ref resolved and skip loudly otherwise.
	if !strings.Contains(args, `git rev-parse --verify --quiet "origin/$BASE"`) {
		t.Errorf("bite check must verify the base ref resolved:\\n%s", args)
	}
	if !strings.Contains(args, "could not fetch base ref") {
		t.Errorf("bite check must skip loudly when the base ref cannot be fetched:\\n%s", args)
	}
}

// TestRenderGateJob_BaseBranchEnv asserts the BASE_BRANCH env var is rendered
// from the BaseBranch field and that an empty BaseBranch defaults to main.
func TestRenderGateJob_BaseBranchEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"explicit", "release-1.0", "release-1.0"},
		{"defaults to main", "", "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := renderGateJob(rendererInput{
				Name:                    "foreman-gate-baseenv",
				Namespace:               "foreman-system",
				Image:                   "golang:1.26",
				Repo:                    "defilantech/LLMKube",
				Branch:                  "foreman/issue-799",
				BaseBranch:              tc.in,
				Checks:                  []string{"fmt", "test"},
				BiteCheck:               true,
				PVCName:                 "foreman-gate-cache",
				ActiveDeadlineSeconds:   1800,
				TTLSecondsAfterFinished: 86400,
				CPURequest:              "2",
				CPULimit:                "4",
				MemRequest:              "4Gi",
				MemLimit:                "8Gi",
				CloneURLBase:            "https://github.com",
				TaskNamespace:           "default",
				TaskName:                "gate-799",
			})
			if err != nil {
				t.Fatalf("renderGateJob: %v", err)
			}
			var got string
			found := false
			for _, e := range job.Spec.Template.Spec.Containers[0].Env {
				if e.Name == "BASE_BRANCH" {
					got = e.Value
					found = true
				}
			}
			if !found {
				t.Fatalf("BASE_BRANCH env var not rendered")
			}
			if got != tc.want {
				t.Errorf("BASE_BRANCH = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunGateJob_ImageOverride asserts that a non-empty per-call image
// in the args overrides cfg.Image in the rendered Job, and that an
// empty per-call image falls back to cfg.Image. Regression for #839.
func TestRunGateJob_ImageOverride(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cases := []struct {
		name    string
		argsImg string
		cfgImg  string
		wantImg string
	}{
		{
			name:    "args image overrides cfg image",
			argsImg: "python:3.13",
			cfgImg:  "golang:1.26",
			wantImg: "python:3.13",
		},
		{
			name:    "empty args image falls back to cfg image",
			argsImg: "",
			cfgImg:  "golang:1.26",
			wantImg: "golang:1.26",
		},
		{
			name:    "empty args image falls back to cfg custom image",
			argsImg: "",
			cfgImg:  "rust:1",
			wantImg: "rust:1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
			jobName := "foreman-gate-img-" + tc.name
			key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}

			go flipStatusOnce(ctx, c, key, 1, 0)

			args := map[string]any{
				"repo":   "defilantech/LLMKube",
				"branch": "foreman/issue-839",
				"image":  tc.argsImg,
				"taskRef": map[string]string{
					"namespace": "default",
					"name":      "gate-839",
				},
			}
			argsJSON, err := json.Marshal(args)
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}

			tool := &RunGateJobTool{
				Client: c,
				Cfg: RunGateJobToolConfig{
					Image:        tc.cfgImg,
					NameFn:       pinName(jobName),
					PollInterval: 5 * time.Millisecond,
					PollTimeout:  2 * time.Second,
				},
			}

			res, err := tool.Execute(ctx, argsJSON)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Verdict != VerdictGatePass {
				t.Fatalf("Verdict: want %s got %s", VerdictGatePass, res.Verdict)
			}

			// Verify the Job was created with the expected image.
			var job batchv1.Job
			if err := c.Get(ctx, key, &job); err != nil {
				t.Fatalf("Job should exist: %v", err)
			}
			if got := job.Spec.Template.Spec.Containers[0].Image; got != tc.wantImg {
				t.Errorf("Image: want %q got %q", tc.wantImg, got)
			}
		})
	}
}

func TestGateJobNamePreservesUniquenessSuffixWhenTruncated(t *testing.T) {
	// A long task name whose "foreman-gate-<task>" already exceeds the 63-char
	// k8s object-name limit. The #768 validation caught a retry's gate Job
	// colliding with the prior attempt's because the trailing unix-ms
	// disambiguator was truncated away, so the retry could not re-gate and a
	// failing branch landed as GO. The suffix must survive truncation.
	long := "validate-1110-envtestloop-validate-1110-envtestloop"
	n1 := gateJobName(long, 1784157000000)
	n2 := gateJobName(long, 1784157000001)

	if len(n1) > 63 {
		t.Fatalf("name exceeds the 63-char k8s limit: len=%d %q", len(n1), n1)
	}
	if !strings.HasPrefix(n1, "foreman-gate-") {
		t.Fatalf("lost the foreman-gate- prefix: %q", n1)
	}
	if !strings.HasSuffix(n1, "-1784157000000") {
		t.Fatalf("uniqueness suffix was truncated away: %q", n1)
	}
	if n1 == n2 {
		t.Fatalf("two submissions of the same long task name collide: %q", n1)
	}
}
