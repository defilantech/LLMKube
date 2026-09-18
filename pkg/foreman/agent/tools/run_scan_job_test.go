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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/defilantech/llmkube/pkg/foreman/agent"
)

// The scan-tool tests reuse the gate harness verbatim where it fits:
// gateScheme, pinName, flipStatusOnce and flipDeadlineOnce from
// run_gate_job_test.go, and fakeCodeHost from seam_bypass_test.go.

// scanTestJobName derives a deterministic, DNS-1123-safe, 63-char-safe Job
// name from the test name so polling can resolve it without listing.
func scanTestJobName(t *testing.T) string {
	t.Helper()
	base := strings.TrimRight(sanitizeName(t.Name()), "-")
	const maxTaskLen = 40
	if len(base) > maxTaskLen {
		base = strings.TrimRight(base[:maxTaskLen], "-")
	}
	return "foreman-scan-" + base
}

// runScanJobRun submits a scan Job through Execute against a fake
// apiserver and drives the Job to a terminal status (succeeded/failed
// counts, or a DeadlineExceeded kill when deadline is true). cfgTweak may
// be nil. Returns the tool result and the created Job.
func runScanJobRun(
	t *testing.T, succeeded, failed int32, deadline bool,
	cfgTweak func(*RunScanJobToolConfig), args map[string]any,
) (*agent.ToolResult, *batchv1.Job) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := scanTestJobName(t)
	key := types.NamespacedName{Namespace: "foreman-system", Name: jobName}

	if deadline {
		go flipDeadlineOnce(ctx, c, key)
	} else {
		go flipStatusOnce(ctx, c, key, succeeded, failed)
	}

	full := map[string]any{
		"repo":    "defilantech/LLMKube",
		"branch":  "foreman/issue-900",
		"taskRef": map[string]string{"namespace": "default", "name": "scan-900"},
	}
	for k, v := range args {
		full[k] = v
	}
	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	cfg := RunScanJobToolConfig{
		NameFn:       pinName(jobName),
		PollInterval: 5 * time.Millisecond,
		PollTimeout:  2 * time.Second,
		LogTailFn:    func(_ context.Context, _, _ string) string { return "SCAN PASS\n" },
	}
	if cfgTweak != nil {
		cfgTweak(&cfg)
	}

	tool := &RunScanJobTool{Client: c, Cfg: cfg}
	res, err := tool.Execute(ctx, raw)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var job batchv1.Job
	if err := c.Get(ctx, key, &job); err != nil {
		t.Fatalf("Job should exist on apiserver: %v", err)
	}
	return res, &job
}

// scanRenderJob renders a scan Job from the documented boilerplate (both
// images, no PVC, GitHub base, all built-in targets at the CI defaults),
// with an optional per-test tweak.
func scanRenderJob(t *testing.T, tweak func(*scanRendererInput)) *batchv1.Job {
	t.Helper()
	targets, err := resolveScanTargets(nil)
	if err != nil {
		t.Fatalf("resolveScanTargets(nil): %v", err)
	}
	in := scanRendererInput{
		Name:                    "foreman-scan-test",
		Namespace:               "foreman-system",
		BuilderImage:            "golang:1.26",
		RunnerImage:             "alpine:3.24",
		Repo:                    "defilantech/LLMKube",
		Branch:                  "foreman/issue-900",
		Targets:                 targets,
		Severity:                resolveScanSeverity(nil),
		IgnoreUnfixed:           DefaultScanIgnoreUnfixed,
		TrivyVersion:            TrivyVersion,
		ActiveDeadlineSeconds:   3600,
		TTLSecondsAfterFinished: 86400,
		CPURequest:              "2",
		CPULimit:                "4",
		MemRequest:              "4Gi",
		MemLimit:                "8Gi",
		CloneURLBase:            "https://github.com",
		TaskNamespace:           "default",
		TaskName:                "scan-test",
	}
	if tweak != nil {
		tweak(&in)
	}
	job, err := renderScanJob(in)
	if err != nil {
		t.Fatalf("renderScanJob: %v", err)
	}
	return job
}

// --- Schema / Name --------------------------------------------------------

