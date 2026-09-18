package main

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	foremanv1alpha1 "github.com/defilantech/llmkube/api/foreman/v1alpha1"
	foremantools "github.com/defilantech/llmkube/pkg/foreman/agent/tools"
)

func TestMapScanVerdict(t *testing.T) {
	cases := []struct {
		verdict           string
		wantPass, wantRan bool
	}{
		{"SCAN-PASS", true, true},
		{"SCAN-FAIL", false, true},
		{"SCAN-ERROR", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		pass, ran := mapScanVerdict(c.verdict)
		if pass != c.wantPass || ran != c.wantRan {
			t.Errorf("mapScanVerdict(%q) = (%v,%v) want (%v,%v)", c.verdict, pass, ran, c.wantPass, c.wantRan)
		}
	}
}

// scanRunnerHarness builds a scanJobRunnerImpl over a fake client whose Job
// never reaches a terminal phase, so Run submits the Job and gives up at the
// (millisecond) poll timeout. Tests assert on the created Job -- the
// envtest_runner_test.go idiom applied to the scan gate.
func scanRunnerHarness(t *testing.T, pvcName string) (*scanJobRunnerImpl, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("add batchv1 scheme: %v", err)
	}
	kc := fake.NewClientBuilder().WithScheme(s).Build()
	impl := &scanJobRunnerImpl{
		tool: &foremantools.RunScanJobTool{
			Client: kc,
			Cfg: foremantools.RunScanJobToolConfig{
				Namespace:    "foreman-system",
				PVCName:      pvcName,
				PollInterval: time.Millisecond,
				PollTimeout:  5 * time.Millisecond,
				NameFn:       func(string) string { return "foreman-scan-pinned" },
			},
		},
	}
	return impl, kc
}

func createdScanJob(t *testing.T, kc client.Client) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	key := types.NamespacedName{Namespace: "foreman-system", Name: "foreman-scan-pinned"}
	if err := kc.Get(context.Background(), key, &job); err != nil {
		t.Fatalf("scan Job %s was not created: %v", key, err)
	}
	return &job
}

func containerWithName(pod *corev1.PodSpec, name string) *corev1.Container {
	for i := range pod.InitContainers {
		if pod.InitContainers[i].Name == name {
			return &pod.InitContainers[i]
		}
	}
	for i := range pod.Containers {
		if pod.Containers[i].Name == name {
			return &pod.Containers[i]
		}
	}
	return nil
}

// TestScanJobRunnerStampsTaskLabels is the #893 parity check for the scan
// gate: the clean-room scan Job must carry the originating AgenticTask
// identity in the foreman.llmkube.dev/task-{namespace,name} labels (Job and
// pod template alike), not the "unknown"/"task" defaults the renderer falls
// back to when no taskRef is threaded through.
func TestScanJobRunnerStampsTaskLabels(t *testing.T) {
	runner, kc := scanRunnerHarness(t, "foreman-gate-cache")
	emptyGate := foremanv1alpha1.ScanGate{}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, _ = runner.Run(
		ctx, "foreman-system", "fix-issue-893",
		"defilantech/LLMKube", "foreman/issue-893",
		"https://github.com/Defilan/LLMKube.git",
		"https://github.com/defilantech/LLMKube.git",
		emptyGate.Resolve(),
	)

	job := createdScanJob(t, kc)
	if got := job.Labels["foreman.llmkube.dev/task-name"]; got != "fix-issue-893" {
		t.Errorf("scan Job task-name label = %q, want %q", got, "fix-issue-893")
	}
	if got := job.Labels["foreman.llmkube.dev/task-namespace"]; got != "foreman-system" {
		t.Errorf("scan Job task-namespace label = %q, want %q", got, "foreman-system")
	}
	if got := job.Spec.Template.Labels["foreman.llmkube.dev/task-name"]; got != "fix-issue-893" {
		t.Errorf("scan Job pod template task-name label = %q, want %q", got, "fix-issue-893")
	}
}

