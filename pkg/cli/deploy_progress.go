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
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/defilantech/llmkube/pkg/downloadprogress"
)

// downloaderContainer is the init container that fetches a remote model into
// the serving pod's model cache.
const downloaderContainer = "model-downloader"

var (
	// waitPollInterval is how often deploy --wait reads the Model and the
	// InferenceService.
	waitPollInterval = 2 * time.Second
	// downloadProgressInterval is the least time between two reads of the
	// downloader's log, and so between two progress lines. The downloader
	// itself reports every 10 seconds.
	downloadProgressInterval = 5 * time.Second
)

const (
	// downloaderLogTailLines reaches the latest report even when a concurrent
	// multi-file pool interleaves its other notices with the reports.
	downloaderLogTailLines = 20
	// downloaderLogLimitBytes bounds one read whatever the lines hold.
	downloaderLogLimitBytes = 64 << 10
	// downloaderLogTimeout keeps a slow log read from holding up the status line.
	downloaderLogTimeout = 5 * time.Second
	// downloaderPodsPerPoll bounds the log reads per poll. Replicas sharing one
	// cache wait on its lock while one of them downloads, and print no report.
	downloaderPodsPerPoll = 3
)

// podLogTail returns the last lines a container in a pod logged.
type podLogTail func(ctx context.Context, namespace, pod, container string) (string, error)

// clientsetLogTail reads pod logs through the API server's pods/log subresource.
func clientsetLogTail(cs kubernetes.Interface) podLogTail {
	return func(ctx context.Context, namespace, pod, container string) (string, error) {
		tail, limit := int64(downloaderLogTailLines), int64(downloaderLogLimitBytes)
		raw, err := cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
			Container:  container,
			TailLines:  &tail,
			LimitBytes: &limit,
		}).DoRaw(ctx)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
}

// downloadReport is what the watcher last saw of the model download.
type downloadReport struct {
	// running is set when a serving pod's model-downloader was running.
	running bool
	// finished is set when a model-downloader had exited cleanly and none was
	// running.
	finished bool
	// progress is the latest report read from a running downloader, if any.
	progress *downloadprogress.Progress
}

// downloadWatcher follows the model-downloader init container of an
// InferenceService's pods while deploy waits. The controller marks a Model with
// a remote source Ready before a byte is fetched, and the serving pod downloads
// it, so the pod's log is the only place the download shows (#2415).
type downloadWatcher struct {
	pods      client.Reader
	logs      podLogTail
	name      string
	namespace string
	interval  time.Duration

	lastPoll time.Time
	printed  string
	last     downloadReport
}

// newDownloadWatcher returns a watcher for the InferenceService name. A nil
// logs reader disables it.
func newDownloadWatcher(pods client.Reader, logs podLogTail, name, namespace string) *downloadWatcher {
	return &downloadWatcher{
		pods:      pods,
		logs:      logs,
		name:      name,
		namespace: namespace,
		interval:  downloadProgressInterval,
	}
}

// poll returns a line to print when the download's progress changed since the
// last line it returned, and "" otherwise. It reads the cluster at most once per
// interval. It treats a failed read (no RBAC for pods or pods/log, a pod that
// went away) as nothing new: the status line deploy already prints carries on.
func (w *downloadWatcher) poll(ctx context.Context, now time.Time) string {
	if w.logs == nil {
		return ""
	}
	if !w.lastPoll.IsZero() && now.Sub(w.lastPoll) < w.interval {
		return ""
	}
	w.lastPoll = now

	var pods corev1.PodList
	if err := w.pods.List(ctx, &pods, client.InNamespace(w.namespace), client.MatchingLabels{
		"app":                           w.name,
		"inference.llmkube.dev/service": w.name,
	}); err != nil {
		return ""
	}
	rep := w.observe(ctx, pods.Items)
	if rep.running && rep.progress == nil {
		// A read that failed this time does not undo the last report.
		rep.progress = w.last.progress
	}
	w.last = rep
	if rep.progress == nil {
		return ""
	}
	line := formatDownloadProgress(*rep.progress)
	if line == w.printed {
		return ""
	}
	w.printed = line
	return line
}

// report returns what the latest poll saw.
func (w *downloadWatcher) report() downloadReport {
	return w.last
}