func TestRunScanJob_NameAndSchema(t *testing.T) {
	tool := RunScanJobTool{}
	if got := tool.Name(); got != "run_scan_job" {
		t.Errorf("Name(): got %q", got)
	}
	schema := tool.Schema()
	if schema.Name != "run_scan_job" {
		t.Errorf("Schema.Name: got %q", schema.Name)
	}
	for _, key := range []string{"repo", "branch", "images", "severity", "ignoreUnfixed"} {
		if !strings.Contains(string(schema.Parameters), key) {
			t.Errorf("Schema.Parameters missing %q key: %s", key, schema.Parameters)
		}
	}
}

// --- Argument validation --------------------------------------------------

func TestRunScanJob_RequiresClient(t *testing.T) {
	tool := &RunScanJobTool{}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"x/y","branch":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "Client") {
		t.Errorf("expected Client-required error; got %v", err)
	}
}

func TestRunScanJob_MissingRepo(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunScanJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"branch":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "repo") {
		t.Errorf("expected repo-required error; got %v", err)
	}
}

func TestRunScanJob_MissingBranch(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunScanJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{"repo":"x/y"}`))
	if err == nil || !strings.Contains(err.Error(), "branch") {
		t.Errorf("expected branch-required error; got %v", err)
	}
}

func TestRunScanJob_BadArgsJSON(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).Build()
	tool := &RunScanJobTool{Client: c}
	_, err := tool.Execute(context.Background(), json.RawMessage(`{not-json`))
	if err == nil || !strings.Contains(err.Error(), "bad args") {
		t.Errorf("expected bad-args error; got %v", err)
	}
}

// TestRunScanJob_UnknownImageIDErrorsWithoutSubmit pins the contract that
// a bad image declaration is an argument error, never a SCAN-ERROR
// verdict: an infra verdict reads as could-not-run and the executor would
// retry a typo forever. Nothing may reach the apiserver.
func TestRunScanJob_UnknownImageIDErrorsWithoutSubmit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	tool := &RunScanJobTool{
		Client: c,
		Cfg: RunScanJobToolConfig{
			NameFn:       pinName("foreman-scan-unknown-img"),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  500 * time.Millisecond,
		},
	}

	args, err := json.Marshal(map[string]any{
		"repo":    "defilantech/LLMKube",
		"branch":  "foreman/issue-900",
		"images":  []string{"foreman-agent", "not-a-target"},
		"taskRef": map[string]string{"namespace": "default", "name": "scan-900"},
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	res, err := tool.Execute(ctx, args)
	if err == nil || !strings.Contains(err.Error(), "not-a-target") {
		t.Errorf("expected error naming the unknown image id; got res=%v err=%v", res, err)
	}
	if res != nil {
		t.Errorf("no ToolResult expected on an argument error; got %+v", res)
	}
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs); err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs.Items) != 0 {
		t.Errorf("unknown image id must not submit a Job; created %d", len(jobs.Items))
	}
}

func TestRunScanJob_UpstreamURLValidation(t *testing.T) {
	cases := []struct {
		name        string
		upstreamURL string
		wantErr     bool
	}{
		{"leading dash rejected", "--upload-pack=/tmp/evil", true},
		{"non-URL scheme rejected", "ext::sh -c evil", true},
		{"https accepted", "https://github.com/defilantech/LLMKube.git", false},
		{"git-at accepted", "git@github.com:defilantech/LLMKube.git", false},
		{"empty accepted", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
			jobName := scanTestJobName(t)
			go flipStatusOnce(ctx, c, types.NamespacedName{Namespace: "foreman-system", Name: jobName}, 1, 0)

			tool := &RunScanJobTool{
				Client: c,
				Cfg: RunScanJobToolConfig{
					NameFn:       pinName(jobName),
					PollInterval: 5 * time.Millisecond,
					PollTimeout:  2 * time.Second,
				},
			}
			raw, err := json.Marshal(map[string]any{
				"repo": "defilantech/LLMKube", "branch": "foreman/x",
				"upstreamURL": tc.upstreamURL,
				"taskRef":     map[string]string{"namespace": "default", "name": "scan"},
			})
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			_, err = tool.Execute(ctx, raw)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), tc.upstreamURL) {
					t.Errorf("want error naming the bad upstreamURL %q; got %v", tc.upstreamURL, err)
				}
				return
			}
			if err != nil {
				t.Errorf("valid upstreamURL %q rejected: %v", tc.upstreamURL, err)
			}
		})
	}
}

// --- Submit + poll verdict mapping ----------------------------------------

func TestRunScanJob_SucceededProducesSCANPASS(t *testing.T) {
	const upstream = "https://github.com/defilantech/LLMKube.git"
	res, job := runScanJobRun(t, 1, 0, false, nil, map[string]any{"upstreamURL": upstream})

	if !res.Terminal {
		t.Errorf("Terminal: want true")
	}
	if res.Verdict != VerdictScanPass {
		t.Errorf("Verdict: want %s got %s", VerdictScanPass, res.Verdict)
	}
	if got, _ := res.Extra["logTail"].(string); !strings.Contains(got, "SCAN PASS") {
		t.Errorf("logTail should carry the scan output; got %q", got)
	}
	if job.Labels["app.kubernetes.io/name"] != "foreman-scan" {
		t.Errorf("missing canonical label: %#v", job.Labels)
	}
	if job.Labels["foreman.llmkube.dev/task-name"] != "scan-900" {
		t.Errorf("task-name label: %#v", job.Labels)
	}

	if len(job.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("want exactly 1 initContainer, got %d", len(job.Spec.Template.Spec.InitContainers))
	}
	init := job.Spec.Template.Spec.InitContainers[0]
	if init.Name != "scan-build" {
		t.Errorf("initContainer name: %q", init.Name)
	}
	if init.Image != "golang:1.26" {
		t.Errorf("BuilderImage: %q", init.Image)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("want exactly 1 container, got %d", len(job.Spec.Template.Spec.Containers))
	}
	scan := job.Spec.Template.Spec.Containers[0]
	if scan.Name != "scan" {
		t.Errorf("container name: %q", scan.Name)
	}
	if scan.Image != "alpine:3.24" {
		t.Errorf("RunnerImage: %q", scan.Image)
	}

	// All four targets' build lines present in the builder, all four tars
	// assembled and scanned by the runner, at the CI defaults.
	initArgs := strings.Join(init.Args, "\n")
	scanArgs := strings.Join(scan.Args, "\n")
	for _, tgt := range builtInScanTargets {
		if !strings.Contains(initArgs, "-o \"/work/"+tgt.ID+"/linux/amd64/"+tgt.Binary+"\"") {
			t.Errorf("builder missing staged-binary line for %q:\n%s", tgt.ID, initArgs)
		}
		if !strings.Contains(scanArgs, `--input "/work/`+tgt.ID+`.tar"`) {
			t.Errorf("runner missing trivy input line for %q:\n%s", tgt.ID, scanArgs)
		}
	}
	if n := strings.Count(scanArgs, "buildah build --storage-driver=vfs --isolation=chroot "+
		"--platform linux/amd64"); n != 4 {
		t.Errorf("want 4 daemonless buildah lines, got %d:\n%s", n, scanArgs)
	}
	for _, flag := range []string{"--severity CRITICAL,HIGH", "--ignore-unfixed", "--exit-code 1 --format table"} {
		if !strings.Contains(scanArgs, flag) {
			t.Errorf("default trivy flags missing %q:\n%s", flag, scanArgs)
		}
	}

	// The clone ran against the default GitHub base, and upstreamURL
	// reached the Job's env.
	if !strings.Contains(initArgs, `"https://github.com/defilantech/LLMKube.git" /work/repo`) {
		t.Errorf("clone line missing default base URL:\n%s", initArgs)
	}
	var upstreamEnv string
	for _, e := range init.Env {
		if e.Name == "UPSTREAM_URL" {
			upstreamEnv = e.Value
		}
	}
	if upstreamEnv != upstream {
		t.Errorf("UPSTREAM_URL env: want %q got %q", upstream, upstreamEnv)
	}
}

func TestRunScanJob_FailedProducesSCANFAIL(t *testing.T) {
	res, _ := runScanJobRun(t, 0, 1, false, func(cfg *RunScanJobToolConfig) {
		cfg.LogTailFn = func(_ context.Context, _, _ string) string {
			return "=== trivy controller ===\nCVE-2024-0001  CRITICAL  openssl  fixed in 1.2.3\nSCAN FAIL\n"
		}
	}, nil)

	if res.Verdict != VerdictScanFail {
		t.Errorf("Verdict: want %s got %s", VerdictScanFail, res.Verdict)
	}
	got, _ := res.Extra["logTail"].(string)
	if !strings.Contains(got, "CVE-2024-0001") {
		t.Errorf("logTail should carry the finding table (the feedback surface); got %q", got)
	}
}

// TestRunScanJob_DeadlineExceededProducesSCANERROR applies #1748's rule
// to the scan gate: a Job killed by its activeDeadlineSeconds is an
// infrastructure problem (SCAN-ERROR / could-not-run), never a verdict on
// the branch's images.
func TestRunScanJob_DeadlineExceededProducesSCANERROR(t *testing.T) {
	res, _ := runScanJobRun(t, 0, 1, true, nil, nil)
	if res.Verdict != VerdictScanError {
		t.Errorf("Verdict: want %s got %s", VerdictScanError, res.Verdict)
	}
	if !strings.Contains(res.Summary, "deadline") {
		t.Errorf("Summary should name the deadline; got %q", res.Summary)
	}
}

func TestRunScanJob_PollTimeoutProducesSCANERROR(t *testing.T) {
	// No goroutine flips status -- the poll loop hits PollTimeout and
	// returns SCAN-ERROR with a "poll timeout" reason.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c := fake.NewClientBuilder().WithScheme(gateScheme(t)).WithStatusSubresource(&batchv1.Job{}).Build()
	jobName := "foreman-scan-stuck"
	tool := &RunScanJobTool{
		Client: c,
		Cfg: RunScanJobToolConfig{
			NameFn:       pinName(jobName),
			PollInterval: 5 * time.Millisecond,
			PollTimeout:  50 * time.Millisecond,
			LogTailFn:    func(context.Context, string, string) string { return "(no logs)" },
		},
	}

	raw, err := json.Marshal(map[string]any{
		"repo": "x/y", "branch": "b",
		"taskRef": map[string]string{"namespace": "default", "name": "scan"},
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	res, err := tool.Execute(ctx, raw)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Verdict != VerdictScanError {
		t.Errorf("Verdict: want %s got %s", VerdictScanError, res.Verdict)
	}
	if got, _ := res.Extra["pollError"].(string); got == "" {
		t.Errorf("pollError should be set on timeout; got empty")
	}
}

// --- Clone-URL precedence ---------------------------------------------------

// TestRunScanJob_CloneURLPrecedence pins the three-way contract shared
// with the gate (#1298): an explicit cloneURL argument wins, then the
// injected CodeHost seam, then the template's CloneURLBase + repo GitHub
// fallback.
func TestRunScanJob_CloneURLPrecedence(t *testing.T) {
	t.Run("explicit wins over seam", func(t *testing.T) {
		fake := &fakeCodeHost{}
		cfg := applyScanConfigDefaults(RunScanJobToolConfig{CodeHost: fake})
		got := resolveScanCloneURL(cfg, "group/sub/project", "https://explicit.example/x.git")
		if got != "https://explicit.example/x.git" {
			t.Errorf("explicit cloneURL was overridden: %q", got)
		}
		if len(fake.asked) != 0 {
			t.Errorf("seam consulted despite an explicit cloneURL: %v", fake.asked)
		}
	})

	t.Run("seam wins over base", func(t *testing.T) {
		fake := &fakeCodeHost{}
		cfg := applyScanConfigDefaults(RunScanJobToolConfig{CodeHost: fake})
		got := resolveScanCloneURL(cfg, "group/sub/project", "")
		want := "https://forge.example.org/group/sub/project.git"
		if got != want {
			t.Errorf("clone URL = %q, want %q (injected CodeHost was bypassed)", got, want)
		}
		if strings.Contains(got, "github.com") {
			t.Errorf("clone URL still points at github.com: %q", got)
		}
	})

	t.Run("no seam falls back to CloneURLBase", func(t *testing.T) {
		cfg := applyScanConfigDefaults(RunScanJobToolConfig{})
		if got := resolveScanCloneURL(cfg, "owner/name", ""); got != "" {
			t.Errorf("expected empty so the template uses CloneURLBase, got %q", got)
		}
	})
}

func TestRenderScanJob_CloneURLOverride(t *testing.T) {
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
				`"https://github.com/Defilan/LLMKube.git" /work/repo`,
				"=== clone defilantech/LLMKube @ foreman/issue-900 ===",
			},
			mustNotHave: []string{
				`"https://github.com/defilantech/LLMKube.git"`,
			},
		},
		{
			name:     "empty falls back to CloneURLBase + Repo",
			cloneURL: "",
			mustHave: []string{
				`"https://github.com/defilantech/LLMKube.git" /work/repo`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := scanRenderJob(t, func(in *scanRendererInput) {
				in.CloneURL = tc.cloneURL
			})
			args := strings.Join(job.Spec.Template.Spec.InitContainers[0].Args, "\n")
			for _, sub := range tc.mustHave {
				if !strings.Contains(args, sub) {
					t.Errorf("missing %q in rendered init args:\n%s", sub, args)
				}
			}
			for _, sub := range tc.mustNotHave {
				if strings.Contains(args, sub) {
					t.Errorf("unwanted %q present in rendered init args:\n%s", sub, args)
				}
			}
		})
	}
}

// TestScanJobTrivyInstallFetchesPinnedReleaseAsset pins the supply-chain
// shape of the runner's trivy install: the binary comes from the release
// asset of the exact TrivyVersion tag, never from a script executed off a
// mutable branch. A gate whose whole purpose is CVE hygiene must not pull
// its scanner through a moving-ref pipe-to-sh — the helm-tarball idiom the
// gate template already set (#1441).
func TestScanJobTrivyInstallFetchesPinnedReleaseAsset(t *testing.T) {
	job := scanRenderJob(t, func(*scanRendererInput) {})
	scanArgs := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	for _, sub := range []string{
		`trivy_ver="` + TrivyVersion + `"`,
		`releases/download/${trivy_ver}/trivy_${trivy_ver#v}_Linux-${trivy_arch}.tar.gz`,
		`apk add --no-cache buildah`,
	} {
		if !strings.Contains(scanArgs, sub) {
			t.Errorf("trivy/buildah install missing %q:\n%s", sub, scanArgs)
		}
	}
	for _, sub := range []string{"install.sh", "raw.githubusercontent.com"} {
		if strings.Contains(scanArgs, sub) {
			t.Errorf("runner must not fetch %q from a mutable ref:\n%s", sub, scanArgs)
		}
	}
}

// --- Target subsets and scan-flag overrides --------------------------------

// TestRunScanJob_SubsetImagesRendersOnlyNamedTarget asserts a per-call
// image subset renders exactly that target's build + assemble + scan
// lines and none of the others' (the ScanGate.Images contract).
func TestRunScanJob_SubsetImagesRendersOnlyNamedTarget(t *testing.T) {
	targets, err := resolveScanTargets([]string{"foreman-agent"})
	if err != nil {
		t.Fatalf("resolveScanTargets: %v", err)
	}
	job := scanRenderJob(t, func(in *scanRendererInput) { in.Targets = targets })

	initArgs := strings.Join(job.Spec.Template.Spec.InitContainers[0].Args, "\n")
	scanArgs := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")

	if !strings.Contains(initArgs, `-o "/work/foreman-agent/linux/amd64/foreman-agent"`) {
		t.Errorf("missing foreman-agent build line:\n%s", initArgs)
	}
	if !strings.Contains(scanArgs, `--input "/work/foreman-agent.tar"`) {
		t.Errorf("missing foreman-agent scan line:\n%s", scanArgs)
	}
	for _, other := range []string{"controller", "foreman-operator", "router-proxy"} {
		if strings.Contains(initArgs, "-o \"/work/"+other) {
			t.Errorf("subset leaked a build line for %q:\n%s", other, initArgs)
		}
		if strings.Contains(scanArgs, `/work/`+other) {
			t.Errorf("subset leaked an assemble/scan line for %q:\n%s", other, scanArgs)
		}
	}
}

// TestRunScanJob_SeverityAndIgnoreUnfixedOverrides asserts a custom
// severity list reaches trivy verbatim (uppercased, comma-joined) and
// ignoreUnfixed=false drops the flag while the gate-critical
// --exit-code 1 stays.
func TestRunScanJob_SeverityAndIgnoreUnfixedOverrides(t *testing.T) {
	if got := resolveScanSeverity([]string{"low", "medium"}); got != "LOW,MEDIUM" {
		t.Fatalf("resolveScanSeverity: want LOW,MEDIUM got %q", got)
	}
	job := scanRenderJob(t, func(in *scanRendererInput) {
		in.Severity = resolveScanSeverity([]string{"low", "medium"})
		in.IgnoreUnfixed = false
	})
	args := strings.Join(job.Spec.Template.Spec.Containers[0].Args, "\n")
	if !strings.Contains(args, "--severity LOW,MEDIUM") {
		t.Errorf("custom severity missing verbatim:\n%s", args)
	}
	if strings.Contains(args, "--ignore-unfixed") {
		t.Errorf("ignoreUnfixed=false must omit the flag:\n%s", args)
	}
	if !strings.Contains(args, "--exit-code 1") {
		t.Errorf("--exit-code 1 must survive the overrides:\n%s", args)
	}
}

// TestRunScanJob_CacheVolumeConditional pins #1538's rule on the scan
// template: an empty PVCName renders no cache volume and no /cache mount
// (never a defaulted claim the chart may not have created); a set
// PVCName mounts exactly that claim in BOTH containers.
func TestRunScanJob_CacheVolumeConditional(t *testing.T) {
	t.Run("empty PVCName", func(t *testing.T) {
		job := scanRenderJob(t, nil)
		if n := len(job.Spec.Template.Spec.Volumes); n != 1 {
			t.Fatalf("want only the work emptyDir, got %d: %+v", n, job.Spec.Template.Spec.Volumes)
		}
		if job.Spec.Template.Spec.Volumes[0].EmptyDir == nil {
			t.Errorf("the shared work volume must be an emptyDir: %+v", job.Spec.Template.Spec.Volumes[0])
		}
		for _, cs := range [][]corev1.Container{
			job.Spec.Template.Spec.InitContainers,
			job.Spec.Template.Spec.Containers,
		} {
			for _, ctr := range cs {
				for _, m := range ctr.VolumeMounts {
					if m.MountPath == "/cache" {
						t.Errorf("empty PVCName must render no /cache volumeMount, got %+v", m)
					}
				}
			}
		}
	})

	t.Run("set PVCName", func(t *testing.T) {
		job := scanRenderJob(t, func(in *scanRendererInput) { in.PVCName = "foreman-scan-cache" })
		var claim string
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.PersistentVolumeClaim != nil {
				claim = v.PersistentVolumeClaim.ClaimName
			}
		}
		if claim != "foreman-scan-cache" {
			t.Fatalf("want the given claim mounted, got %q (%+v)", claim, job.Spec.Template.Spec.Volumes)
		}
	})
}