// TestScanJobRunnerForwardsResolvedScan proves the seam's promise: the
// executor resolves the task's ScanGate once and the concrete config reaches
// the rendered Job verbatim -- the runner neither re-resolves nor re-defaults.
// Custom severity lands in the Trivy invocation, ignoreUnfixed=false drops
// --ignore-unfixed, an image subset scans only those targets, and the
// builder/runner images override the template defaults.
func TestScanJobRunnerForwardsResolvedScan(t *testing.T) {
	cases := []struct {
		name string
		gate *foremanv1alpha1.ScanGate
		// assertions against the scan container script
		wantContains    []string
		wantAbsent      []string
		wantBuilderImg  string
		wantRunnerImg   string
		wantTrivyImages int
	}{
		{
			name: "defaults: all targets, CRITICAL,HIGH, ignore-unfixed on",
			gate: nil,
			wantContains: []string{
				"--severity CRITICAL,HIGH",
				"--ignore-unfixed",
			},
			wantBuilderImg:  "golang:1.26",
			wantRunnerImg:   "alpine:3.24",
			wantTrivyImages: 4,
		},
		{
			name: "overrides land verbatim: severity, ignoreUnfixed=false, subset, images",
			gate: &foremanv1alpha1.ScanGate{
				Images:        []string{"foreman-agent"},
				Severity:      []string{"medium", "CRITICAL"},
				IgnoreUnfixed: ptr(false),
				BuilderImage:  "mirror/golang:1.26.4",
				RunnerImage:   "alpine:edge",
			},
			wantContains: []string{
				"--severity MEDIUM,CRITICAL",
				"=== build foreman-agent ",
				"=== trivy foreman-agent ===",
			},
			wantAbsent: []string{
				"--ignore-unfixed",
				"=== build controller ",
				"=== trivy controller ===",
				"=== trivy router-proxy ===",
			},
			wantBuilderImg:  "mirror/golang:1.26.4",
			wantRunnerImg:   "alpine:edge",
			wantTrivyImages: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner, kc := scanRunnerHarness(t, "foreman-gate-cache")

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _, _ = runner.Run(
				ctx, "foreman-system", "scan-cfg-task",
				"defilantech/LLMKube", "foreman/scan-cfg",
				"https://github.com/Defilan/LLMKube.git",
				"https://github.com/defilantech/LLMKube.git",
				tc.gate.Resolve(),
			)

			job := createdScanJob(t, kc)
			scan := containerWithName(&job.Spec.Template.Spec, "scan")
			if scan == nil {
				t.Fatalf("scan container missing from the rendered Job")
			}
			if scan.Image != tc.wantRunnerImg {
				t.Errorf("scan container image = %q, want %q", scan.Image, tc.wantRunnerImg)
			}
			build := containerWithName(&job.Spec.Template.Spec, "scan-build")
			if build == nil {
				t.Fatalf("scan-build initContainer missing from the rendered Job")
			}
			if build.Image != tc.wantBuilderImg {
				t.Errorf("builder initContainer image = %q, want %q", build.Image, tc.wantBuilderImg)
			}

			scripts := build.Args[0] + "\n" + scan.Args[0]
			for _, want := range tc.wantContains {
				if !strings.Contains(scripts, want) {
					t.Errorf("rendered script missing %q; got:\n%s", want, scripts)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(scripts, absent) {
					t.Errorf("rendered script contains %q; want it absent", absent)
				}
			}
			if n := strings.Count(scan.Args[0], "trivy image --input"); n != tc.wantTrivyImages {
				t.Errorf("trivy invocations = %d, want %d", n, tc.wantTrivyImages)
			}

			// The scan shares the gate's cache claim end to end: the flag
			// value handed to makeScanJobRunner surfaces as the /cache volume.
			var claim string
			for _, v := range job.Spec.Template.Spec.Volumes {
				if v.Name == "cache" && v.PersistentVolumeClaim != nil {
					claim = v.PersistentVolumeClaim.ClaimName
				}
			}
			if claim != "foreman-gate-cache" {
				t.Errorf("cache volume claim = %q, want the gate-cache PVC name", claim)
			}
		})
	}
}

func ptr(b bool) *bool { return &b }
