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

package cli

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	inferencev1alpha1 "github.com/defilantech/llmkube/api/v1alpha1"
	"github.com/defilantech/llmkube/pkg/downloadprogress"
)

const progressTestNS = "ns"

func progressTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := inferencev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

var (
	downloaderRunning  = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	downloaderWaiting  = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}
	downloaderFinished = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
)

// servingPod is a pod the InferenceService controller creates for service, with
// the labels its Deployment selects on and the model-downloader in state.
func servingPod(name, service string, state corev1.ContainerState) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: progressTestNS,
			Labels: map[string]string{"app": service, "inference.llmkube.dev/service": service},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "model-cache-prep", State: downloaderFinished},
				{Name: "model-downloader", State: state},
			},
		},
	}
}

// stubLogs serves canned log tails per pod and records every read.
type stubLogs struct {
	logs  map[string]string
	err   error
	reads []string
}

func (s *stubLogs) tail(_ context.Context, namespace, pod, container string) (string, error) {
	s.reads = append(s.reads, namespace+"/"+pod+"/"+container)
	if s.err != nil {
		return "", s.err
	}
	return s.logs[pod], nil
}

func TestFormatDownloadProgress(t *testing.T) {
	tests := []struct {
		name string
		p    downloadprogress.Progress
		want string
	}{
		{"known total", downloadprogress.Progress{DownloadedMiB: 1104, TotalMiB: 2765, Percent: 39, HasTotal: true},
			"Downloading model: 1104 MiB of 2765 MiB (39%)"},
		{"no total", downloadprogress.Progress{DownloadedMiB: 1104},
			"Downloading model: 1104 MiB so far"},
		{"one file of several",
			downloadprogress.Progress{DownloadedMiB: 2, TotalMiB: 4, Percent: 50, HasTotal: true, File: "a.bin"},
			"Downloading a.bin: 2 MiB of 4 MiB (50%)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatDownloadProgress(tt.p); got != tt.want {
				t.Errorf("formatDownloadProgress() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The downloader logs every 10 seconds and deploy polls every 2. A line is
// printed only when the report changed, and the logs are read at most once per
// interval, so a slow download cannot flood the terminal or the API server.
func TestDownloadWatcherPrintsChangesAtMostOncePerInterval(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(progressTestScheme(t)).
		WithObjects(servingPod("svc-abc", "svc", downloaderRunning)).Build()
	logs := &stubLogs{logs: map[string]string{
		"svc-abc": "Downloading model...\nDownloaded 10 MiB of 100 MiB (10%)\n",
	}}
	w := newDownloadWatcher(c, logs.tail, "svc", progressTestNS)
	w.interval = 5 * time.Second
	t0 := time.Now()

	if got := w.poll(ctx, t0); got != "Downloading model: 10 MiB of 100 MiB (10%)" {
		t.Fatalf("first poll = %q", got)
	}
	if want := []string{"ns/svc-abc/model-downloader"}; !reflect.DeepEqual(logs.reads, want) {
		t.Fatalf("reads = %v, want %v", logs.reads, want)
	}

	logs.logs["svc-abc"] += "Downloaded 20 MiB of 100 MiB (20%)\n"
	if got := w.poll(ctx, t0.Add(time.Second)); got != "" || len(logs.reads) != 1 {
		t.Fatalf("poll inside the interval printed %q after %d reads; want nothing and no new read", got, len(logs.reads))
	}
	if got := w.poll(ctx, t0.Add(5*time.Second)); got != "Downloading model: 20 MiB of 100 MiB (20%)" {
		t.Fatalf("poll after the interval = %q", got)
	}
	if got := w.poll(ctx, t0.Add(10*time.Second)); got != "" || len(logs.reads) != 3 {
		t.Fatalf("unchanged report printed %q after %d reads; want nothing after 3 reads", got, len(logs.reads))
	}
	rep := w.report()
	if !rep.running || rep.progress == nil || rep.progress.DownloadedMiB != 20 {
		t.Errorf("report = %+v, want a running download at 20 MiB", rep)
	}
}

func TestDownloadWatcherReadsOnlyRunningDownloadersOfThisService(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(progressTestScheme(t)).WithObjects(
		servingPod("svc-waiting", "svc", downloaderWaiting),
		servingPod("other-abc", "other", downloaderRunning),
	).Build()
	logs := &stubLogs{logs: map[string]string{
		"svc-waiting": "Downloaded 1 MiB of 2 MiB (50%)\n",
		"other-abc":   "Downloaded 1 MiB of 2 MiB (50%)\n",
	}}
	w := newDownloadWatcher(c, logs.tail, "svc", progressTestNS)
	if got := w.poll(context.Background(), time.Now()); got != "" {
		t.Errorf("poll = %q, want nothing", got)
	}
	if len(logs.reads) != 0 {
		t.Errorf("read %v; want no reads of a waiting downloader or another service's pod", logs.reads)
	}
	if rep := w.report(); rep.running || rep.finished {
		t.Errorf("report = %+v, want neither running nor finished", rep)
	}
}

// With several replicas on a shared cache, one pod downloads while the others
// wait on the cache lock and print no report.
func TestDownloadWatcherReadsTheNextPodWhenOneHasNoReport(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(progressTestScheme(t)).WithObjects(
		servingPod("svc-a", "svc", downloaderRunning),
		servingPod("svc-b", "svc", downloaderRunning),
	).Build()
	logs := &stubLogs{logs: map[string]string{
		"svc-a": "Waiting for the cache lock\n",
		"svc-b": "Downloading model...\nDownloaded 30 MiB of 60 MiB (50%)\n",
	}}
	w := newDownloadWatcher(c, logs.tail, "svc", progressTestNS)
	if got := w.poll(context.Background(), time.Now()); got != "Downloading model: 30 MiB of 60 MiB (50%)" {
		t.Errorf("poll = %q, want svc-b's report", got)
	}
}

func TestDownloadWatcherSeesAFinishedDownload(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(progressTestScheme(t)).
		WithObjects(servingPod("svc-abc", "svc", downloaderFinished)).Build()
	logs := &stubLogs{}
	w := newDownloadWatcher(c, logs.tail, "svc", progressTestNS)
	if got := w.poll(context.Background(), time.Now()); got != "" {
		t.Errorf("poll = %q, want nothing", got)
	}
	if rep := w.report(); rep.running || !rep.finished {
		t.Errorf("report = %+v, want finished", rep)
	}
}

// Reading logs needs pods/log, which a namespace-scoped user may lack, and the
// pod can be replaced between the List and the read. Neither is deploy's
// failure: the watcher stays quiet and the status line carries on.
func TestDownloadWatcherIsQuietWhenItCannotRead(t *testing.T) {
	ctx := context.Background()

	t.Run("log read fails", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(progressTestScheme(t)).
			WithObjects(servingPod("svc-abc", "svc", downloaderRunning)).Build()
		logs := &stubLogs{err: errors.New(`pods "svc-abc" is forbidden: cannot get resource "pods/log"`)}
		w := newDownloadWatcher(c, logs.tail, "svc", progressTestNS)
		if got := w.poll(ctx, time.Now()); got != "" {
			t.Errorf("poll = %q, want nothing", got)
		}
		if rep := w.report(); !rep.running || rep.progress != nil {
			t.Errorf("report = %+v, want running with no progress", rep)
		}
	})

	t.Run("pod list fails", func(t *testing.T) {
		// A scheme without core types makes every pod List fail.
		c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
		logs := &stubLogs{}
		w := newDownloadWatcher(c, logs.tail, "svc", progressTestNS)
		if got := w.poll(ctx, time.Now()); got != "" || len(logs.reads) != 0 {
			t.Errorf("poll = %q after reads %v, want nothing", got, logs.reads)
		}
	})

	t.Run("no log reader", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(progressTestScheme(t)).
			WithObjects(servingPod("svc-abc", "svc", downloaderRunning)).Build()
		w := newDownloadWatcher(c, nil, "svc", progressTestNS)
		if got := w.poll(ctx, time.Now()); got != "" {
			t.Errorf("poll = %q, want nothing", got)
		}
	})
}

func TestDeployTimeoutHelp(t *testing.T) {
	progress := &downloadprogress.Progress{DownloadedMiB: 1104, TotalMiB: 2765, Percent: 39, HasTotal: true}
	tests := []struct {
		name    string
		report  downloadReport
		want    []string
		notWant []string
	}{
		{
			name:   "download still running",
			report: downloadReport{running: true, progress: progress},
			want: []string{
				"The model download was still running (last report: Downloading model: 1104 MiB of 2765 MiB (39%))",
				"kubectl logs -n ns -f -c model-downloader -l app=svc,inference.llmkube.dev/service=svc",
			},
		},
		{
			name:    "download running with no report yet",
			report:  downloadReport{running: true},
			want:    []string{"The model download was still running."},
			notWant: []string{"last report"},
		},
		{
			name:    "download finished",
			report:  downloadReport{finished: true},
			want:    []string{"The model download had finished; the inference server was still starting."},
			notWant: []string{"kubectl logs", "still running"},
		},
		{
			name:    "download not observed",
			report:  downloadReport{},
			notWant: []string{"kubectl logs", "still running", "had finished"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deployTimeoutHelp("svc", progressTestNS, 10*time.Minute, tt.report)
			// Every timeout says the deployment carries on and how to follow
			// it. deploy cannot be rerun to keep waiting (the Model exists), so
			// the longer --timeout is advice for the next deploy.
			for _, s := range append([]string{
				"The deployment keeps going in the cluster",
				"llmkube status svc -n ns",
				"kubectl wait --for=condition=available --timeout=20m0s inferenceservice/svc -n ns",
				"--timeout 20m0s",
			}, tt.want...) {
				if !strings.Contains(got, s) {
					t.Errorf("help is missing %q:\n%s", s, got)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(got, s) {
					t.Errorf("help should not contain %q:\n%s", s, got)
				}
			}
		})
	}
}

// waitTestObjects is a Model whose remote source the controller has resolved
// (Ready) and an InferenceService still Creating while its pod downloads.
func waitTestObjects(accelerator string) []client.Object {
	return []client.Object{
		&inferencev1alpha1.Model{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: progressTestNS},
			Spec: inferencev1alpha1.ModelSpec{
				Source:   "https://huggingface.co/org/repo/resolve/main/model.gguf",
				Hardware: &inferencev1alpha1.HardwareSpec{Accelerator: accelerator},
			},
			Status: inferencev1alpha1.ModelStatus{Phase: "Ready"},
		},
		&inferencev1alpha1.InferenceService{
			ObjectMeta: metav1.ObjectMeta{Name: "svc", Namespace: progressTestNS},
			Spec:       inferencev1alpha1.InferenceServiceSpec{ModelRef: "svc"},
			Status:     inferencev1alpha1.InferenceServiceStatus{Phase: "Creating", DesiredReplicas: 1},
		},
		servingPod("svc-abc", "svc", downloaderRunning),
	}
}

func shortWaitIntervals(t *testing.T) {
	t.Helper()
	oldPoll, oldProgress := waitPollInterval, downloadProgressInterval
	waitPollInterval, downloadProgressInterval = 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { waitPollInterval, downloadProgressInterval = oldPoll, oldProgress })
}

func TestWaitForReadyShowsDownloadProgressThenExplainsTheTimeout(t *testing.T) {
	shortWaitIntervals(t)
	c := fake.NewClientBuilder().WithScheme(progressTestScheme(t)).WithObjects(waitTestObjects("cuda")...).Build()
	logs := &stubLogs{logs: map[string]string{"svc-abc": "Downloaded 1104 MiB of 2765 MiB (39%)\n"}}

	var err error
	out := captureStdout(t, func() {
		err = waitForReady(context.Background(), c, logs.tail, "svc", progressTestNS, 300*time.Millisecond)
	})

	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	// The timeout help quotes the last report too, so count progress lines only.
	if n := strings.Count(out, "⏳ Downloading model: 1104 MiB of 2765 MiB (39%)\n"); n != 1 {
		t.Errorf("progress line printed %d times, want once for an unchanged report:\n%s", n, out)
	}
	if len(logs.reads) < 2 {
		t.Errorf("logs read %d times in 300ms at a 40ms interval; the watcher stopped polling", len(logs.reads))
	}
	for _, s := range []string{
		"Model: Ready, Service: Creating (0/1 replicas)",
		"The model download was still running",
		"The deployment keeps going in the cluster",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("output is missing %q:\n%s", s, out)
		}
	}
}

// A Metal-served model downloads on the Mac, and its service has no pod with a
// model-downloader: deploy must not go looking for one.
func TestWaitForReadySkipsDownloadProgressForMetal(t *testing.T) {
	shortWaitIntervals(t)
	c := fake.NewClientBuilder().WithScheme(progressTestScheme(t)).WithObjects(waitTestObjects("metal")...).Build()
	logs := &stubLogs{logs: map[string]string{"svc-abc": "Downloaded 1104 MiB of 2765 MiB (39%)\n"}}

	var err error
	out := captureStdout(t, func() {
		err = waitForReady(context.Background(), c, logs.tail, "svc", progressTestNS, 150*time.Millisecond)
	})

	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if len(logs.reads) != 0 {
		t.Errorf("read logs %v for a Metal-served model", logs.reads)
	}
	if strings.Contains(out, "Downloading model") || strings.Contains(out, "still running") {
		t.Errorf("Metal output claims a pod download:\n%s", out)
	}
	if !strings.Contains(out, "The deployment keeps going in the cluster") {
		t.Errorf("timeout help missing:\n%s", out)
	}
}