// --- Defaults + naming ------------------------------------------------------

func TestApplyScanConfigDefaults_FillsEveryField(t *testing.T) {
	c := applyScanConfigDefaults(RunScanJobToolConfig{})
	if c.Namespace == "" || c.BuilderImage == "" || c.RunnerImage == "" || c.CloneURLBase == "" {
		t.Errorf("string defaults missing: %#v", c)
	}
	// PVCName is deliberately NOT defaulted (#1538's rule from the gate).
	if c.PVCName != "" {
		t.Errorf("PVCName must not be defaulted, got %q", c.PVCName)
	}
	if c.ActiveDeadlineSeconds != 3600 {
		t.Errorf("ActiveDeadlineSeconds default: want 3600 got %d", c.ActiveDeadlineSeconds)
	}
	if c.TTLSecondsAfterFinished == 0 {
		t.Errorf("deadline defaults missing: %#v", c)
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

	// PollTimeout defaults to 2 * ActiveDeadlineSeconds so the Job's own
	// deadline always fires before ours.
	want := 2 * time.Duration(c.ActiveDeadlineSeconds) * time.Second
	if c.PollTimeout != want {
		t.Errorf("PollTimeout default: want %s got %s", want, c.PollTimeout)
	}
}

func TestScanJobNamePreservesUniquenessSuffixWhenTruncated(t *testing.T) {
	// gateJobName's #768 lesson applied to the scan gate: a retry submits
	// a second scan Job for the same task while the prior attempt's Job
	// still exists. The unix-ms disambiguator must survive truncation to
	// the 63-char k8s object-name limit; losing it would collide the
	// retry's Create with the prior Job.
	long := "validate-1110-envtestloop-validate-1110-envtestloop"
	n1 := scanJobName(long, 1784157000000)
	n2 := scanJobName(long, 1784157000001)

	if len(n1) > 63 {
		t.Fatalf("name exceeds the 63-char k8s limit: len=%d %q", len(n1), n1)
	}
	if !strings.HasPrefix(n1, "foreman-scan-") {
		t.Fatalf("lost the foreman-scan- prefix: %q", n1)
	}
	if !strings.HasSuffix(n1, "-1784157000000") {
		t.Fatalf("uniqueness suffix was truncated away: %q", n1)
	}
	if n1 == n2 {
		t.Fatalf("two submissions of the same long task name collide: %q", n1)
	}
}