// observe reads the downloader state of each pod, and the log of running
// downloaders until one carries a report. Pods are taken in name order so the
// same pod is read each poll.
func (w *downloadWatcher) observe(ctx context.Context, pods []corev1.Pod) downloadReport {
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	var rep downloadReport
	reads := 0
	for i := range pods {
		state, ok := downloaderState(&pods[i])
		if !ok {
			continue
		}
		switch {
		case state.Running != nil:
			rep.running = true
			if rep.progress != nil || reads >= downloaderPodsPerPoll {
				continue
			}
			reads++
			if p, ok := w.readProgress(ctx, pods[i].Name); ok {
				rep.progress = &p
			}
		case state.Terminated != nil && state.Terminated.ExitCode == 0:
			rep.finished = true
		}
	}
	if rep.running {
		rep.finished = false
	}
	return rep
}

func (w *downloadWatcher) readProgress(ctx context.Context, pod string) (downloadprogress.Progress, bool) {
	ctx, cancel := context.WithTimeout(ctx, downloaderLogTimeout)
	defer cancel()
	logs, err := w.logs(ctx, w.namespace, pod, downloaderContainer)
	if err != nil {
		return downloadprogress.Progress{}, false
	}
	return downloadprogress.Last(logs)
}

// downloaderState returns the state of the pod's model-downloader init
// container, and false when the pod has none.
func downloaderState(pod *corev1.Pod) (corev1.ContainerState, bool) {
	for _, s := range pod.Status.InitContainerStatuses {
		if s.Name == downloaderContainer {
			return s.State, true
		}
	}
	return corev1.ContainerState{}, false
}

// formatDownloadProgress renders a report for the terminal.
func formatDownloadProgress(p downloadprogress.Progress) string {
	what := "model"
	if p.File != "" {
		// The file name comes from the model's repository listing; keep its
		// control characters off the user's terminal.
		what = strings.Map(func(r rune) rune {
			if unicode.IsPrint(r) {
				return r
			}
			return '?'
		}, p.File)
	}
	if p.HasTotal {
		return fmt.Sprintf("Downloading %s: %d MiB of %d MiB (%d%%)", what, p.DownloadedMiB, p.TotalMiB, p.Percent)
	}
	return fmt.Sprintf("Downloading %s: %d MiB so far", what, p.DownloadedMiB)
}

// deployTimeoutHelp explains a deploy --wait timeout: whether the model was
// still downloading, that the deployment carries on without the CLI, and how to
// follow it. Rerunning deploy cannot resume the wait, since the Model and the
// InferenceService already exist, so the longer --timeout is for next time.
func deployTimeoutHelp(name, namespace string, timeout time.Duration, rep downloadReport) string {
	longer := 2 * timeout
	selector := fmt.Sprintf("app=%s,inference.llmkube.dev/service=%s", name, name)

	var b strings.Builder
	fmt.Fprintf(&b, "\n⏱️  Stopped waiting after %s; %s is not ready yet.\n", timeout, name)
	switch {
	case rep.running && rep.progress != nil:
		fmt.Fprintf(&b, "The model download was still running (last report: %s).\n", formatDownloadProgress(*rep.progress))
	case rep.running:
		fmt.Fprintf(&b, "The model download was still running.\n")
	case rep.finished:
		fmt.Fprintf(&b, "The model download had finished; the inference server was still starting.\n")
	}
	fmt.Fprintf(&b, "The deployment keeps going in the cluster; only this command stopped waiting.\n\n")
	fmt.Fprintf(&b, "To keep watching:\n")
	fmt.Fprintf(&b, "  llmkube status %s -n %s\n", name, namespace)
	if rep.running {
		fmt.Fprintf(&b, "  kubectl logs -n %s -f -c %s -l %s\n", namespace, downloaderContainer, selector)
	}
	fmt.Fprintf(&b, "  kubectl wait --for=condition=available --timeout=%s inferenceservice/%s -n %s\n\n",
		longer, name, namespace)
	fmt.Fprintf(&b, "A first deploy downloads the model before it can serve. For a large model or a slow\n")
	fmt.Fprintf(&b, "link, deploy with a longer --timeout (for example --timeout %s), or with --wait=false\n", longer)
	fmt.Fprintf(&b, "and follow it with the commands above.\n")
	return b.String()
}
